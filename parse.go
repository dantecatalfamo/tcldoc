package main

import (
	"html"
	"regexp"
	"strconv"
	"strings"
)

type Kind int

const (
	KPara Kind = iota
	KPre
	KSection
	KSubsection
	KDefList
	KDefItem
	KBullets
	KItem
	KOption
	KIndent
	KNote
	KStdOpts
)

type Node struct {
	Kind     Kind
	Text     string // inline HTML: body for KPara/KNote, heading for sections
	Tag      string // inline HTML for KDefItem / KItem labels
	ID       string
	Meta     []string // KOption: command-line name, database name, database class
	Children []*Node
}

func (n *Node) add(c *Node) *Node { n.Children = append(n.Children, c); return c }
func (n *Node) last() *Node {
	if len(n.Children) == 0 {
		return nil
	}
	return n.Children[len(n.Children)-1]
}

// Entry is an addressable thing inside a page: an ensemble subcommand, an
// object method, or a widget option. These are what the default Tcl site has no
// way to link to, and they roughly quadruple the size of the index.
type Entry struct {
	Name    string `json:"n"`
	Anchor  string `json:"a"`
	Kind    string `json:"k"`
	Context string `json:"c"` // enclosing section heading
}

type Page struct {
	File     string
	Name     string   // identity: derived from the source filename
	Title    string   // display name, from .TH (may contain spaces)
	Names    []string // every name on the NAME line
	Section  string   // n, 1 or 3
	Source   string   // .TH's source field: the distribution the page ships in
	Manual   string   // trailing quoted field of .TH
	Summary  string
	Keywords []string
	SeeAlso  []string
	Entries  []Entry
	Root     *Node
}

// skipMacros are layout, register and conditional directives with no bearing on
// the document structure we care about.
var skipMacros = map[string]bool{
	"so": true, "ta": true, "AS": true, "BS": true, "BE": true,
	"VS": true, "VE": true, "ie": true, "el": true, "if": true,
	"nr": true, "ds": true, "ft": true, "ll": true, "in": true,
	"na": true, "ad": true, "hy": true, "nh": true, "ne": true,
	"rn": true, "rm": true, "tr": true, "fc": true, "am": true,
	"CT": true, "UL": true, "DT": true, "PD": true, "RT": true,
}

var slugBad = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = slugBad.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// anchorFor keeps the leading hyphen of an option name, so the anchor for
// -takefocus is "#-takefocus" and cannot collide with a section called
// "takefocus".
func anchorFor(s string) string {
	if strings.HasPrefix(strings.TrimSpace(s), "-") {
		return "-" + slug(s)
	}
	return slug(s)
}

// acronyms are the tokens that must not be lowercased when a shouting heading
// is converted to sentence case.
var acronyms = map[string]string{
	"TCL": "Tcl", "TK": "Tk", "TCLOO": "TclOO", "TTK": "Ttk", "ITCL": "Itcl",
	"API": "API", "URL": "URL", "URI": "URI", "HTTP": "HTTP", "HTTPS": "HTTPS",
	"GUI": "GUI", "CPU": "CPU", "OS": "OS", "ID": "ID", "IO": "I/O",
	"EOF": "EOF", "TLS": "TLS", "SSL": "SSL", "DNS": "DNS", "MIME": "MIME",
	"JSON": "JSON", "XML": "XML", "HTML": "HTML", "UTF": "UTF", "ASCII": "ASCII",
	"TCP": "TCP", "UDP": "UDP", "IP": "IP", "SQL": "SQL", "RGB": "RGB",
	"DPI": "DPI", "PDF": "PDF", "PNG": "PNG", "GIF": "GIF", "JPEG": "JPEG",
	"X11": "X11", "UNIX": "Unix", "VFS": "VFS", "ZIP": "ZIP", "FIFO": "FIFO",
	"NUL": "NUL", "ANSI": "ANSI", "POSIX": "POSIX", "IDNA": "IDNA", "DDE": "DDE",
	"C": "C", "X": "X", "A": "A", "AND": "and", "OR": "or", "OF": "of",
	"THE": "the", "TO": "to", "IN": "in", "FOR": "for", "WITH": "with",
	"ON": "on", "BY": "by", "AS": "as", "AT": "at", "FROM": "from",
}

