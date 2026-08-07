package main

import (
	"html"
	"strings"
	"unicode/utf8"
)

// specials maps troff special-character escapes to their Unicode equivalents.
// Derived from an inventory of every \(xx sequence used in the Tcl/Tk corpus.
var specials = map[string]string{
	"->": "\u2192", "<-": "\u2190", "bu": "\u2022", "em": "\u2014",
	"en": "\u2013", "co": "\u00a9", "rg": "\u00ae", "tm": "\u2122",
	"fm": "\u2032", "+-": "\u00b1", "mu": "\u00d7", "mi": "\u2212",
	"pl": "+", "eq": "=", "==": "\u2261", "!=": "\u2260",
	"<=": "\u2264", ">=": "\u2265", "mc": "\u00b5", "de": "\u00b0",
	"dq": "\"", "aq": "'", "ga": "`", "ha": "^", "ti": "~",
	"^o": "\u00f4", "~o": "\u00f5", "~n": "\u00f1", "~a": "\u00e3",
	"~O": "\u00d5", "^a": "\u00e2", "^e": "\u00ea", "'e": "\u00e9",
	"`e": "\u00e8", ":u": "\u00fc", ":o": "\u00f6", ":a": "\u00e4",
	"ss": "\u00df", "12": "\u00bd", "14": "\u00bc", "34": "\u00be",
	"lq": "\u201c", "rq": "\u201d",
}

// fontClass maps a troff font code to the HTML element used to render it.
// B is used for literal Tcl syntax, I for user-substituted arguments; keeping
// them semantically distinct is what lets the CSS style arguments differently.
var fontTag = map[byte]string{'B': "code", 'I': "var"}

// inlineState carries font state across the lines of a single filled paragraph,
// because troff font escapes are not required to balance within a line.
type inlineState struct {
	font byte // 'R', 'B', or 'I'
	open string
}

func newInline() *inlineState { return &inlineState{font: 'R'} }

// reset closes any dangling font element. Called at block boundaries.
func (s *inlineState) reset() string {
	out := ""
	if s.open != "" {
		out = "</" + s.open + ">"
		s.open = ""
	}
	s.font = 'R'
	return out
}

func (s *inlineState) setFont(f byte) string {
	if f == 'P' {
		f = 'R' // \fP means "previous"; treating it as roman is right often enough
	}
	if f == s.font {
		return ""
	}
	out := ""
	if s.open != "" {
		out += "</" + s.open + ">"
		s.open = ""
	}
	if tag, ok := fontTag[f]; ok {
		out += "<" + tag + ">"
		s.open = tag
	}
	s.font = f
	return out
}

