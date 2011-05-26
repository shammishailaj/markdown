package main

import (
	"bytes"
	"fmt"
	"html"
	"sort"
	"unicode"
)

const (
	MKDA_NOT_AUTOLINK = iota
	MKDA_NORMAL
	MKDA_EMAIL
)

const (
	MKDEXT_NO_INTRA_EMPHASIS = 1 << iota
	MKDEXT_TABLES
	MKDEXT_FENCED_CODE
	MKDEXT_AUTOLINK
	MKDEXT_STRIKETHROUGH
	MKDEXT_LAX_HTML_BLOCKS
	MKDEXT_SPACE_HEADERS
)

const (
	_ = iota
	MKD_LIST_ORDERED
	MKD_LI_BLOCK // <li> containing block data
	MKD_LI_END   = 8
)

const (
	MKD_TABLE_ALIGN_L = 1 << iota
	MKD_TABLE_ALIGN_R
	MKD_TABLE_ALIGN_CENTER = (MKD_TABLE_ALIGN_L | MKD_TABLE_ALIGN_R)
)

var block_tags = map[string]int{
	"p":          1, // 0
	"dl":         2,
	"h1":         2,
	"h2":         2,
	"h3":         2,
	"h4":         2,
	"h5":         2,
	"h6":         2,
	"ol":         2,
	"ul":         2,
	"del":        3, // 10
	"div":        3,
	"ins":        3, // 12
	"pre":        3,
	"form":       4,
	"math":       4,
	"table":      5,
	"iframe":     6,
	"script":     6,
	"fieldset":   8,
	"noscript":   8,
	"blockquote": 10,
}

// functions for rendering parsed data
type mkd_renderer struct {
	// block-level callbacks---nil skips the block
	blockcode  func(ob *bytes.Buffer, text []byte, lang string, opaque interface{})
	blockquote func(ob *bytes.Buffer, text []byte, opaque interface{})
	blockhtml  func(ob *bytes.Buffer, text []byte, opaque interface{})
	header     func(ob *bytes.Buffer, text []byte, level int, opaque interface{})
	hrule      func(ob *bytes.Buffer, opaque interface{})
	list       func(ob *bytes.Buffer, text []byte, flags int, opaque interface{})
	listitem   func(ob *bytes.Buffer, text []byte, flags int, opaque interface{})
	paragraph  func(ob *bytes.Buffer, text []byte, opaque interface{})
	table      func(ob *bytes.Buffer, header []byte, body []byte, opaque interface{})
	table_row  func(ob *bytes.Buffer, text []byte, opaque interface{})
	table_cell func(ob *bytes.Buffer, text []byte, flags int, opaque interface{})

	// span-level callbacks---nil or return 0 prints the span verbatim
	autolink        func(ob *bytes.Buffer, link []byte, kind int, opaque interface{}) int
	codespan        func(ob *bytes.Buffer, text []byte, opaque interface{}) int
	double_emphasis func(ob *bytes.Buffer, text []byte, opaque interface{}) int
	emphasis        func(ob *bytes.Buffer, text []byte, opaque interface{}) int
	image           func(ob *bytes.Buffer, link []byte, title []byte, alt []byte, opaque interface{}) int
	linebreak       func(ob *bytes.Buffer, opaque interface{}) int
	link            func(ob *bytes.Buffer, link []byte, title []byte, content []byte, opaque interface{}) int
	raw_html_tag    func(ob *bytes.Buffer, tag []byte, opaque interface{}) int
	triple_emphasis func(ob *bytes.Buffer, text []byte, opaque interface{}) int
	strikethrough   func(ob *bytes.Buffer, text []byte, opaque interface{}) int

	// low-level callbacks---nil copies input directly into the output
	entity      func(ob *bytes.Buffer, entity []byte, opaque interface{})
	normal_text func(ob *bytes.Buffer, text []byte, opaque interface{})

	// header and footer
	doc_header func(ob *bytes.Buffer, opaque interface{})
	doc_footer func(ob *bytes.Buffer, opaque interface{})

	// user data---passed back to every callback
	opaque interface{}
}

type link_ref struct {
	id    []byte
	link  []byte
	title []byte
}

type link_ref_array []*link_ref

// implement the sorting interface
func (elt link_ref_array) Len() int {
	return len(elt)
}

func (elt link_ref_array) Less(i, j int) bool {
	a, b := elt[i].id, elt[j].id

	// adapted from bytes.Compare in stdlib
	m := len(a)
	if m > len(b) {
		m = len(b)
	}
	for i, ac := range a[0:m] {
		// do a case-insensitive comparison
		ai, bi := unicode.ToLower(int(ac)), unicode.ToLower(int(b[i]))
		switch {
		case ai > bi:
			return false
		case ai < bi:
			return true
		}
	}
	switch {
	case len(a) < len(b):
		return true
	case len(a) > len(b):
		return false
	}
	return false
}

func (elt link_ref_array) Swap(i, j int) {
	elt[i], elt[j] = elt[j], elt[i]
}

// returns whether or not a line is a reference
func is_ref(data []byte, beg int, last *int, rndr *render) bool {
	// up to 3 optional leading spaces
	if beg+3 > len(data) {
		return false
	}
	i := 0
	if data[beg] == ' ' {
		i++
		if data[beg+1] == ' ' {
			i++
			if data[beg+2] == ' ' {
				i++
				if data[beg+3] == ' ' {
					return false
				}
			}
		}
	}
	i += beg

	// id part: anything but a newline between brackets
	if data[i] != '[' {
		return false
	}
	i++
	id_offset := i
	for i < len(data) && data[i] != '\n' && data[i] != '\r' && data[i] != ']' {
		i++
	}
	if i >= len(data) || data[i] != ']' {
		return false
	}
	id_end := i

	// spacer: colon (space | tab)* newline? (space | tab)*
	i++
	if i >= len(data) || data[i] != ':' {
		return false
	}
	i++
	for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
		i++
	}
	if i < len(data) && (data[i] == '\n' || data[i] == '\r') {
		i++
		if i < len(data) && data[i] == '\r' && data[i-1] == '\n' {
			i++
		}
	}
	for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
		i++
	}
	if i >= len(data) {
		return false
	}

	// link: whitespace-free sequence, optionally between angle brackets
	if data[i] == '<' {
		i++
	}
	link_offset := i
	for i < len(data) && data[i] != ' ' && data[i] != '\t' && data[i] != '\n' && data[i] != '\r' {
		i++
	}
	var link_end int
	if data[i-1] == '>' {
		link_end = i - 1
	} else {
		link_end = i
	}

	// optional spacer: (space | tab)* (newline | '\'' | '"' | '(' )
	for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
		i++
	}
	if i < len(data) && data[i] != '\n' && data[i] != '\r' && data[i] != '\'' && data[i] != '"' && data[i] != '(' {
		return false
	}

	// compute end-of-line
	line_end := 0
	if i >= len(data) || data[i] == '\r' || data[i] == '\n' {
		line_end = i
	}
	if i+1 < len(data) && data[i] == '\n' && data[i+1] == '\r' {
		line_end = i + 1
	}

	// optional (space|tab)* spacer after a newline
	if line_end > 0 {
		i = line_end + 1
		for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
			i++
		}
	}

	// optional title: any non-newline sequence enclosed in '"() alone on its line
	title_offset, title_end := 0, 0
	if i+1 < len(data) && (data[i] == '\'' || data[i] == '"' || data[i] == '(') {
		i++
		title_offset = i

		// looking for EOL
		for i < len(data) && data[i] != '\n' && data[i] != '\r' {
			i++
		}
		if i+1 < len(data) && data[i] == '\n' && data[i+1] == '\r' {
			title_end = i + 1
		} else {
			title_end = i
		}

		// stepping back
		i--
		for i > title_offset && (data[i] == ' ' || data[i] == '\t') {
			i--
		}
		if i > title_offset && (data[i] == '\'' || data[i] == '"' || data[i] == ')') {
			line_end = title_end
			title_end = i
		}
	}
	if line_end == 0 { // garbage after the link
		return false
	}

	// a valid ref has been found; fill in return structures
	if last != nil {
		*last = line_end
	}
	if rndr == nil {
		return true
	}
	item := &link_ref{id: data[id_offset:id_end], link: data[link_offset:link_end], title: data[title_offset:title_end]}
	rndr.refs = append(rndr.refs, item)

	return true
}

