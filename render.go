package main

import (
	"html"
	"html/template"
	"regexp"
	"sort"
	"strings"
)

// stdOptRe matches an option name in a .SO block.
var stdOptRe = regexp.MustCompile(`-[a-z][a-z0-9]*`)

// renderNode walks the document tree emitting HTML. Definition lists get the
// anchor and self-link that make subcommands addressable.
func renderNode(n *Node, sb *strings.Builder) {
	switch n.Kind {
	case KSection:
		sb.WriteString(`<h2 id="` + n.ID + `">` + n.Text +
			`<a class="anchor" href="#` + n.ID + `" aria-label="Permalink">#</a></h2>` + "\n")
		renderKids(n, sb)
	case KSubsection:
		sb.WriteString(`<h3 id="` + n.ID + `">` + n.Text +
			`<a class="anchor" href="#` + n.ID + `" aria-label="Permalink">#</a></h3>` + "\n")
		renderKids(n, sb)
	case KPara:
		sb.WriteString("<p>" + n.Text + "</p>\n")
	case KNote:
		sb.WriteString(`<p class="note">` + n.Text + "</p>\n")
	case KPre:
		sb.WriteString("<pre><code>" + n.Text + "</code></pre>\n")
	case KStdOpts:
		// Each cell is a standard option name documented elsewhere; linking them
		// turns a dead-end list into navigation.
		//
		// The source lays the names out with tabs against .ta tab stops, which
		// this does not honour, so as preformatted text the columns never lined
		// up -- and the rows are not columns anyway, just however many names
		// fitted a line. Every cell in all 41 blocks of the corpus is a plain
		// option name, so reflow them and let the grid do the aligning.
		ref := "options"
		if len(n.Meta) > 0 && n.Meta[0] != "" {
			ref = n.Meta[0]
		}
		names := stdOptRe.FindAllString(n.Text, -1)
		if len(names) == 0 {
			break
		}
		// Sort: the corpus is inconsistent about it -- ttk::button lists them
		// alphabetically along its rows, text(n) down its columns -- and the
		// block is a set, so the order carries nothing worth preserving.
		seen := map[string]bool{}
		uniq := names[:0]
		for _, o := range names {
			if !seen[o] {
				seen[o] = true
				uniq = append(uniq, o)
			}
		}
		sort.Strings(uniq)
		sb.WriteString(`<ul class="stdopts">` + "\n")
		for _, o := range uniq {
			sb.WriteString(`<li><a href="` + ref + `.html#` + o + `">` + o + "</a></li>\n")
		}
		sb.WriteString("</ul>\n")
	case KDefList:
		sb.WriteString("<dl>\n")
		renderKids(n, sb)
		sb.WriteString("</dl>\n")
	case KDefItem:
		id := ""
		link := ""
		if n.ID != "" {
			id = ` id="` + n.ID + `"`
			link = `<a class="anchor" href="#` + n.ID + `" aria-label="Permalink">#</a>`
		}
		sb.WriteString("<dt" + id + ">" + n.Tag + link + "</dt>\n<dd>\n")
		renderKids(n, sb)
		sb.WriteString("</dd>\n")
	case KOption:
		id, link := "", ""
		if n.ID != "" {
			id = ` id="` + n.ID + `"`
			link = `<a class="anchor" href="#` + n.ID + `" aria-label="Permalink">#</a>`
		}
		sb.WriteString("<dt" + id + ">" + n.Tag + link + "</dt>\n<dd>\n")
		if len(n.Meta) == 3 && n.Meta[1] != "" {
			sb.WriteString(`<table class="opmeta"><tr><th>Database name</th><td><code>` +
				html.EscapeString(n.Meta[1]) + `</code></td></tr><tr><th>Database class</th><td><code>` +
				html.EscapeString(n.Meta[2]) + `</code></td></tr></table>` + "\n")
		}
		renderKids(n, sb)
		sb.WriteString("</dd>\n")
	case KBullets:
		sb.WriteString("<ul>\n")
		renderKids(n, sb)
		sb.WriteString("</ul>\n")
	case KItem:
		sb.WriteString("<li>")
		renderKids(n, sb)
		sb.WriteString("</li>\n")
	case KIndent:
		sb.WriteString(`<div class="indent">` + "\n")
		renderKids(n, sb)
		sb.WriteString("</div>\n")
	default:
		renderKids(n, sb)
	}
}