// text converts one line of troff source to HTML, honouring and updating font
// state. Escape handling is deliberately limited to what the corpus uses.
func (s *inlineState) text(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); i++ {
		c := line[i]
		if c >= utf8.RuneSelf {
			// Copy multi-byte UTF-8 through intact. Promoting bytes to runes
			// one at a time would mangle the typographic quotes and accented
			// names that a handful of tcllib pages carry.
			r, size := utf8.DecodeRuneInString(line[i:])
			switch {
			case r == utf8.RuneError && size == 1:
				b.WriteRune(decodeLegacy(c)) // not UTF-8 at all
			case r >= 0x80 && r <= 0x9f:
				// Valid UTF-8, but a C1 control: upstream encoded Windows-1252
				// text without transcoding it. Same repair.
				b.WriteRune(decodeLegacy(byte(r)))
			default:
				b.WriteString(line[i : i+size])
			}
			i += size - 1
			continue
		}
		if c != '\\' {
			escapeByte(&b, c)
			continue
		}
		if i+1 >= len(line) {
			break // trailing backslash: line continuation, drop it
		}
		i++
		switch line[i] {
		case 'f':
			if i+1 < len(line) {
				i++
				if line[i] == '(' && i+2 < len(line) {
					i += 2 // \f(XX two-character font name; treat as roman
					b.WriteString(s.setFont('R'))
				} else {
					b.WriteString(s.setFont(line[i]))
				}
			}
		case '(':
			if i+2 < len(line) {
				key := line[i+1 : i+3]
				i += 2
				if v, ok := specials[key]; ok {
					b.WriteString(html.EscapeString(v))
				}
			}
		case '*':
			// String register: \*(xx. Only the quote registers matter here.
			if i+1 < len(line) && line[i+1] == '(' && i+3 < len(line) {
				key := line[i+2 : i+4]
				i += 3
				if v, ok := specials[key]; ok {
					b.WriteString(html.EscapeString(v))
				}
			}
		case '-':
			b.WriteString("-")
		case 'e':
			b.WriteString(`\`)
		case '0':
			b.WriteString("&numsp;")
		case '|', '^', '&', ':', 'z', 'c':
			// zero-width and fine-spacing escapes: no output
		case ' ':
			b.WriteString("&nbsp;")
		case '"':
			return b.String() // rest of line is a comment
		case 'N':
			// \N'ddd' numeric character reference
			if i+1 < len(line) && line[i+1] == '\'' {
				j := strings.IndexByte(line[i+2:], '\'')
				if j >= 0 {
					b.WriteString("&#" + line[i+2:i+2+j] + ";")
					i += 2 + j
				}
			}
		default:
			escapeByte(&b, line[i])
		}
	}
	return b.String()
}

// cp1252C1 covers the bytes Windows-1252 assigns to the C1 control range. A
// byte that is not part of a valid UTF-8 sequence is, in this corpus, always a
// smart quote or a dash from a word processor rather than a control character.
var cp1252C1 = [32]rune{
	0x00: '€', 0x02: '‚', 0x03: 'ƒ', 0x04: '„',
	0x05: '…', 0x06: '†', 0x07: '‡', 0x08: 'ˆ',
	0x09: '‰', 0x0a: 'Š', 0x0b: '‹', 0x0c: 'Œ',
	0x0e: 'Ž', 0x11: '‘', 0x12: '’', 0x13: '“',
	0x14: '”', 0x15: '•', 0x16: '–', 0x17: '—',
	0x18: '˜', 0x19: '™', 0x1a: 'š', 0x1b: '›',
	0x1c: 'œ', 0x1e: 'ž', 0x1f: 'Ÿ',
}

// decodeLegacy interprets a byte that did not decode as ordinary UTF-8 text,
// as Windows-1252 — which is Latin-1 everywhere except the C1 range.
func decodeLegacy(c byte) rune {
	if c >= 0x80 && c <= 0x9f {
		if r := cp1252C1[c-0x80]; r != 0 {
			return r
		}
	}
	return rune(c)
}

// escapeByte writes one ASCII source byte as HTML, matching html.EscapeString.
func escapeByte(b *strings.Builder, c byte) {
	switch c {
	case '&':
		b.WriteString("&amp;")
	case '<':
		b.WriteString("&lt;")
	case '>':
		b.WriteString("&gt;")
	case '"':
		b.WriteString("&#34;")
	case '\'':
		b.WriteString("&#39;")
	default:
		b.WriteByte(c)
	}
}

// plain renders a line with all markup stripped, for slugs, summaries and the
// search index.
func plain(line string) string {
	s := newInline()
	h := s.text(line) + s.reset()
	h = strings.NewReplacer(
		"<code>", "", "</code>", "", "<var>", "", "</var>", "",
		"&numsp;", " ", "&nbsp;", " ",
	).Replace(h)
	return strings.TrimSpace(html.UnescapeString(h))
}

// args splits a macro argument line the way troff does: on whitespace, but
// respecting double quotes so that .SH "SEE ALSO" yields one argument.
func args(rest string) []string {
	var out []string
	var cur strings.Builder
	inQ, have := false, false
	flush := func() {
		if have {
			out = append(out, cur.String())
			cur.Reset()
			have = false
		}
	}
	for i := 0; i < len(rest); i++ {
		switch c := rest[i]; {
		case c == '"':
			inQ = !inQ
			have = true
		case (c == ' ' || c == '\t') && !inQ:
			flush()
		default:
			cur.WriteByte(c)
			have = true
		}
	}
	flush()
	return out
}