type render struct {
	mk          *mkd_renderer
	refs        link_ref_array
	active_char [256]int
	ext_flags   uint32
	nesting     int
	max_nesting int
}

const (
	MD_CHAR_NONE = iota
	MD_CHAR_EMPHASIS
	MD_CHAR_CODESPAN
	MD_CHAR_LINEBREAK
	MD_CHAR_LINK
	MD_CHAR_LANGLE
	MD_CHAR_ESCAPE
	MD_CHAR_ENTITITY
	MD_CHAR_AUTOLINK
)

// closures to render active chars, each:
//   returns the number of chars taken care of
//   data is the complete block being rendered
//   offset is the number of valid chars before the data
//
// Note: this is filled in in Markdown to prevent an initilization loop
var markdown_char_ptrs [9]func(ob *bytes.Buffer, rndr *render, data []byte, offset int) int

func parse_inline(ob *bytes.Buffer, rndr *render, data []byte) {
	if rndr.nesting >= rndr.max_nesting {
		return
	}
	rndr.nesting++

	i, end := 0, 0
	for i < len(data) {
		// copy inactive chars into the output
		for end < len(data) && rndr.active_char[data[end]] == 0 {
			end++
		}

		if rndr.mk.normal_text != nil {
			rndr.mk.normal_text(ob, data[i:], rndr.mk.opaque)
		} else {
			ob.Write(data[i:])
		}

		if end >= len(data) {
			break
		}
		i = end

		// call the trigger
		action := rndr.active_char[data[end]]
		end = markdown_char_ptrs[action](ob, rndr, data, i)

		if end == 0 { // no action from the callback
			end = i + 1
		} else {
			i += end
			end = i
		}
	}

	rndr.nesting--
}

// single and double emphasis parsing
func char_emphasis(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	data = data[offset:]
	c := data[0]
	ret := 0

	if len(data) > 2 && data[1] != c {
		// whitespace cannot follow an opening emphasis;
		// strikethrough only takes two characters '~~'
		if c == '~' || unicode.IsSpace(int(data[1])) {
			return 0
		}
		if ret = parse_emph1(ob, rndr, data[1:], c); ret == 0 {
			return 0
		}

		return ret + 1
	}

	if len(data) > 3 && data[1] == c && data[2] != c {
		if unicode.IsSpace(int(data[2])) {
			return 0
		}
		if ret = parse_emph2(ob, rndr, data[2:], c); ret == 0 {
			return 0
		}

		return ret + 2
	}

	if len(data) > 4 && data[1] == c && data[2] == c && data[3] != c {
		if c == '~' || unicode.IsSpace(int(data[3])) {
			return 0
		}
		if ret = parse_emph3(ob, rndr, data, 3, c); ret == 0 {
			return 0
		}

		return ret + 3
	}

	return 0
}

func char_codespan(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	data = data[offset:]

	nb := 0

	// counting the number of backticks in the delimiter
	for nb < len(data) && data[nb] == '`' {
		nb++
	}

	// finding the next delimiter
	i, end := 0, 0
	for end = nb; end < len(data) && i < nb; end++ {
		if data[end] == '`' {
			i++
		} else {
			i = 0
		}
	}

	if i < nb && end >= len(data) {
		return 0 // no matching delimiter
	}

	// trim outside whitespace
	f_begin := nb
	for f_begin < end && (data[f_begin] == ' ' || data[f_begin] == '\t') {
		f_begin++
	}

	f_end := end - nb
	for f_end > nb && (data[f_end-1] == ' ' || data[f_end-1] == '\t') {
		f_end--
	}

	// real code span
	if rndr.mk.codespan == nil {
		return 0
	}
	if f_begin < f_end {
		if rndr.mk.codespan(ob, data[f_end:f_end], rndr.mk.opaque) == 0 {
			end = 0
		}
	} else {
		if rndr.mk.codespan(ob, nil, rndr.mk.opaque) == 0 {
			end = 0
		}
	}

	return end

}

// '\n' preceded by two spaces
func char_linebreak(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	if offset < 2 || data[offset-1] != ' ' || data[offset-2] != ' ' {
		return 0
	}

	// remove trailing spaces from ob and render
	ob_bytes := ob.Bytes()
	end := len(ob_bytes)
	for end > 0 && ob_bytes[end-1] == ' ' {
		end--
	}
	ob.Truncate(end)

	if rndr.mk.linebreak == nil {
		return 0
	}
	if rndr.mk.linebreak(ob, rndr.mk.opaque) > 0 {
		return 1
	} else {
		return 0
	}

	return 0
}

func char_link(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	data = data[offset:]
	return 0
}

// '<' when tags or autolinks are allowed
func char_langle_tag(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	data = data[offset:]
	altype := MKDA_NOT_AUTOLINK
	end := tag_length(data, &altype)
	ret := 0

	if end > 2 {
		switch {
		case rndr.mk.autolink != nil && altype != MKDA_NOT_AUTOLINK:
			u_link := bytes.NewBuffer(nil)
			unscape_text(u_link, data[1:end-2])
			ret = rndr.mk.autolink(ob, u_link.Bytes(), altype, rndr.mk.opaque)
		case rndr.mk.raw_html_tag != nil:
			ret = rndr.mk.raw_html_tag(ob, data[:end], rndr.mk.opaque)
		}
	}

	if ret == 0 {
		return 0
	}
	return end
}

// '\\' backslash escape
var escape_chars = []byte("\\`*_{}[]()#+-.!:|&<>")

func char_escape(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	data = data[offset:]

	if len(data) > 1 {
		if bytes.IndexByte(escape_chars, data[1]) < 0 {
			return 0
		}

		if rndr.mk.normal_text != nil {
			rndr.mk.normal_text(ob, data[1:2], rndr.mk.opaque)
		} else {
			ob.WriteByte(data[1])
		}
	}

	return 2
}

// '&' escaped when it doesn't belong to an entity
// valid entities are assumed to be anything matching &#?[A-Za-z0-9]+;
func char_entity(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	data = data[offset:]

	end := 1

	if end < len(data) && data[end] == '#' {
		end++
	}

	for end < len(data) && (unicode.IsDigit(int(data[end])) || unicode.IsLetter(int(data[end]))) {
		end++
	}

	if end < len(data) && data[end] == ';' {
		end++ // real entity
	} else {
		return 0 // lone '&'
	}

	if rndr.mk.entity != nil {
		rndr.mk.entity(ob, data[:end], rndr.mk.opaque)
	} else {
		ob.Write(data[:end])
	}

	return end
}

func char_autolink(ob *bytes.Buffer, rndr *render, data []byte, offset int) int {
	//orig_data := data
	data = data[offset:]
	return 0
}