// titleize converts an all-caps troff heading to sentence case, leaving
// identifiers and mixed-case headings untouched.
func titleize(s string) string {
	if s != strings.ToUpper(s) || strings.ContainsAny(s, `\<`) {
		return s
	}
	toks := strings.Split(s, " ")
	for i, t := range toks {
		trimmed := strings.Trim(t, "(),.:;?!\"")
		if trimmed == "" {
			continue
		}
		var repl string
		switch {
		case strings.ContainsAny(trimmed, "_0123456789") || strings.Contains(trimmed, "::"):
			continue // identifier such as TCL_BADCHANNELOPTION or CLOSE2PROC
		case acronyms[trimmed] != "":
			repl = acronyms[trimmed]
		default:
			repl = strings.ToLower(trimmed)
		}
		toks[i] = strings.Replace(t, trimmed, repl, 1)
	}
	out := strings.Join(toks, " ")
	for i, r := range out {
		if r >= 'a' && r <= 'z' {
			return out[:i] + strings.ToUpper(string(r)) + out[i+1:]
		}
		if r >= 'A' && r <= 'Z' {
			break
		}
	}
	return out
}

var firstCode = regexp.MustCompile(`(?s)^\s*<code>(.*?)</code>`)
var tagStrip = regexp.MustCompile(`<[^>]+>`)

type parser struct {
	page  *Page
	stack []*Node
	ins   *inlineState

	para     *strings.Builder // open filled paragraph
	pre      *strings.Builder // open verbatim block
	awaitTag *Node            // .TP/.AP/.OP item whose label is the next text line
	section  string           // current .SH heading, plain text
	capture  *[]string        // when set, text lines are metadata, not body
	nameLn   []string
	kwLn     []string
	saLn     []string
	seen     map[string]bool
	ids      map[string]int
}

func newParser() *parser {
	root := &Node{}
	return &parser{
		page:  &Page{Root: root},
		stack: []*Node{root},
		ins:   newInline(),
		seen:  map[string]bool{},
		ids:   map[string]int{},
	}
}

func (p *parser) top() *Node { return p.stack[len(p.stack)-1] }

func (p *parser) push(n *Node) { p.stack = append(p.stack, n) }