func renderKids(n *Node, sb *strings.Builder) {
	for _, c := range n.Children {
		renderNode(c, sb)
	}
}

// toc collects the page's headings so every page gets a sidebar outline.
type tocItem struct {
	ID, Text string
	Sub      bool
}

func buildTOC(n *Node) []tocItem {
	var out []tocItem
	var walk func(*Node)
	walk = func(x *Node) {
		for _, c := range x.Children {
			switch c.Kind {
			case KSection:
				out = append(out, tocItem{c.ID, c.Text, false})
			case KSubsection:
				out = append(out, tocItem{c.ID, c.Text, true})
			}
			walk(c)
		}
	}
	walk(n)
	return out
}

type pageView struct {
	Page        *Page
	Body        template.HTML
	TOC         []tocItem
	Subcmds     []Entry
	Options     []Entry
	Defs        []Entry // anchored definitions that are neither subcommand nor option
	DefsLabel   string
	Copyright   []string
	SeeAlso     []seeLink
	Root        string
	HasDemos    bool
	HasKeywords bool
	Keywords    []seeLink // linked into the keyword index
}

type seeLink struct{ Name, URL string }

type indexView struct {
	Title       string
	Manual      string
	Dist        string
	Summary     string // the manual's own description, where it has one
	Groups      []indexGroup
	Root        string
	Count       int
	HasDemos    bool
	HasKeywords bool
	Copyright   []string // pooled from every page in the manual
}

// homeView is the landing page: a search box and a compact list of manuals.
// The manual list lives here and nowhere else — the corpus can run to a couple
// of hundred manuals, which is far more than a masthead or a rail can hold.
type homeView struct {
	Title       string
	Root        string
	Featured    []manualCard // the manuals worth putting in front of a new reader
	Sections    []manualSection
	Manuals     []manualCard // flat, for the totals
	Pages       int
	Entries     int
	HasDemos    bool
	HasKeywords bool
}

// manualSection is one distribution's worth of manuals on the landing page.
type manualSection struct {
	Label   string
	ID      string
	Desc    string
	Manuals []manualCard
}

type manualCard struct {
	Name, URL, Desc, Dist string
	Entries               int
}

// demosView is the shared frame for the demonstration pages: both the index
// and each script get the same rail listing every demo.
type demosView struct {
	Title       string
	Root        string
	HasDemos    bool
	HasKeywords bool
	Programs    []*demo
	Packages    []*demo
}

type demoView struct {
	demosView
	Demo *demo
	Body template.HTML
}

type indexGroup struct {
	Letter  string // display label
	ID      string // fragment-safe form of Letter
	Entries []indexEntry
}

type indexEntry struct {
	Name, URL, Desc string
	Subs            []subLink // subcommands, grouped under their command
}

type subLink struct{ Name, URL string }

// keywordsView is the aggregated keyword index: every .SH KEYWORDS term in the
// corpus, with the pages that claim it. Upstream ships one of these too.
type keywordsView struct {
	Title       string
	Root        string
	HasDemos    bool
	HasKeywords bool
	Count       int
	Pairs       int
	Groups      []keywordGroup
}

type keywordGroup struct {
	Letter  string
	ID      string
	Entries []keywordEntry
}

type keywordEntry struct {
	Name  string
	ID    string
	Pages []subLink
}

func groupByLetter(entries []indexEntry) []indexGroup {
	sort.Slice(entries, func(i, j int) bool {
		a, b := strings.ToLower(entries[i].Name), strings.ToLower(entries[j].Name)
		if a != b {
			return a < b
		}
		return entries[i].Name < entries[j].Name
	})
	var groups []indexGroup
	for _, e := range entries {
		l := strings.ToUpper(e.Name[:1])
		if l < "A" || l > "Z" {
			l = "#"
		}
		if len(groups) == 0 || groups[len(groups)-1].Letter != l {
			id := l
			if l == "#" {
				id = "sym" // "#" is not usable in a URL fragment
			}
			groups = append(groups, indexGroup{Letter: l, ID: id})
		}
		g := &groups[len(groups)-1]
		g.Entries = append(g.Entries, e)
	}
	return groups
}