// taken from regexp in the stdlib
func ispunct(c int) bool {
	for _, r := range "!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~" {
		if c == r {
			return true
		}
	}
	return false
}

// return the length of the given tag, or 0 is it's not valid
func tag_length(data []byte, autolink *int) int {
	var i, j int

	// a valid tag can't be shorter than 3 chars
	if len(data) < 3 {
		return 0
	}

	// begins with a '<' optionally followed by '/', followed by letter or number
	if data[0] != '<' {
		return 0
	}
	if data[1] == '/' {
		i = 2
	} else {
		i = 1
	}

	if !unicode.IsDigit(int(data[i])) && !unicode.IsLetter(int(data[i])) {
		return 0
	}

	// scheme test
	*autolink = MKDA_NOT_AUTOLINK

	// try to find the beggining of an URI
	for i < len(data) && ((unicode.IsLetter(int(data[i])) || unicode.IsDigit(int(data[i]))) || data[i] == '.' || data[i] == '+' || data[i] == '-') {
		i++
	}

	if i > 1 && data[i] == '@' {
		if j = is_mail_autolink(data[i:]); j != 0 {
			*autolink = MKDA_EMAIL
			return i + j
		}
	}

	if i > 2 && data[i] == ':' {
		*autolink = MKDA_NORMAL
		i++
	}

	// complete autolink test: no whitespace or ' or "
	switch {
	case i >= len(data):
		*autolink = MKDA_NOT_AUTOLINK
	case *autolink != 0:
		j = i

		for i < len(data) {
			if data[i] == '\\' {
				i += 2
			} else {
				if data[i] == '>' || data[i] == '\'' || data[i] == '"' || unicode.IsSpace(int(data[i])) {
					break
				} else {
					i++
				}
			}

		}

		if i >= len(data) {
			return 0
		}
		if i > j && data[i] == '>' {
			return i + 1
		}

		// one of the forbidden chars has been found
		*autolink = MKDA_NOT_AUTOLINK
	}

	// looking for sometinhg looking like a tag end
	for i < len(data) && data[i] != '>' {
		i++
	}
	if i >= len(data) {
		return 0
	}
	return i + 1
}

// look for the address part of a mail autolink and '>'
// this is less strict than the original markdown e-mail address matching
func is_mail_autolink(data []byte) int {
	nb := 0

	// address is assumed to be: [-@._a-zA-Z0-9]+ with exactly one '@'
	for i := 0; i < len(data); i++ {
		if unicode.IsLetter(int(data[i])) || unicode.IsDigit(int(data[i])) {
			continue
		}

		switch data[i] {
		case '@':
			nb++

		case '-', '.', '_':
			break

		case '>':
			if nb == 1 {
				return i + 1
			} else {
				return 0
			}
		default:
			return 0
		}
	}

	return 0
}

// look for the next emph char, skipping other constructs
func find_emph_char(data []byte, c byte) int {
	i := 1

	for i < len(data) {
		for i < len(data) && data[i] != c && data[i] != '`' && data[i] != '[' {
			i++
		}
		if data[i] == c {
			return i
		}

		// do not count escaped chars
		if i != 0 && data[i-1] == '\\' {
			i++
			continue
		}

		if data[i] == '`' {
			// skip a code span
			tmp_i := 0
			i++
			for i < len(data) && data[i] != '`' {
				if tmp_i == 0 && data[i] == c {
					tmp_i = i
				}
				i++
			}
			if i >= len(data) {
				return tmp_i
			}
			i++
		} else {
			if data[i] == '[' {
				// skip a link
				tmp_i := 0
				i++
				for i < len(data) && data[i] != ']' {
					if tmp_i == 0 && data[i] == c {
						tmp_i = i
					}
					i++
				}
				i++
				for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n') {
					i++
				}
				if i >= len(data) {
					return tmp_i
				}
				if data[i] != '[' && data[i] != '(' { // not a link
					if tmp_i > 0 {
						return tmp_i
					} else {
						continue
					}
				}
				cc := data[i]
				i++
				for i < len(data) && data[i] != cc {
					if tmp_i == 0 && data[i] == c {
						tmp_i = i
					}
					i++
				}
				if i >= len(data) {
					return tmp_i
				}
				i++
			}
		}
	}
	return 0
}

func parse_emph1(ob *bytes.Buffer, rndr *render, data []byte, c byte) int {
	i := 0

	if rndr.mk.emphasis == nil {
		return 0
	}

	// skip one symbol if coming from emph3
	if len(data) > 1 && data[0] == c && data[1] == c {
		i = 1
	}

	for i < len(data) {
		length := find_emph_char(data[i:], c)
		if length == 0 {
			return 0
		}
		i += length
		if i >= len(data) {
			return 0
		}

		if i+1 < len(data) && data[i+1] == c {
			i++
			continue
		}

		if data[i] == c && !unicode.IsSpace(int(data[i-1])) {

			if rndr.ext_flags&MKDEXT_NO_INTRA_EMPHASIS != 0 {
				if !(i+1 == len(data) || unicode.IsSpace(int(data[i+1])) || ispunct(int(data[i+1]))) {
					continue
				}
			}

			work := bytes.NewBuffer(nil)
			parse_inline(work, rndr, data[:i])
			r := rndr.mk.emphasis(ob, work.Bytes(), rndr.mk.opaque)
			if r > 0 {
				return i + 1
			} else {
				return 0
			}
		}
	}

	return 0
}

func parse_emph2(ob *bytes.Buffer, rndr *render, data []byte, c byte) int {
	render_method := rndr.mk.double_emphasis
	if c == '~' {
		render_method = rndr.mk.strikethrough
	}

	if render_method == nil {
		return 0
	}

	i := 0

	for i < len(data) {
		length := find_emph_char(data[i:], c)
		if length == 0 {
			return 0
		}
		i += length

		if i+1 < len(data) && data[i] == c && data[i+1] == c && i > 0 && !unicode.IsSpace(int(data[i-1])) {
			work := bytes.NewBuffer(nil)
			parse_inline(work, rndr, data[:i])
			r := render_method(ob, work.Bytes(), rndr.mk.opaque)
			if r > 0 {
				return i + 2
			} else {
				return 0
			}
		}
		i++
	}
	return 0
}

func parse_emph3(ob *bytes.Buffer, rndr *render, data []byte, offset int, c byte) int {
	i := 0
	orig_data := data
	data = data[offset:]

	for i < len(data) {
		length := find_emph_char(data[i:], c)
		if length == 0 {
			return 0
		}
		i += length

		// skip whitespace preceded symbols
		if data[i] != c || unicode.IsSpace(int(data[i-1])) {
			continue
		}

		switch {
		case (i+2 < len(data) && data[i+1] == c && data[i+2] == c && rndr.mk.triple_emphasis != nil):
			// triple symbol found
			work := bytes.NewBuffer(nil)

			parse_inline(work, rndr, data[:i])
			r := rndr.mk.triple_emphasis(ob, work.Bytes(), rndr.mk.opaque)
			if r > 0 {
				return i + 3
			} else {
				return 0
			}
		case (i+1 < len(data) && data[i+1] == c):
			// double symbol found, handing over to emph1
			length = parse_emph1(ob, rndr, orig_data[offset-2:], c)
			if length == 0 {
				return 0
			} else {
				return length - 2
			}
		default:
			// single symbol found, handing over to emph2
			length = parse_emph2(ob, rndr, orig_data[offset-1:], c)
			if length == 0 {
				return 0
			} else {
				return length - 1
			}
		}
	}
	return 0
}