func (p *parser) popTo(kinds ...Kind) {
	want := map[Kind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	for len(p.stack) > 1 {
		if want[p.top().Kind] {
			return
		}
		p.stack = p.stack[:len(p.stack)-1]
	}
}

// popKind pops the innermost container of the given kind, inclusive.
func (p *parser) popKind(k Kind) {
	for len(p.stack) > 1 {
		cur := p.top()
		p.stack = p.stack[:len(p.stack)-1]
		if cur.Kind == k {
			return
		}
	}
}

// uniqueID keeps anchors stable and collision-free within a page.
func (p *parser) uniqueID(base string) string {
	if base == "" {
		return ""
	}
	p.ids[base]++
	if n := p.ids[base]; n > 1 {
		return base + "-" + strconv.Itoa(n)
	}
	return base
}

// --- block lifecycle -------------------------------------------------------

func (p *parser) endPara() {
	if p.para == nil {
		return
	}
	txt := strings.TrimSpace(p.para.String() + p.ins.reset())
	p.para = nil
	if txt == "" {
		return
	}
	p.top().add(&Node{Kind: KPara, Text: txt})
}

func (p *parser) endPre() {
	if p.pre == nil {
		return
	}
	txt := strings.Trim(p.pre.String(), "\n")
	p.pre = nil
	p.ins.reset()
	if strings.TrimSpace(txt) == "" {
		return
	}
	p.top().add(&Node{Kind: KPre, Text: txt})
}

func (p *parser) flush() { p.endPara(); p.endPre() }

func (p *parser) addText(line string) {
	if p.pre != nil {
		p.pre.WriteString(p.ins.text(line) + "\n")
		return
	}
	if p.capture != nil {
		*p.capture = append(*p.capture, line)
		return
	}
	if p.awaitTag != nil {
		n := p.awaitTag
		p.awaitTag = nil
		n.Tag = strings.TrimSpace(p.ins.text(line) + p.ins.reset())
		p.registerEntry(n)
		p.push(n)
		return
	}
	if p.para == nil {
		p.para = &strings.Builder{}
	} else {
		p.para.WriteString(" ")
	}
	p.para.WriteString(p.ins.text(line))
}

// trimSyntax drops the argument syntax a leading bold run can end with. troff
// sets "array for {key value} body" as a bold "array for {" followed by
// italics, so the extracted name would otherwise keep the brace.
//
// "(", "[" and "{" can never end a Tcl command name, so they always go. "?",
// "<" and ">" can — tcllib documents the methods "as?" and
// "::fileutil::magic::rt::<" — so those are dropped only when they stand as a
// separate trailing token, as in "itk_initialize ?" and "event add <<".
func trimSyntax(s string) string {
	for {
		if t := strings.TrimRight(s, " ({["); t != s {
			s = t
			continue
		}
		i := strings.LastIndexByte(s, ' ')
		if i < 0 || strings.Trim(s[i+1:], "?<>") != "" {
			return strings.TrimSpace(s)
		}
		s = s[:i]
	}
}

// indexable rejects definition labels that are not addressable API. troff has
// no table macro in this corpus, so .TP doubles as the layout for reference
// tables: clock(n) contributes forty labels like "%A", expr(n) lays its
// operator precedence out as "lt  gt  le  ge", and method signatures name their
// arguments "$result". None of those deserve a permalink or an index row.
func indexable(lead string) bool {
	switch {
	case !strings.ContainsFunc(lead, isAlnum):
		return false // "#", "!", "--", "<  >"
	case lead[0] == '%' && len(lead) <= 3:
		return false // clock and bind format specifier
	case lead[0] == '$' && !strings.Contains(lead, "::") && !strings.Contains(lead, " "):
		// A bare "$result" or "$traverser" names the object a command returns,
		// not a callable. "${log}::debug" and "$pca eigenvalues" are callables.
		return false
	case strings.Contains(lead, "  "):
		return false // a multi-column table row
	}
	return true
}

func isAlnum(r rune) bool {
	return r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

// registerEntry derives an anchor and a search entry from a definition label.
// The leading bold run of a .TP label is the callable signature, e.g.
// "\fBstring cat\fR ?\fIstring\fR ...?" yields "string cat".
func (p *parser) registerEntry(n *Node) {
	m := firstCode.FindStringSubmatch(n.Tag)
	if m == nil {
		return
	}
	lead := strings.ReplaceAll(tagStrip.ReplaceAllString(m[1], ""), "&numsp;", " ")
	// Unescape: the label is inline HTML, but an entry name is plain text and
	// gets escaped again on the way out. Left alone, the "<" operator in expr(n)
	// reached the index as the literal text "&lt;".
	lead = html.UnescapeString(lead)
	lead = strings.TrimSpace(strings.ReplaceAll(lead, " ", " "))
	lead = trimSyntax(lead)
	if lead == "" || len(lead) > 60 || strings.Contains(lead, "\n") || !indexable(lead) {
		return
	}
	kind := "entry"
	switch {
	case n.Kind == KOption || strings.HasPrefix(lead, "-"):
		kind = "option"
	case strings.Contains(p.section, "OPTION"):
		kind = "option"
	case strings.Contains(lead, " "):
		kind = "subcommand"
	case strings.Contains(p.section, "METHOD"):
		kind = "method"
	}
	// Tk documents option aliases inline, e.g. "-background or -bg". Anchor on
	// the first spelling and register every spelling so both are findable.
	aliases := []string{lead}
	if strings.Contains(lead, " or ") {
		aliases = nil
		for _, part := range strings.Split(lead, " or ") {
			if part = strings.TrimSpace(part); part != "" {
				aliases = append(aliases, part)
			}
		}
	}
	if len(aliases) == 0 {
		return
	}
	id := p.uniqueID(anchorFor(aliases[0]))
	n.ID = id
	for _, alias := range aliases {
		if p.seen[alias] {
			continue
		}
		p.seen[alias] = true
		p.page.Entries = append(p.page.Entries, Entry{
			Name: alias, Anchor: id, Kind: kind, Context: p.section,
		})
	}
}

// defItem opens a new definition item, closing any previous one and reusing the
// enclosing definition list when there is one.
func (p *parser) defItem(kind Kind) *Node {
	p.flush()
	if p.top().Kind == KDefItem || p.top().Kind == KItem || p.top().Kind == KOption {
		p.stack = p.stack[:len(p.stack)-1]
	}
	if p.top().Kind != KDefList {
		if l := p.top().last(); l != nil && l.Kind == KDefList {
			p.push(l)
		} else {
			p.push(p.top().add(&Node{Kind: KDefList}))
		}
	}
	return p.top().add(&Node{Kind: kind})
}

// --- main loop -------------------------------------------------------------

func (p *parser) parse(src string) *Page {
	lines := strings.Split(src, "\n")
	inDef := false // inside a .de macro definition (inlined man.macros)

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \t\r")

		if inDef {
			if line == ".." {
				inDef = false
			}
			continue
		}
		if line == "" {
			p.endPara()
			continue
		}
		if line[0] != '.' && line[0] != '\'' {
			p.addText(line)
			continue
		}
		if strings.HasPrefix(line, `.\"`) || strings.HasPrefix(line, `'\"`) ||
			strings.HasPrefix(line, `.'`) || line == "." {
			continue
		}
		if strings.HasPrefix(line, ".de") || strings.HasPrefix(line, ".am") {
			inDef = true
			continue
		}

		name, rest := line[1:], ""
		if i := strings.IndexAny(name, " \t"); i >= 0 {
			name, rest = name[:i], strings.TrimLeft(name[i:], " \t")
		}
		if skipMacros[name] {
			continue
		}
		p.macro(name, rest)
	}
	p.flush()
	p.finish()
	return p.page
}

func (p *parser) macro(name, rest string) {
	a := args(rest)
	arg := func(i int) string {
		if i < len(a) {
			return a[i]
		}
		return ""
	}

	switch name {
	case "TH":
		// .TH name... section version group "Manual Title"
		// Seven Tk pages use a multi-word name (".TH tk accessible n"), so the
		// section is located by value rather than by position.
		sec := -1
		for i, t := range a {
			if t == "n" || t == "1" || t == "3" {
				sec = i
				break
			}
		}
		if sec > 0 {
			p.page.Title = plain(strings.Join(a[:sec], " "))
			p.page.Section = a[sec]
		} else {
			p.page.Title = plain(arg(0))
			p.page.Section = plain(arg(1))
		}
		if len(a) > 0 {
			p.page.Manual = plain(a[len(a)-1])
		}
		// The source field names the distribution the page ships in, and sits
		// immediately before the manual title. Counting forward from the
		// section is not safe: math::roman omits the section entirely, and
		// sqlite3 and the Thread pages omit the source, which would otherwise
		// be read as the version or the title. Require a field that can be
		// neither.
		if n := len(a); n >= 4 && (sec < 0 || n-2 >= sec+2) {
			p.page.Source = plain(a[n-2])
		}

	case "SH", "SS":
		p.flush()
		p.capture = nil
		heading := strings.Join(a, " ")
		p.section = plain(heading)
		up := strings.ToUpper(p.section)
		if name == "SH" {
			p.popTo()
		} else {
			p.popTo(KSection)
		}
		// NAME, KEYWORDS and SEE ALSO are metadata, not body content.
		switch {
		case up == "NAME":
			p.capture = &p.nameLn
			return
		case up == "KEYWORDS":
			p.capture = &p.kwLn
			return
		case up == "SEE ALSO":
			p.capture = &p.saLn
			return
		}
		k := KSection
		if name == "SS" {
			k = KSubsection
		}
		n := p.top().add(&Node{Kind: k, Text: p.ins.text(titleize(heading)) + p.ins.reset(),
			ID: p.uniqueID(slug(p.section))})
		p.push(n)

	case "PP", "LP", "P":
		p.flush()
		// .PP returns to the left margin, which ends an .IP bullet list. Without
		// this the remainder of the section is absorbed into the last bullet --
		// snit(n) ended up with 31 paragraphs inside one <li>. .sp is only
		// vertical space and deliberately does not do this.
		if p.top().Kind == KItem {
			p.stack = p.stack[:len(p.stack)-1]
		}
		if p.top().Kind == KBullets {
			p.stack = p.stack[:len(p.stack)-1]
		}

	case "sp":
		p.flush()

	case "br":
		if p.para != nil {
			p.para.WriteString("<br>")
		}

	case "nf", "DS":
		p.flush()
		p.pre = &strings.Builder{}

	case "fi", "DE":
		p.endPre()

	case "CS":
		p.flush()
		p.pre = &strings.Builder{}

	case "CE":
		p.endPre()

	case "RS":
		p.flush()
		p.push(p.top().add(&Node{Kind: KIndent}))

	case "RE":
		p.flush()
		p.popKind(KIndent)

	case "TP":
		n := p.defItem(KDefItem)
		p.awaitTag = n

	case "IP":
		p.flush()
		tag := arg(0)
		if tag == "" {
			// Bare .IP is just a fresh paragraph at the current indent, which
			// the flush above has already begun.
			return
		}
		if strings.Contains(tag, `\(bu`) || tag == "*" {
			// Close the open item before starting the next one. Without this
			// the stack is left inside the previous KItem, whose last child is
			// never a KBullets, so every bullet opened a fresh list inside the
			// one before it -- math(n) reached eighteen levels of indent.
			if p.top().Kind == KItem {
				p.stack = p.stack[:len(p.stack)-1]
			}
			if p.top().Kind != KBullets {
				if l := p.top().last(); l != nil && l.Kind == KBullets {
					p.push(l)
				} else {
					p.push(p.top().add(&Node{Kind: KBullets}))
				}
			}
			p.push(p.top().add(&Node{Kind: KItem}))
			return
		}
		n := p.defItem(KDefItem)
		n.Tag = strings.TrimSpace(p.ins.text(tag) + p.ins.reset())
		p.registerEntry(n)
		p.push(n)

	case "AP":
		// .AP type name in/out  — a C API argument prototype.
		n := p.defItem(KDefItem)
		var sb strings.Builder
		sb.WriteString(p.ins.text(arg(0)))
		if arg(1) != "" {
			sb.WriteString(" " + p.ins.setFont('I') + p.ins.text(arg(1)) + p.ins.setFont('R'))
		}
		if arg(2) != "" {
			sb.WriteString(" <span class=\"io\">(" + p.ins.text(arg(2)) + ")</span>")
		}
		n.Tag = strings.TrimSpace(sb.String() + p.ins.reset())
		p.push(n)

	case "OP":
		// .OP command-line-name database-name database-class
		n := p.defItem(KOption)
		n.Meta = []string{plain(arg(0)), plain(arg(1)), plain(arg(2))}
		tag := plain(arg(0))
		if parts := strings.Split(tag, " or "); len(parts) > 1 {
			for i := range parts {
				parts[i] = "<code>" + strings.TrimSpace(parts[i]) + "</code>"
			}
			n.Tag = strings.Join(parts, " or ")
		} else {
			n.Tag = "<code>" + tag + "</code>"
		}
		p.registerEntry(n)
		p.push(n)

	case "SO":
		p.flush()
		p.popTo()
		ref := plain(arg(0))
		if ref == "" {
			ref = "options"
		}
		n := p.top().add(&Node{Kind: KSection, Text: "Standard Options",
			ID: p.uniqueID("standard-options")})
		p.push(n)
		p.section = "STANDARD OPTIONS"
		n.Meta = []string{ref}
		p.pre = &strings.Builder{}

	case "SE":
		p.endPre()
		ref := "options"
		if p.top().Kind == KSection && len(p.top().Meta) > 0 {
			ref = p.top().Meta[0]
		}
		// The block just closed lists standard option names; tag it so the
		// renderer can turn each one into a link to its definition.
		if l := p.top().last(); l != nil && l.Kind == KPre {
			l.Kind = KStdOpts
			l.Meta = []string{ref}
		}
		p.top().add(&Node{Kind: KNote,
			Text: `See the <a href="` + ref + `.html">` + ref +
				`</a> manual entry for details on the standard options.`})

	case "QW", "PQ", "QR", "MT":
		p.inlineQuote(name, a)

	case "B", "I", "SB", "BI", "IB", "BR", "RB", "IR", "RI":
		// Standard man font macros: alternate the named fonts across arguments.
		p.fontMacro(name, a)

	default:
		// Unknown macro: keep its text so nothing silently vanishes.
		if rest != "" {
			p.addText(rest)
		}
	}
}

func (p *parser) inlineQuote(name string, a []string) {
	at := func(i int) string {
		if i < len(a) {
			return p.ins.text(a[i])
		}
		return ""
	}
	var out string
	switch name {
	case "QW":
		out = "\u201c" + at(0) + "\u201d" + at(1)
	case "PQ":
		out = "(\u201c" + at(0) + "\u201d" + at(1) + ")" + at(2)
	case "QR":
		out = "\u201c" + at(0) + "\u201d-\u201c" + at(1) + "\u201d" + at(2)
	case "MT":
		out = "\u201c\u201d"
	}
	if p.pre != nil {
		p.pre.WriteString(out + "\n")
		return
	}
	if p.awaitTag != nil {
		n := p.awaitTag
		p.awaitTag = nil
		n.Tag = out
		p.push(n)
		return
	}
	if p.para == nil {
		p.para = &strings.Builder{}
	} else {
		p.para.WriteString(" ")
	}
	p.para.WriteString(out)
}

func (p *parser) fontMacro(name string, a []string) {
	seq := map[string]string{
		"B": "B", "I": "I", "SB": "B", "BI": "BI", "IB": "IB",
		"BR": "BR", "RB": "RB", "IR": "IR", "RI": "RI",
	}[name]
	var sb strings.Builder
	for i, tok := range a {
		f := seq[i%len(seq)]
		sb.WriteString(p.ins.setFont(f))
		sb.WriteString(p.ins.text(tok))
	}
	sb.WriteString(p.ins.setFont('R'))
	if p.awaitTag != nil {
		n := p.awaitTag
		p.awaitTag = nil
		n.Tag = strings.TrimSpace(sb.String() + p.ins.reset())
		p.registerEntry(n)
		p.push(n)
		return
	}
	if p.para == nil {
		p.para = &strings.Builder{}
	} else {
		p.para.WriteString(" ")
	}
	p.para.WriteString(sb.String())
}

// finish post-processes the metadata sections captured during parsing.
func (p *parser) finish() {
	// NAME: "cmd, cmd, cmd \- summary"
	nameLine := strings.Join(p.nameLn, " ")
	if i := strings.Index(nameLine, `\-`); i >= 0 {
		names := plain(nameLine[:i])
		p.page.Summary = plain(nameLine[i+2:])
		for _, n := range strings.Split(names, ",") {
			if n = strings.TrimSpace(n); n != "" {
				p.page.Names = append(p.page.Names, n)
			}
		}
	} else if t := plain(nameLine); t != "" {
		p.page.Names = append(p.page.Names, t)
	}
	if p.page.Summary != "" {
		r := []rune(p.page.Summary)
		p.page.Summary = strings.ToUpper(string(r[0])) + string(r[1:])
	}
	if len(p.page.Names) == 0 && p.page.Name != "" {
		p.page.Names = []string{p.page.Name}
	}
	p.page.Keywords = splitList(strings.Join(p.kwLn, " "))
	p.page.SeeAlso = splitList(strings.Join(p.saLn, " "))
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(plain(s), ",") {
		if part = strings.TrimSpace(strings.Trim(part, ".")); part != "" {
			out = append(out, part)
		}
	}
	return out
}