// parse block-level data
func parse_block(ob *bytes.Buffer, rndr *render, data []byte) {
	if rndr.nesting >= rndr.max_nesting {
		return
	}
	rndr.nesting++

	for len(data) > 0 {
		if is_atxheader(rndr, data) {
			data = data[parse_atxheader(ob, rndr, data):]
			continue
		}
		if data[0] == '<' && rndr.mk.blockhtml != nil {
			if i := parse_htmlblock(ob, rndr, data, true); i > 0 {
				data = data[i:]
				continue
			}
		}
		if i := is_empty(data); i > 0 {
			data = data[i:]
			continue
		}
		if is_hrule(data) {
			if rndr.mk.hrule != nil {
				rndr.mk.hrule(ob, rndr.mk.opaque)
			}
			var i int
			for i = 0; i < len(data) && data[i] != '\n'; i++ {
			}
			data = data[i:]
			continue
		}
		if rndr.ext_flags&MKDEXT_FENCED_CODE != 0 {
			if i := parse_fencedcode(ob, rndr, data); i > 0 {
				data = data[i:]
				continue
			}
		}
		if rndr.ext_flags&MKDEXT_TABLES != 0 {
			if i := parse_table(ob, rndr, data); i > 0 {
				data = data[i:]
				continue
			}
		}
		if prefix_quote(data) > 0 {
			data = data[parse_blockquote(ob, rndr, data):]
			continue
		}
		if prefix_code(data) > 0 {
			data = data[parse_blockcode(ob, rndr, data):]
			continue
		}
		if prefix_uli(data) > 0 {
			data = data[parse_list(ob, rndr, data, 0):]
			continue
		}
		if prefix_oli(data) > 0 {
			data = data[parse_list(ob, rndr, data, MKD_LIST_ORDERED):]
			continue
		}

		data = data[parse_paragraph(ob, rndr, data):]
	}

	rndr.nesting--
}

func is_atxheader(rndr *render, data []byte) bool {
	if data[0] != '#' {
		return false
	}

	if rndr.ext_flags&MKDEXT_SPACE_HEADERS != 0 {
		level := 0
		for level < len(data) && level < 6 && data[level] == '#' {
			level++
		}
		if level < len(data) && data[level] != ' ' && data[level] != '\t' {
			return false
		}
	}
	return true
}

func parse_atxheader(ob *bytes.Buffer, rndr *render, data []byte) int {
	level := 0
	for level < len(data) && level < 6 && data[level] == '#' {
		level++
	}
	i, end := 0, 0
	for i = level; i < len(data) && (data[i] == ' ' || data[i] == '\t'); i++ {
	}
	for end = i; end < len(data) && data[end] != '\n'; end++ {
	}
	skip := end
	for end > 0 && data[end-1] == '#' {
		end--
	}
	for end > 0 && (data[end-1] == ' ' || data[end-1] == '\t') {
		end--
	}
	if end > i {
		work := bytes.NewBuffer(nil)
		parse_inline(work, rndr, data[i:end])
		if rndr.mk.header != nil {
			rndr.mk.header(ob, work.Bytes(), level, rndr.mk.opaque)
		}
	}
	return skip
}

func is_headerline(data []byte) int {
	i := 0

	// test of level 1 header
	if data[i] == '=' {
		for i = 1; i < len(data) && data[i] == '='; i++ {
		}
		for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
			i++
		}
		if i >= len(data) || data[i] == '\n' {
			return 1
		} else {
			return 0
		}
	}

	// test of level 2 header
	if data[i] == '-' {
		for i = 1; i < len(data) && data[i] == '-'; i++ {
		}
		for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
			i++
		}
		if i >= len(data) || data[i] == '\n' {
			return 2
		} else {
			return 0
		}
	}

	return 0
}

func parse_htmlblock(ob *bytes.Buffer, rndr *render, data []byte, do_render bool) int {
	var i, j int

	// identify the opening tag
	if len(data) < 2 || data[0] != '<' {
		return 0
	}
	curtag, tagfound := find_block_tag(data[1:])

	// handle special cases
	if !tagfound {

		// HTML comment, laxist form
		if len(data) > 5 && data[1] == '!' && data[2] == '-' && data[3] == '-' {
			i = 5

			for i < len(data) && !(data[i-2] == '-' && data[i-1] == '-' && data[i] == '>') {
				i++
			}
			i++

			if i < len(data) {
				j = is_empty(data[i:])
			}

			if j > 0 {
				size := i + j
				if do_render && rndr.mk.blockhtml != nil {
					rndr.mk.blockhtml(ob, data[:size], rndr.mk.opaque)
				}
				return size
			}
		}

		// HR, which is the only self-closing block tag considered
		if len(data) > 4 && (data[i] == 'h' || data[1] == 'H') && (data[2] == 'r' || data[2] == 'R') {
			i = 3
			for i < len(data) && data[i] != '>' {
				i++
			}

			if i+1 < len(data) {
				i++
				j = is_empty(data[i:])
				if j > 0 {
					size := i + j
					if do_render && rndr.mk.blockhtml != nil {
						rndr.mk.blockhtml(ob, data[:size], rndr.mk.opaque)
					}
					return size
				}
			}
		}

		// no special case recognized
		return 0
	}

	// look for an unindented matching closing tag
	//      followed by a blank line
	i = 1
	found := false

	// if not found, try a second pass looking for indented match
	// but not if tag is "ins" or "del" (following original Markdown.pl)
	if curtag != "ins" && curtag != "del" {
		i = 1
		for i < len(data) {
			i++
			for i < len(data) && !(data[i-1] == '<' && data[i] == '/') {
				i++
			}

			if i+2+len(curtag) >= len(data) {
				break
			}

			j = htmlblock_end(curtag, rndr, data[i-1:])

			if j > 0 {
				i += j - 1
				found = true
				break
			}
		}
	}

	if !found {
		return 0
	}

	// the end of the block has been found
	if do_render && rndr.mk.blockhtml != nil {
		rndr.mk.blockhtml(ob, data[:i], rndr.mk.opaque)
	}

	return i
}

func find_block_tag(data []byte) (string, bool) {
	i := 0
	for i < len(data) && ((data[i] >= '0' && data[i] <= '9') || (data[i] >= 'A' && data[i] <= 'Z') || (data[i] >= 'a' && data[i] <= 'z')) {
		i++
	}
	if i >= len(data) {
		return "", false
	}
	key := string(data[:i])
	if _, ok := block_tags[key]; ok {
		return key, true
	}
	return "", false
}

func htmlblock_end(tag string, rndr *render, data []byte) int {
	// assume data[0] == '<' && data[1] == '/' already tested

	// check if tag is a match
	if len(tag)+3 >= len(data) || bytes.Compare(data[2:2+len(tag)], []byte(tag)) != 0 || data[len(tag)+2] != '>' {
		return 0
	}

	// check white lines
	i := len(tag) + 3
	w := 0
	if i < len(data) {
		if w = is_empty(data[i:]); w == 0 {
			return 0 // non-blank after tag
		}
	}
	i += w
	w = 0

	if rndr.ext_flags&MKDEXT_LAX_HTML_BLOCKS != 0 {
		if i < len(data) {
			w = is_empty(data[i:])
		}
	} else {
		if i < len(data) {
			if w = is_empty(data[i:]); w == 0 {
				return 0 // non-blank line after tag line
			}
		}
	}

	return i + w
}

func is_empty(data []byte) int {
	var i int
	for i = 0; i < len(data) && data[i] != '\n'; i++ {
		if data[i] != ' ' && data[i] != '\t' {
			return 0
		}
	}
	return i + 1
}

func is_hrule(data []byte) bool {
	// skip initial spaces
	if len(data) < 3 {
		return false
	}
	i := 0
	if data[0] == ' ' {
		i++
		if data[1] == ' ' {
			i++
			if data[2] == ' ' {
				i++
			}
		}
	}

	// look at the hrule char
	if i+2 >= len(data) || (data[i] != '*' && data[i] != '-' && data[i] != '_') {
		return false
	}
	c := data[i]

	// the whole line must be the char or whitespace
	n := 0
	for i < len(data) && data[i] != '\n' {
		switch {
		case data[i] == c:
			n++
		case data[i] != ' ' && data[i] != '\t':
			return false
		}
		i++
	}

	return n >= 3
}

func is_codefence(data []byte, syntax **string) int {
	i, n := 0, 0

	// skip initial spaces
	if len(data) < 3 {
		return 0
	}
	if data[0] == ' ' {
		i++
		if data[1] == ' ' {
			i++
			if data[2] == ' ' {
				i++
			}
		}
	}

	// look at the hrule char
	if i+2 >= len(data) || !(data[i] == '~' || data[i] == '`') {
		return 0
	}

	c := data[i]

	// the whole line must be the char or whitespace
	for i < len(data) && data[i] == c {
		n++
		i++
	}

	if n < 3 {
		return 0
	}

	if syntax != nil {
		syn := 0

		for i < len(data) && (data[i] == ' ' || data[i] == '\t') {
			i++
		}

		syntax_start := i

		if i < len(data) && data[i] == '{' {
			i++
			syntax_start++

			for i < len(data) && data[i] != '}' && data[i] != '\n' {
				syn++
				i++
			}

			if i == len(data) || data[i] != '}' {
				return 0
			}

			// string all whitespace at the beginning and the end
			// of the {} block
			for syn > 0 && unicode.IsSpace(int(data[syntax_start])) {
				syntax_start++
				syn--
			}

			for syn > 0 && unicode.IsSpace(int(data[syntax_start+syn-1])) {
				syn--
			}

			i++
		} else {
			for i < len(data) && !unicode.IsSpace(int(data[i])) {
				syn++
				i++
			}
		}

		language := string(data[syntax_start : syntax_start+syn])
		*syntax = &language
	}

	for i < len(data) && data[i] != '\n' {
		if !unicode.IsSpace(int(data[i])) {
			return 0
		}
		i++
	}

	return i + 1
}

func parse_fencedcode(ob *bytes.Buffer, rndr *render, data []byte) int {
	var lang *string
	beg := is_codefence(data, &lang)
	if beg == 0 {
		return 0
	}

	work := bytes.NewBuffer(nil)

	for beg < len(data) {
		fence_end := is_codefence(data[beg:], nil)
		if fence_end != 0 {
			beg += fence_end
			break
		}

		var end int
		for end = beg + 1; end < len(data) && data[end-1] != '\n'; end++ {
		}

		if beg < end {
			// verbatim copy to the working buffer, escaping entities
			if is_empty(data[beg:]) > 0 {
				work.WriteByte('\n')
			} else {
				work.Write(data[beg:end])
			}
		}
		beg = end
	}

	if work.Len() > 0 && work.Bytes()[work.Len()-1] != '\n' {
		work.WriteByte('\n')
	}

	if rndr.mk.blockcode != nil {
		syntax := ""
		if lang != nil {
			syntax = *lang
		}

		rndr.mk.blockcode(ob, work.Bytes(), syntax, rndr.mk.opaque)
	}

	return beg
}

func parse_table(ob *bytes.Buffer, rndr *render, data []byte) int {
	header_work := bytes.NewBuffer(nil)
	i, columns, col_data := parse_table_header(header_work, rndr, data)
	if i > 0 {
		body_work := bytes.NewBuffer(nil)

		for i < len(data) {
			pipes, row_start := 0, i
			for ; i < len(data) && data[i] != '\n'; i++ {
				if data[i] == '|' {
					pipes++
				}
			}

			if pipes == 0 || i == len(data) {
				i = row_start
				break
			}

			parse_table_row(body_work, rndr, data[row_start:i], columns, col_data)
			i++
		}

		if rndr.mk.table != nil {
			rndr.mk.table(ob, header_work.Bytes(), body_work.Bytes(), rndr.mk.opaque)
		}
	}

	return i
}

func parse_table_header(ob *bytes.Buffer, rndr *render, data []byte) (size int, columns int, column_data []int) {
	i, pipes := 0, 0
	column_data = []int{}
	for i = 0; i < len(data) && data[i] != '\n'; i++ {
		if data[i] == '|' {
			pipes++
		}
	}

	if i == len(data) || pipes == 0 {
		return 0, 0, column_data
	}

	header_end := i

	if data[0] == '|' {
		pipes--
	}

	if i > 2 && data[i-1] == '|' {
		pipes--
	}

	columns = pipes + 1
	column_data = make([]int, columns)

	// parse the header underline
	i++
	if i < len(data) && data[i] == '|' {
		i++
	}

	under_end := i
	for under_end < len(data) && data[under_end] != '\n' {
		under_end++
	}

	col := 0
	for ; col < columns && i < under_end; col++ {
		dashes := 0

		for i < under_end && (data[i] == ' ' || data[i] == '\t') {
			i++
		}

		if data[i] == ':' {
			i++
			column_data[col] |= MKD_TABLE_ALIGN_L
			dashes++
		}

		for i < under_end && data[i] == '-' {
			i++
			dashes++
		}

		if i < under_end && data[i] == ':' {
			i++
			column_data[col] |= MKD_TABLE_ALIGN_R
			dashes++
		}

		for i < under_end && (data[i] == ' ' || data[i] == '\t') {
			i++
		}

		if i < under_end && data[i] != '|' {
			break
		}

		if dashes < 3 {
			break
		}

		i++
	}

	if col < columns {
		return 0, 0, column_data
	}

	parse_table_row(ob, rndr, data[:header_end], columns, column_data)
	size = under_end + 1
	return
}

func parse_table_row(ob *bytes.Buffer, rndr *render, data []byte, columns int, col_data []int) {
	i, col := 0, 0
	row_work := bytes.NewBuffer(nil)

	if i < len(data) && data[i] == '|' {
		i++
	}

	for col = 0; col < columns && i < len(data); col++ {
		for i < len(data) && unicode.IsSpace(int(data[i])) {
			i++
		}

		cell_start := i

		for i < len(data) && data[i] != '|' {
			i++
		}

		cell_end := i - 1

		for cell_end > cell_start && unicode.IsSpace(int(data[cell_end])) {
			cell_end--
		}

		cell_work := bytes.NewBuffer(nil)
		parse_inline(cell_work, rndr, data[cell_start:cell_end+1])

		if rndr.mk.table_cell != nil {
			cdata := 0
			if col < len(col_data) {
				cdata = col_data[col]
			}
			rndr.mk.table_cell(row_work, cell_work.Bytes(), cdata, rndr.mk.opaque)
		}

		i++
	}

	for ; col < columns; col++ {
		empty_cell := []byte{}
		if rndr.mk.table_cell != nil {
			cdata := 0
			if col < len(col_data) {
				cdata = col_data[col]
			}
			rndr.mk.table_cell(row_work, empty_cell, cdata, rndr.mk.opaque)
		}
	}

	if rndr.mk.table_row != nil {
		rndr.mk.table_row(ob, row_work.Bytes(), rndr.mk.opaque)
	}
}

// returns blockquote prefix length
func prefix_quote(data []byte) int {
	i := 0
	for i < len(data) && i < 3 && data[i] == ' ' {
		i++
	}
	if i < len(data) && data[i] == '>' {
		if i+1 < len(data) && (data[i+1] == ' ' || data[i+1] == '\t') {
			return i + 2
		}
		return i + 1
	}
	return 0
}

// handles parsing of a blockquote fragment
func parse_blockquote(ob *bytes.Buffer, rndr *render, data []byte) int {
	out := bytes.NewBuffer(nil)
	work := bytes.NewBuffer(nil)
	beg, end := 0, 0
	for beg < len(data) {
		for end = beg + 1; end < len(data) && data[end-1] != '\n'; end++ {
		}

		if pre := prefix_quote(data[beg:]); pre > 0 {
			beg += pre // skip prefix
		} else {
			// empty line followed by non-quote line
			if is_empty(data[beg:]) > 0 && (end >= len(data) || (prefix_quote(data[end:]) == 0 && is_empty(data[end:]) == 0)) {
				break
			}
		}

		if beg < end { // copy into the in-place working buffer
			work.Write(data[beg:end])
		}
		beg = end
	}

	parse_block(out, rndr, work.Bytes())
	if rndr.mk.blockquote != nil {
		rndr.mk.blockquote(ob, out.Bytes(), rndr.mk.opaque)
	}
	return end
}

// returns prefix length for block code
func prefix_code(data []byte) int {
	if len(data) > 0 && data[0] == '\t' {
		return 1
	}
	if len(data) > 3 && data[0] == ' ' && data[1] == ' ' && data[2] == ' ' && data[3] == ' ' {
		return 4
	}
	return 0
}

func parse_blockcode(ob *bytes.Buffer, rndr *render, data []byte) int {
	work := bytes.NewBuffer(nil)

	beg, end := 0, 0
	for beg < len(data) {
		for end = beg + 1; end < len(data) && data[end-1] != '\n'; end++ {
		}

		chunk := data[beg:end]
		if pre := prefix_code(chunk); pre > 0 {
			beg += pre
		} else {
			if is_empty(chunk) == 0 {
				// non-empty non-prefixed line breaks the pre
				break
			}
		}

		if beg < end {
			// verbatim copy to the working buffer, escaping entities
			if is_empty(chunk) > 0 {
				work.WriteByte('\n')
			} else {
				work.Write(chunk)
			}
		}
		beg = end
	}

	// trim all the \n off the end of work
	workbytes := work.Bytes()
	n := 0
	for len(workbytes) > n && workbytes[len(workbytes)-n-1] == '\n' {
		n++
	}
	if n > 0 {
		work = bytes.NewBuffer(workbytes[:len(workbytes)-n])
	}

	work.WriteByte('\n')

	if rndr.mk.blockcode != nil {
		rndr.mk.blockcode(ob, work.Bytes(), "", rndr.mk.opaque)
	}

	return beg
}

// returns unordered list item prefix
func prefix_uli(data []byte) int {
	i := 0
	for i < len(data) && i < 3 && data[i] == ' ' {
		i++
	}
	if i+1 >= len(data) || (data[i] != '*' && data[i] != '+' && data[i] != '-') || (data[i+1] != ' ' && data[i+1] != '\t') {
		return 0
	}
	return i + 2
}

// returns ordered list item prefix
func prefix_oli(data []byte) int {
	i := 0
	for i < len(data) && i < 3 && data[i] == ' ' {
		i++
	}
	if i >= len(data) || data[i] < '0' || data[i] > '9' {
		return 0
	}
	for i < len(data) && data[i] >= '0' && data[i] <= '9' {
		i++
	}
	if i+1 >= len(data) || data[i] != '.' || (data[i+1] != ' ' && data[i+1] != '\t') {
		return 0
	}
	return i + 2
}

// parsing ordered or unordered list block
func parse_list(ob *bytes.Buffer, rndr *render, data []byte, flags int) int {
	work := bytes.NewBuffer(nil)

	i, j := 0, 0
	for i < len(data) {
		j = parse_listitem(work, rndr, data[i:], &flags)
		i += j

		if j == 0 || flags&MKD_LI_END != 0 {
			break
		}
	}

	if rndr.mk.list != nil {
		rndr.mk.list(ob, work.Bytes(), flags, rndr.mk.opaque)
	}
	return i
}

// parse a single list item
// assumes initial prefix is already removed
func parse_listitem(ob *bytes.Buffer, rndr *render, data []byte, flags *int) int {
	// keep track of the first indentation prefix
	beg, end, pre, sublist, orgpre, i := 0, 0, 0, 0, 0, 0

	for orgpre < 3 && orgpre < len(data) && data[orgpre] == ' ' {
		orgpre++
	}

	beg = prefix_uli(data)
	if beg == 0 {
		beg = prefix_oli(data)
	}
	if beg == 0 {
		return 0
	}

	// skip to the beginning of the following line
	end = beg
	for end < len(data) && data[end-1] != '\n' {
		end++
	}

	// get working buffers
	work := bytes.NewBuffer(nil)
	inter := bytes.NewBuffer(nil)

	// put the first line into the working buffer
	work.Write(data[beg:end])
	beg = end

	// process the following lines
	in_empty, has_inside_empty := false, false
	for beg < len(data) {
		end++

		for end < len(data) && data[end-1] != '\n' {
			end++
		}

		// process an empty line
		if is_empty(data[beg:end]) > 0 {
			in_empty = true
			beg = end
			continue
		}

		// calculate the indentation
		i = 0
		for i < 4 && beg+i < end && data[beg+i] == ' ' {
			i++
		}

		pre = i
		if data[beg] == '\t' {
			i = 1
			pre = 8
		}

		// check for a new item
		chunk := data[beg+i : end]
		if (prefix_uli(chunk) > 0 && !is_hrule(chunk)) || prefix_oli(chunk) > 0 {
			if in_empty {
				has_inside_empty = true
			}

			if pre == orgpre { // the following item must have the same indentation
				break
			}

			if sublist == 0 {
				sublist = work.Len()
			}
		} else {
			// only join indented stuff after empty lines
			if in_empty && i < 4 && data[beg] != '\t' {
				*flags |= MKD_LI_END
				break
			} else {
				if in_empty {
					work.WriteByte('\n')
					has_inside_empty = true
				}
			}
		}

		in_empty = false

		// add the line into the working buffer without prefix
		work.Write(data[beg+i : end])
		beg = end
	}

	// render li contents
	if has_inside_empty {
		*flags |= MKD_LI_BLOCK
	}

	workbytes := work.Bytes()
	if *flags&MKD_LI_BLOCK != 0 {
		// intermediate render of block li
		if sublist > 0 && sublist < len(workbytes) {
			parse_block(inter, rndr, workbytes[:sublist])
			parse_block(inter, rndr, workbytes[sublist:])
		} else {
			parse_block(inter, rndr, workbytes)
		}
	} else {
		// intermediate render of inline li
		if sublist > 0 && sublist < len(workbytes) {
			parse_inline(inter, rndr, workbytes[:sublist])
			parse_inline(inter, rndr, workbytes[sublist:])
		} else {
			parse_inline(inter, rndr, workbytes)
		}
	}

	// render li itself
	if rndr.mk.listitem != nil {
		rndr.mk.listitem(ob, inter.Bytes(), *flags, rndr.mk.opaque)
	}

	return beg
}

func parse_paragraph(ob *bytes.Buffer, rndr *render, data []byte) int {
	i, end, level := 0, 0, 0

	for i < len(data) {
		for end = i + 1; end < len(data) && data[end-1] != '\n'; end++ {
		}

		if is_empty(data[i:]) > 0 {
			break
		}
		if level = is_headerline(data[i:]); level > 0 {
			break
		}

		if rndr.ext_flags&MKDEXT_LAX_HTML_BLOCKS != 0 {
			if data[i] == '<' && rndr.mk.blockhtml != nil && parse_htmlblock(ob, rndr, data[i:], false) > 0 {
				end = i
				break
			}
		}

		if is_atxheader(rndr, data[i:]) || is_hrule(data[i:]) {
			end = i
			break
		}

		i = end
	}

	work := data
	size := i
	for size > 0 && work[size-1] == '\n' {
		size--
	}

	if level == 0 {
		tmp := bytes.NewBuffer(nil)
		parse_inline(tmp, rndr, work[:size])
		if rndr.mk.paragraph != nil {
			rndr.mk.paragraph(ob, tmp.Bytes(), rndr.mk.opaque)
		}
	} else {
		if size > 0 {
			beg := 0
			i = size
			size--

			for size > 0 && work[size] != '\n' {
				size--
			}

			beg = size + 1
			for size > 0 && work[size-1] == '\n' {
				size--
			}

			if size > 0 {
				tmp := bytes.NewBuffer(nil)
				parse_inline(tmp, rndr, work[:size])
				if rndr.mk.paragraph != nil {
					rndr.mk.paragraph(ob, tmp.Bytes(), rndr.mk.opaque)
				}

				work = work[beg:]
				size = i - beg
			} else {
				size = i
			}
		}

		header_work := bytes.NewBuffer(nil)
		parse_inline(header_work, rndr, work[:size])

		if rndr.mk.header != nil {
			rndr.mk.header(ob, header_work.Bytes(), level, rndr.mk.opaque)
		}
	}

	return end
}


//
//
// HTML rendering
//
//

const (
	HTML_SKIP_HTML = 1 << iota
	HTML_SKIP_STYLE
	HTML_SKIP_IMAGES
	HTML_SKIP_LINKS
	HTML_EXPAND_TABS
	HTML_SAFELINK
	HTML_TOC
	HTML_HARD_WRAP
	HTML_GITHUB_BLOCKCODE
	HTML_USE_XHTML
)

type html_renderopts struct {
	toc_data struct {
		header_count  int
		current_level int
	}
	flags     uint32
	close_tag string
}

func attr_escape(ob *bytes.Buffer, src []byte) {
	ob.WriteString(html.EscapeString(string(src)))
}

func unscape_text(ob *bytes.Buffer, src []byte) {
	i := 0
	for i < len(src) {
		org := i
		for i < len(src) && src[i] != '\\' {
			i++
		}

		if i > org {
			ob.Write(src[org:i])
		}

		if i+1 >= len(src) {
			break
		}

		ob.WriteByte(src[i+1])
		i += 2
	}
}

func rndr_header(ob *bytes.Buffer, text []byte, level int, opaque interface{}) {
	options := opaque.(*html_renderopts)

	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}

	if options.flags&HTML_TOC != 0 {
		ob.WriteString(fmt.Sprintf("<h%d id=\"toc_%d\">", level, options.toc_data.header_count))
		options.toc_data.header_count++
	} else {
		ob.WriteString(fmt.Sprintf("<h%d>", level))
	}

	ob.Write(text)
	ob.WriteString(fmt.Sprintf("</h%d>\n", level))
}

func rndr_raw_block(ob *bytes.Buffer, text []byte, opaque interface{}) {
	sz := len(text)
	for sz > 0 && text[sz-1] == '\n' {
		sz--
	}
	org := 0
	for org < sz && text[org] == '\n' {
		org++
	}
	if org >= sz {
		return
	}
	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}
	ob.Write(text[org:sz])
	ob.WriteByte('\n')
}

func rndr_hrule(ob *bytes.Buffer, opaque interface{}) {
	options := opaque.(*html_renderopts)

	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}
	ob.WriteString("<hr")
	ob.WriteString(options.close_tag)
}

func rndr_blockcode(ob *bytes.Buffer, text []byte, lang string, opaque interface{}) {
	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}

	if lang != "" {
		ob.WriteString("<pre><code class=\"")

		for i, cls := 0, 0; i < len(lang); i, cls = i+1, cls+1 {
			for i < len(lang) && unicode.IsSpace(int(lang[i])) {
				i++
			}

			if i < len(lang) {
				org := i
				for i < len(lang) && !unicode.IsSpace(int(lang[i])) {
					i++
				}

				if lang[org] == '.' {
					org++
				}

				if cls > 0 {
					ob.WriteByte(' ')
				}
				attr_escape(ob, []byte(lang[org:]))
			}
		}

		ob.WriteString("\">")
	} else {
		ob.WriteString("<pre><code>")
	}

	if len(text) > 0 {
		attr_escape(ob, text)
	}

	ob.WriteString("</code></pre>\n")
}

func rndr_table(ob *bytes.Buffer, header []byte, body []byte, opaque interface{}) {
	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}
	ob.WriteString("<table><thead>\n")
	ob.Write(header)
	ob.WriteString("\n</thead><tbody>\n")
	ob.Write(body)
	ob.WriteString("\n</tbody></table>")
}

func rndr_tablerow(ob *bytes.Buffer, text []byte, opaque interface{}) {
	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}
	ob.WriteString("<tr>\n")
	ob.Write(text)
	ob.WriteString("\n</tr>")
}

func rndr_tablecell(ob *bytes.Buffer, text []byte, align int, opaque interface{}) {
	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}
	switch align {
	case MKD_TABLE_ALIGN_L:
		ob.WriteString("<td align=\"left\">")
	case MKD_TABLE_ALIGN_R:
		ob.WriteString("<td align=\"right\">")
	case MKD_TABLE_ALIGN_CENTER:
		ob.WriteString("<td align=\"center\">")
	default:
		ob.WriteString("<td>")
	}

	ob.Write(text)
	ob.WriteString("</td>")
}

func rndr_list(ob *bytes.Buffer, text []byte, flags int, opaque interface{}) {
	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}
	if flags&MKD_LIST_ORDERED != 0 {
		ob.WriteString("<ol>\n")
	} else {
		ob.WriteString("<ul>\n")
	}
	ob.Write(text)
	if flags&MKD_LIST_ORDERED != 0 {
		ob.WriteString("</ol>\n")
	} else {
		ob.WriteString("</ul>\n")
	}
}

func rndr_listitem(ob *bytes.Buffer, text []byte, flags int, opaque interface{}) {
	ob.WriteString("<li>")
	size := len(text)
	for size > 0 && text[size-1] == '\n' {
		size--
	}
	ob.Write(text[:size])
	ob.WriteString("</li>\n")
}

func rndr_paragraph(ob *bytes.Buffer, text []byte, opaque interface{}) {
	options := opaque.(*html_renderopts)
	i := 0

	if ob.Len() > 0 {
		ob.WriteByte('\n')
	}

	if len(text) == 0 {
		return
	}

	for i < len(text) && unicode.IsSpace(int(text[i])) {
		i++
	}

	if i == len(text) {
		return
	}

	ob.WriteString("<p>")
	if options.flags&HTML_HARD_WRAP != 0 {
		for i < len(text) {
			org := i
			for i < len(text) && text[i] != '\n' {
				i++
			}

			if i > org {
				ob.Write(text[org:i])
			}

			if i >= len(text) {
				break
			}

			ob.WriteString("<br>")
			ob.WriteString(options.close_tag)
			i++
		}
	} else {
		ob.Write(text[i:])
	}
	ob.WriteString("</p>\n")
}


func main() {
	ob := bytes.NewBuffer(nil)
	input := ""
	input += "##Header##\n"
	input += "\n"
	input += "----------\n"
	input += "\n"
	input += "Underlined header\n"
	input += "-----------------\n"
	input += "\n"
	input += "<p>Some block html\n"
	input += "</p>\n"
	input += "\n"
	input += "Score | Grade\n"
	input += "------|------\n"
	input += "94    | A\n"
	input += "85    | B\n"
	input += "74    | C\n"
	input += "65    | D\n"
	input += "\n"
	input += "``` go\n"
	input += "func fib(n int) int {\n"
	input += "    if n <= 1 {\n"
	input += "        return n\n"
	input += "    }\n"
	input += "    return n * fib(n-1)\n"
	input += "}\n"
	input += "```\n"
	input += "\n"
	input += "> A blockquote\n"
	input += "> or something like that\n"
	input += "> With a table | of two columns\n"
	input += "> -------------|---------------\n"
	input += "> key          | value \n"
	input += "\n"
	input += "\n"
	input += "Some **bold** Some *italic* and [a link][1] \n"
	input += "\n"
	input += "A little code sample\n"
	input += "\n"
	input += "    </head>\n"
	input += "    <title>Web Page Title</title>\n"
	input += "    </head>\n"
	input += "\n"
	input += "A picture\n"
	input += "\n"
	input += "![alt text][2]\n"
	input += "\n"
	input += "A list\n"
	input += "\n"
	input += "- apples\n"
	input += "- oranges\n"
	input += "- eggs\n"
	input += "\n"
	input += "A numbered list\n"
	input += "\n"
	input += "1. a\n"
	input += "2. b\n"
	input += "3. c\n"
	input += "\n"
	input += "A little quote\n"
	input += "\n"
	input += "> It is now time for all good men to come to the aid of their country. \n"
	input += "\n"
	input += "A final paragraph.\n"
	input += "\n"
	input += "  [1]: http://www.google.com\n"
	input += "  [2]: http://www.google.com/intl/en_ALL/images/logo.gif\n"

	ib := []byte(input)
	rndrer := new(mkd_renderer)
	rndrer.blockcode = rndr_blockcode
	rndrer.blockhtml = rndr_raw_block
	rndrer.header = rndr_header
	rndrer.hrule = rndr_hrule
	rndrer.list = rndr_list
	rndrer.listitem = rndr_listitem
	rndrer.paragraph = rndr_paragraph
	rndrer.table = rndr_table
	rndrer.table_row = rndr_tablerow
	rndrer.table_cell = rndr_tablecell
	rndrer.opaque = &html_renderopts{close_tag: " />"}
	var extensions uint32 = MKDEXT_FENCED_CODE | MKDEXT_TABLES
	Markdown(ob, ib, rndrer, extensions)
	fmt.Print(ob.String())
}

func expand_tabs(ob *bytes.Buffer, line []byte) {
	i, tab := 0, 0

	for i < len(line) {
		org := i
		for i < len(line) && line[i] != '\t' {
			i++
			tab++
		}

		if i > org {
			ob.Write(line[org:i])
		}

		if i >= len(line) {
			break
		}

		for {
			ob.WriteByte(' ')
			tab++
			if tab%4 == 0 {
				break
			}
		}

		i++
	}
}

func Markdown(ob *bytes.Buffer, ib []byte, rndrer *mkd_renderer, extensions uint32) {
	// no point in parsing if we can't render
	if rndrer == nil {
		return
	}

	// fill in the character-level parsers
	markdown_char_ptrs[MD_CHAR_NONE] = nil
	markdown_char_ptrs[MD_CHAR_EMPHASIS] = char_emphasis
	markdown_char_ptrs[MD_CHAR_CODESPAN] = char_codespan
	markdown_char_ptrs[MD_CHAR_LINEBREAK] = char_linebreak
	markdown_char_ptrs[MD_CHAR_LINK] = char_link
	markdown_char_ptrs[MD_CHAR_LANGLE] = char_langle_tag
	markdown_char_ptrs[MD_CHAR_ESCAPE] = char_escape
	markdown_char_ptrs[MD_CHAR_ENTITITY] = char_entity
	markdown_char_ptrs[MD_CHAR_AUTOLINK] = char_autolink

	// fill in the render structure
	rndr := new(render)
	rndr.mk = rndrer
	rndr.ext_flags = extensions
	rndr.max_nesting = 16

	if rndr.mk.emphasis != nil || rndr.mk.double_emphasis != nil || rndr.mk.triple_emphasis != nil {
		rndr.active_char['*'] = MD_CHAR_EMPHASIS
		rndr.active_char['_'] = MD_CHAR_EMPHASIS
		if extensions&MKDEXT_STRIKETHROUGH != 0 {
			rndr.active_char['~'] = MD_CHAR_EMPHASIS
		}
	}
	if rndr.mk.codespan != nil {
		rndr.active_char['`'] = MD_CHAR_CODESPAN
	}
	if rndr.mk.linebreak != nil {
		rndr.active_char['\n'] = MD_CHAR_LINEBREAK
	}
	if rndr.mk.image != nil || rndr.mk.link != nil {
		rndr.active_char['['] = MD_CHAR_LINK
	}
	rndr.active_char['<'] = MD_CHAR_LANGLE
	rndr.active_char['\\'] = MD_CHAR_ESCAPE
	rndr.active_char['&'] = MD_CHAR_ENTITITY

	if extensions&MKDEXT_AUTOLINK != 0 {
		rndr.active_char['h'] = MD_CHAR_AUTOLINK // http, https
		rndr.active_char['H'] = MD_CHAR_AUTOLINK

		rndr.active_char['f'] = MD_CHAR_AUTOLINK // ftp
		rndr.active_char['F'] = MD_CHAR_AUTOLINK

		rndr.active_char['m'] = MD_CHAR_AUTOLINK // mailto
		rndr.active_char['M'] = MD_CHAR_AUTOLINK
	}

	// first pass: look for references, copying everything else
	text := bytes.NewBuffer(nil)
	beg, end := 0, 0
	for beg < len(ib) { // iterate over lines
		if is_ref(ib, beg, &end, rndr) {
			beg = end
		} else { // skip to the next line
			end = beg
			for end < len(ib) && ib[end] != '\n' && ib[end] != '\r' {
				end++
			}

			// add the line body if present
			if end > beg {
				expand_tabs(text, ib[beg:end])
			}

			for end < len(ib) && (ib[end] == '\n' || ib[end] == '\r') {
				// add one \n per newline
				if ib[end] == '\n' || (end+1 < len(ib) && ib[end+1] != '\n') {
					text.WriteByte('\n')
				}
				end++
			}

			beg = end
		}
	}

	// sort the reference array
	if len(rndr.refs) > 1 {
		sort.Sort(rndr.refs)
	}

	// second pass: actual rendering
	if rndr.mk.doc_header != nil {
		rndr.mk.doc_header(ob, rndr.mk.opaque)
	}

	if text.Len() > 0 {
		// add a final newline if not already present
		finalchar := text.Bytes()[text.Len()-1]
		if finalchar != '\n' && finalchar != '\r' {
			text.WriteByte('\n')
		}
		parse_block(ob, rndr, text.Bytes())
	}

	if rndr.mk.doc_footer != nil {
		rndr.mk.doc_footer(ob, rndr.mk.opaque)
	}

	if rndr.nesting != 0 {
		panic("Nesting level did not end at zero")
	}
}
