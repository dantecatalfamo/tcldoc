// Command tcldoc generates a static documentation site from Tcl/Tk manual page
// sources.
//
//	tcldoc -src ~/tcl-src -out ./site
//
// -src may be a Tcl/Tk source tree (containing tcl*/doc and tk*/doc), or any
// directory of man pages, or repeated for several directories. Output is plain
// files: no server, no database, and no JavaScript outside of search.
package main

import (
	"crypto/sha256"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed templates/site.html
var tmplFS embed.FS

//go:embed assets
var assetFS embed.FS

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	var srcs, demoDirs multiFlag
	flag.Var(&srcs, "src", "source tree or man page directory (repeatable)")
	flag.Var(&demoDirs, "demos", "Tk demonstration directory, e.g. <prefix>/lib/tk9.0/demos (repeatable)")
	out := flag.String("out", "site", "output directory")
	serve := flag.String("serve", "", "after building, serve on this address (e.g. :8080)")
	quiet := flag.Bool("quiet", false, "suppress per-page warnings")
	flag.Parse()

	if len(srcs) == 0 {
		log.Fatal("tcldoc: -src is required")
	}

	files, err := discover(srcs)
	if err != nil {
		log.Fatalf("tcldoc: %v", err)
	}
	if len(files) == 0 {
		log.Fatal("tcldoc: no manual pages found; check -src")
	}
	fmt.Printf("found %d manual pages\n", len(files))

	pages, warnings := parseAll(files)
	pages, collapsed := dedupe(pages)
	if !*quiet {
		for _, w := range warnings {
			fmt.Fprintln(os.Stderr, "warning:", w)
		}
		for _, c := range collapsed {
			fmt.Fprintln(os.Stderr, "collapsed:", c)
		}
	}
	if len(collapsed) > 0 {
		fmt.Printf("collapsed %d groups of duplicate pages\n", len(collapsed))
	}

	site := plan(pages)

	if len(demoDirs) > 0 {
		site.Demos, err = discoverDemos(demoDirs)
		if err != nil {
			log.Fatalf("tcldoc: %v", err)
		}
		fmt.Printf("found %d demonstration scripts\n", len(site.Demos))
	}

	if err := site.write(*out); err != nil {
		log.Fatalf("tcldoc: %v", err)
	}

	if *serve != "" {
		fmt.Printf("serving %s on http://localhost%s\n", *out, *serve)
		log.Fatal(http.ListenAndServe(*serve, http.FileServer(http.Dir(*out))))
	}
}

// --- discovery -------------------------------------------------------------

// pageExt matches a manual page filename. An installed tree suffixes the
// section with the package it came from, so section 3 arrives as ".3tcl" and
// ".3tk" just as section n arrives as ".ntcl" and ".ntk". Spelling only the
// section-n suffixes, as this did, silently dropped the entire C API: 969 of
// the 970 files in man3, plus tclsh.1tcl and wish.1tk.
var pageExt = regexp.MustCompile(`\.(n|1|3)(tcl|tk)?$`)

func discover(roots []string) ([]string, error) {
	seen := map[string]bool{}
	var files []string
	add := func(p string) {
		base := filepath.Base(p)
		if base == "man.macros" || seen[base] {
			return
		}
		seen[base] = true
		files = append(files, p)
	}

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			add(root)
			continue
		}
		// Walk, but only into plausible documentation directories, so pointing
		// -src at a whole source tree does not scan the C sources.
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "tests", "generic", "unix", "win", "macosx", "compat", "library":
					return fs.SkipDir
				}
				return nil
			}
			if pageExt.MatchString(d.Name()) {
				add(p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	return files, nil
}

func parseAll(files []string) ([]*Page, []string) {
	var pages []*Page
	var warnings []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		p := newParser().parse(string(b))
		p.File = f
		// Identity is the source filename. Several Tk pages share a .TH name
		// ("tk print", "tk systray", ...) and would otherwise overwrite one
		// another on output.
		base := filepath.Base(f)
		p.Name = strings.TrimSuffix(base, filepath.Ext(base))
		if p.Title == "" {
			p.Title = p.Name
			warnings = append(warnings, fmt.Sprintf("%s: no .TH", f))
		}
		if p.Manual == "" {
			p.Manual = "Uncategorized"
		}
		if len(p.Names) == 0 {
			warnings = append(warnings, fmt.Sprintf("%s: no NAME section", f))
			p.Names = []string{p.Name}
		}
		pages = append(pages, p)
	}
	return pages, warnings
}

// --- deduplication ---------------------------------------------------------

// fingerprint identifies a page by its content rather than its filename.
func fingerprint(p *Page) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", p.Title, p.Section, p.Manual, p.Summary)
	for _, n := range p.Names {
		fmt.Fprintf(h, "%s\x00", n)
	}
	var body strings.Builder
	collectText(p.Root, &body)
	h.Write([]byte(body.String()))
	return string(h.Sum(nil))
}

// dedupe collapses pages that are copies of one another.
//
// An installed manual tree writes one complete copy of a multi-name page per
// name on its NAME line: library.n arrives 19 times over, as auto_execok.n,
// parray.n, writeFile.n and so on. Left alone every copy is a page in its own
// right, and every copy contributes all 18 of its names to the manual index —
// which is why "writeFile" appeared there 33 times.
//
// The copy whose filename matches its .TH name is the canonical one. No names
// are lost by dropping the others: each copy carries the same NAME line, so the
// survivor still registers every spelling and every anchor.
func dedupe(pages []*Page) ([]*Page, []string) {
	groups := map[string][]*Page{}
	var order []string
	for _, p := range pages {
		f := fingerprint(p)
		if _, seen := groups[f]; !seen {
			order = append(order, f)
		}
		groups[f] = append(groups[f], p)
	}

	var out []*Page
	var notes []string
	for _, f := range order {
		g := groups[f]
		best := g[0] // discovery order is sorted, so this is deterministic
		for _, p := range g {
			// Compare slugs, not raw names: an installed page for the .TH name
			// "pkg::create" is filed as pkg_create.n.
			if strings.EqualFold(slugFile(p.Name), slugFile(p.Title)) {
				best = p
				break
			}
		}
		if len(g) > 1 {
			notes = append(notes, fmt.Sprintf("%d copies of %s -> %s",
				len(g), best.Title, filepath.Base(best.File)))
		}
		out = append(out, best)
	}
	return out, notes
}

// --- planning --------------------------------------------------------------

type manual struct {
	Name   string
	Slug   string
	Source string // dominant .TH source field across the manual's pages
	Family string
	Pages  []*Page
}

type site struct {
	Pages   []*Page
	Manuals []*manual
	Demos   []*demo
	urlFor  map[string]string // command name -> relative URL (with anchor)
	pageURL map[*Page]string
}

// manualOverride renames a manual whose .TH title is not a useful name for it.
// Each override is guarded on the manual's contents, so a corpus where the same
// title covers more than this is left alone.
func manualOverride(m *manual) string {
	// sqlite3's page carries a short .TH with no source field, so the generic
	// "Tcl-Extensions" is read as its manual title. Where that page is the only
	// one filed under it, the package it documents is the better name.
	if m.Name == "Tcl-Extensions" && len(m.Pages) == 1 {
		return m.Pages[0].Title
	}
	return m.Name
}

// normalizeManual folds the inconsistent .TH titles found upstream.
func normalizeManual(m string) string {
	m = strings.ReplaceAll(m, "Built-in", "Built-In")
	// The tepam pages spell the same manual title both ways, which would
	// otherwise split it into two manuals on the landing page.
	m = strings.ReplaceAll(m, "’", "'")
	if strings.Contains(m, "Float ") || strings.Contains(m, "Integer Division") {
		return "Tcl Mathematical Functions"
	}
	return m
}

func plan(pages []*Page) *site {
	s := &site{urlFor: map[string]string{}, pageURL: map[*Page]string{}}
	byManual := map[string]*manual{}

	for _, p := range pages {
		p.Manual = normalizeManual(p.Manual)
		m := byManual[p.Manual]
		if m == nil {
			m = &manual{Name: p.Manual, Slug: slug(p.Manual)}
			byManual[p.Manual] = m
			s.Manuals = append(s.Manuals, m)
		}
		m.Pages = append(m.Pages, p)
	}

	// Rename where the .TH title is not a useful name for the manual. This runs
	// after grouping because the overrides are guarded on what a manual turned
	// out to contain.
	for _, m := range s.Manuals {
		if name := manualOverride(m); name != m.Name {
			m.Name, m.Slug = name, slug(name)
			for _, p := range m.Pages {
				p.Manual = name // the breadcrumb reads from the page
			}
		}
	}
	sort.Slice(s.Manuals, func(i, j int) bool { return s.Manuals[i].Name < s.Manuals[j].Name })

	// A manual claims a distribution only when its pages agree on one. Upstream
	// files nine separate packages under the single title "Tcl Bundled
	// Packages" -- http, msgcat, dde, registry and the rest each name
	// themselves -- and picking whichever has the most pages would label the
	// whole manual "http". The family is still unambiguous there, so group by
	// that and leave the manual itself unattributed; each page keeps its own.
	for _, m := range s.Manuals {
		fams := map[string]int{}
		secs := map[string]int{}
		src, mixed := "", false
		for _, p := range m.Pages {
			secs[p.Section]++
			// A page with a short .TH carries no source at all. That is missing
			// information, not disagreement, so it must not unattribute the
			// whole manual -- one such page in Tcl Math Library would otherwise
			// strip the label off all twenty-six.
			if p.Source == "" {
				continue
			}
			fams[distFamily(p.Source)]++
			if src == "" {
				src = p.Source
			} else if p.Source != src {
				mixed = true
			}
		}
		if !mixed {
			m.Source = src
		}
		m.Family = "Unattributed"
		if len(fams) == 0 {
			// No page names a distribution. Fall back to the manual's own
			// title, which is what the .TH says for Tcl Threading and
			// Tcl-Extensions -- the two whose pages omit the source field.
			m.Family = distFamily(m.Name)
		}
		keys := make([]string, 0, len(fams))
		for f := range fams {
			keys = append(keys, f)
		}
		sort.Strings(keys) // deterministic tie-break
		for _, f := range keys {
			if fams[f] > fams[m.Family] {
				m.Family = f
			}
		}

		// Section 3 is the C API. Tcl, Tk and TclOO each ship one, and they are
		// a different kind of reference from the command pages, so they group
		// together rather than under the distribution they came from. Judged on
		// the dominant section: Tk Themed Widget has eighteen section-3 pages
		// among forty-one and is not a C API manual.
		for sec, n := range secs {
			if sec == "3" && n*2 > len(m.Pages) {
				m.Family = "C API"
			}
		}
	}

	for _, m := range s.Manuals {
		sort.Slice(m.Pages, func(i, j int) bool { return m.Pages[i].Name < m.Pages[j].Name })

		taken := map[string]*Page{}
		for _, p := range m.Pages {
			file := slugFile(p.Name)
			if prev, dup := taken[strings.ToLower(file)]; dup {
				// Never silently overwrite: make the collision visible and fix it.
				fmt.Fprintf(os.Stderr,
					"warning: %s and %s both map to %s.html; disambiguating\n",
					prev.File, p.File, file)
				file += "-" + slugFile(p.Section)
			}
			taken[strings.ToLower(file)] = p
			url := m.Slug + "/" + file + ".html"
			s.pageURL[p] = url
			for _, n := range p.Names {
				if _, dup := s.urlFor[n]; !dup {
					s.urlFor[n] = url
				}
			}
			for _, e := range p.Entries {
				if _, dup := s.urlFor[e.Name]; !dup {
					s.urlFor[e.Name] = url + "#" + e.Anchor
				}
			}
		}
	}
	s.Pages = pages
	return s
}

var fileBad = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// slugFile keeps page filenames recognisable (ttk::button -> ttk_button) while
// staying safe on case-insensitive filesystems.
func slugFile(name string) string {
	return strings.Trim(fileBad.ReplaceAllString(name, "_"), "_")
}

// --- rendering -------------------------------------------------------------

// markOptional tints Tcl's ?optional? question marks without disturbing markup.
func markOptional(h string) string {
	var b strings.Builder
	depth := 0
	for i := 0; i < len(h); i++ {
		switch c := h[i]; c {
		case '<':
			depth++
			b.WriteByte(c)
		case '>':
			if depth > 0 {
				depth--
			}
			b.WriteByte(c)
		case '?':
			if depth == 0 {
				b.WriteString(`<span class="opt">?</span>`)
			} else {
				b.WriteByte(c)
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// --- the featured manuals --------------------------------------------------

// featuredManuals names, in order, the manuals worth putting in front of
// someone arriving at a Tcl/Tk reference.
//
// This is a list rather than a ranking because relevance is a judgement that
// entry counts cannot make. Ranked by size, the top of a tcllib-bearing corpus
// is doctools (523 entries), the parser generator (502) and the tcllib maths
// packages (658) -- among the largest manuals present and among the least
// likely to be why anyone opened the site -- while ttk, the modern widget set,
// does not make the cut at 200.
var featuredManuals = []string{
	"Tcl Built-In Commands",
	"Tk Built-In Commands",
	"Tk Themed Widget",
	"TclOO Commands",
	"Tcl Bundled Packages",
	"Tcl Database Connectivity",
	"Tcl Threading",
	"Tcl Math Library",
}

const featuredCount = 8

// pickFeatured takes the named manuals that this corpus actually has, in the
// order given, and tops the list up by size. A corpus none of whose manuals are
// recognised therefore still gets a sensible band rather than an empty one.
// Below a certain size the whole list is scannable and the band is just noise.
func pickFeatured(all []manualCard) []manualCard {
	if len(all) <= 2*featuredCount {
		return nil
	}
	byName := make(map[string]manualCard, len(all))
	for _, m := range all {
		byName[m.Name] = m
	}
	var out []manualCard
	taken := map[string]bool{}
	for _, name := range featuredManuals {
		if m, ok := byName[name]; ok && len(out) < featuredCount {
			out = append(out, m)
			taken[name] = true
		}
	}
	if len(out) < featuredCount {
		bySize := append([]manualCard(nil), all...)
		sort.Slice(bySize, func(i, j int) bool {
			if bySize[i].Entries != bySize[j].Entries {
				return bySize[i].Entries > bySize[j].Entries
			}
			return bySize[i].Name < bySize[j].Name
		})
		for _, m := range bySize {
			if len(out) == featuredCount {
				break
			}
			if !taken[m.Name] {
				out = append(out, m)
				taken[m.Name] = true
			}
		}
	}
	return out
}

// --- the keyword index -----------------------------------------------------

// buildKeywords pools every page's .SH KEYWORDS terms into one A-Z index. The
// keywords are already parsed and shown per page; this is the aggregation the
// official site has under Keywords/ and this did not.
func buildKeywords(s *site) keywordsView {
	pagesFor := map[string][]subLink{}
	seen := map[string]bool{} // keyword + page, so a repeat on one page is not two links
	for _, m := range s.Manuals {
		for _, p := range m.Pages {
			for _, k := range p.Keywords {
				key := strings.ToLower(k)
				if pair := key + "\x00" + p.Title; seen[pair] {
					continue
				} else {
					seen[pair] = true
				}
				pagesFor[key] = append(pagesFor[key], subLink{Name: p.Title, URL: s.pageURL[p]})
			}
		}
	}

	var view keywordsView
	keys := make([]string, 0, len(pagesFor))
	for k := range pagesFor {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ids := map[string]int{}
	for _, k := range keys {
		pages := pagesFor[k]
		sort.Slice(pages, func(i, j int) bool { return pages[i].Name < pages[j].Name })

		letter := strings.ToUpper(k[:1])
		if letter < "A" || letter > "Z" {
			letter = "#"
		}
		if n := len(view.Groups); n == 0 || view.Groups[n-1].Letter != letter {
			id := letter
			if letter == "#" {
				id = "sym"
			}
			view.Groups = append(view.Groups, keywordGroup{Letter: letter, ID: id})
		}
		// Anchor per keyword, so a page's keyword can link into this index.
		base := slug(k)
		ids[base]++
		if n := ids[base]; n > 1 {
			base += "-" + strconv.Itoa(n)
		}
		g := &view.Groups[len(view.Groups)-1]
		g.Entries = append(g.Entries, keywordEntry{Name: k, ID: base, Pages: pages})
		view.Count++
		view.Pairs += len(pages)
	}
	return view
}

// --- distributions ---------------------------------------------------------

// The .TH source field names the distribution a page ships in, but at a finer
// grain than is useful for browsing: Tcl's own bundled packages each name
// themselves ("msgcat", "registry", "dde"). These two tables fold the known
// Tcl ecosystem into the three families a reader actually distinguishes.
var (
	coreSource = map[string]bool{
		"Tcl": true, "Tk": true, "TclOO": true, "Zipfs": true, "Tcl-Extensions": true,
	}
	bundledSource = map[string]bool{
		"itcl": true, "itk": true, "Thread": true, "Tcl Threading": true,
		"http": true, "msgcat": true, "platform": true, "platform::shell": true,
		"dde": true, "registry": true, "tcltest": true, "cookiejar": true,
		"tdbc": true, "sqlite3": true, "tcl_idna": true,
	}
)

// distFamily buckets a .TH source field. An unrecognised source keeps its own
// name rather than being guessed into a family, so a corpus carrying something
// this does not know about (tklib, a vendor's own packages) still groups.
func distFamily(source string) string {
	switch {
	case source == "":
		return "Unattributed"
	case coreSource[source]:
		return "Tcl and Tk"
	case strings.EqualFold(source, "tcllib"):
		return "tcllib"
	case bundledSource[source]:
		return "Bundled packages"
	}
	return source
}

// distRank orders the families: the core commands first, then the C API, then
// what ships alongside them, then the standard library, then anything
// unrecognised, with the unattributed last.
func distRank(family string) int {
	switch family {
	case "Tcl and Tk":
		return 0
	case "C API":
		return 1
	case "Bundled packages":
		return 2
	case "tcllib":
		return 3
	case "Unattributed":
		return 5
	}
	return 4
}

// genericHeading are section titles that say nothing about what the section
// holds, so they make useless sidebar labels.
var genericHeading = map[string]bool{
	"": true, "DESCRIPTION": true, "SYNOPSIS": true, "INTRODUCTION": true,
	"NAME": true, "OVERVIEW": true, "NOTES": true,
}

// defsLabel names the sidebar group for a page's plain definitions. thread(n)
// files its under "COMMANDS" and math::geometry files its under "PROCEDURES",
// which beats a generic label; where the enclosing section says nothing useful,
// or the definitions are spread across several sections, fall back.
func defsLabel(defs []Entry) string {
	ctx := ""
	for i, e := range defs {
		if i > 0 && e.Context != ctx {
			return "Definitions"
		}
		ctx = e.Context
	}
	if genericHeading[strings.ToUpper(strings.TrimSpace(ctx))] {
		return "Definitions"
	}
	return titleize(ctx)
}

// commandLike reports whether a multi-word definition label is a command
// invocation rather than prose. `.TP` doubles as this corpus's
// paragraph-with-a-title macro, so section-like labels arrive looking exactly
// like "string cat" does: "Atomic Parsing Expressions" in the parser tools,
// "Torn-off Menus" in menu(n), "Interpreters Passed As Arguments" in the C API.
//
// This decides the entry's kind, so nothing is dropped by it — the label keeps
// its anchor, its place in the page's sidebar and its search entry. It just is
// not called a subcommand, and so does not reach an index of commands.
func commandLike(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'a' && c <= 'z' || c == '$' || c == ':' || c == '_'
}

// subLabel shortens a subcommand for display beneath its parent: "string cat"
// under "string" reads as just "cat". A name that does not start with one of
// the page's own names is left whole, so "tsv::array get" stays legible.
func subLabel(name string, pageNames []string) string {
	best := ""
	for _, n := range pageNames {
		if len(n) > len(best) && strings.HasPrefix(name, n+" ") {
			best = n
		}
	}
	if best == "" {
		return name
	}
	return strings.TrimSpace(name[len(best):])
}

// commonSubPrefix returns the leading tokens that every label in the group
// shares. The last token is never stripped, so a label can not be emptied.
func commonSubPrefix(subs []subLink) string {
	if len(subs) < 2 {
		return ""
	}
	first := strings.Split(subs[0].Name, " ")
	n := len(first) - 1
	for _, s := range subs[1:] {
		t := strings.Split(s.Name, " ")
		if len(t)-1 < n {
			n = len(t) - 1
		}
		for i := 0; i < n; i++ {
			if t[i] != first[i] {
				n = i
				break
			}
		}
		if n <= 0 {
			return ""
		}
	}
	return strings.Join(first[:n], " ") + " "
}

var (
	creditRe = regexp.MustCompile(
		`^Copyright ©\s*(\d{4}(?:\s*[-–]\s*\d{4})?(?:\s*,\s*\d{4}(?:\s*[-–]\s*\d{4})?)*)\s+(.+)$`)
	yearRe     = regexp.MustCompile(`\d{4}`)
	reservedRe = regexp.MustCompile(`(?i)[.,]?\s*all rights reserved\.?$`)
)

// groupCredits pools a manual's attributions by holder, coalescing the years
// into a single span, which is the shape upstream's contents pages use.
//
// Pooled across a manual the verbatim list is mostly the same few names over
// and over: Tcl Built-In Commands has 54 lines naming 22 holders, six of them
// the Regents and seven of them Sun. Individual pages keep their lines exactly
// as written — this only applies where the pooling is what makes them repeat.
//
// Anything that does not parse is carried through untouched rather than
// dropped. "All rights reserved" is kept only where every one of a holder's
// lines carries it: upstream discards it outright, but carrying it over from a
// single line would assert it across a merged span that no source claims —
// Donal K. Fellows wrote it once, in 2006, out of fourteen lines.
func groupCredits(lines []string) []string {
	type credit struct {
		min, max int
		lines    int
		reserved bool
	}
	byHolder := map[string]*credit{}
	var order, verbatim []string

	for _, l := range lines {
		m := creditRe.FindStringSubmatch(l)
		if m == nil {
			verbatim = append(verbatim, l)
			continue
		}
		name := strings.TrimSpace(reservedRe.ReplaceAllString(m[2], ""))
		reserved := name != strings.TrimSpace(m[2])
		if name == "" {
			verbatim = append(verbatim, l)
			continue
		}
		c := byHolder[name]
		if c == nil {
			c = &credit{min: 1 << 30}
			byHolder[name] = c
			order = append(order, name)
		}
		if c.lines == 0 {
			c.reserved = reserved
		} else {
			c.reserved = c.reserved && reserved
		}
		c.lines++
		for _, y := range yearRe.FindAllString(m[1], -1) {
			n, _ := strconv.Atoi(y)
			if n < c.min {
				c.min = n
			}
			if n > c.max {
				c.max = n
			}
		}
	}

	sort.Slice(order, func(i, j int) bool {
		a, b := byHolder[order[i]], byHolder[order[j]]
		if a.min != b.min {
			return a.min < b.min
		}
		return order[i] < order[j]
	})

	out := make([]string, 0, len(order)+len(verbatim))
	for _, name := range order {
		c := byHolder[name]
		span := strconv.Itoa(c.min)
		if c.max != c.min {
			span += "-" + strconv.Itoa(c.max)
		}
		s := "Copyright © " + span + " " + name
		if c.reserved {
			s += ". All rights reserved"
		}
		out = append(out, s)
	}
	return append(out, verbatim...)
}

// dedupeStrings keeps the first occurrence of each value, order preserved.
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// plural formats a count with the right noun, so the site never says "1 pages".
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (s *site) write(outDir string) error {
	tmpl, err := template.New("site").
		Funcs(template.FuncMap{"plural": plural}).
		ParseFS(tmplFS, "templates/site.html")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// Static assets.
	assets, _ := fs.Sub(assetFS, "assets")
	err = fs.WalkDir(assets, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(outDir, p), b, 0o644)
	})
	if err != nil {
		return err
	}

	ix := NewIndexer()

	// Built before the pages, so each page's keywords can link into it, and
	// because the masthead needs to know whether it exists.
	kw := buildKeywords(s)
	hasKeywords := len(kw.Groups) > 0
	kwAnchor := map[string]string{}
	for _, g := range kw.Groups {
		for _, e := range g.Entries {
			kwAnchor[e.Name] = e.ID
		}
	}

	// Documentation pages.
	for _, m := range s.Manuals {
		for _, p := range m.Pages {
			url := s.pageURL[p]
			var body strings.Builder
			renderKids(p.Root, &body)

			// Every anchored definition belongs in the sidebar, including the
			// ones that fall into the generic bucket: thread(n) and tclvars(n)
			// consist entirely of those, and used to get no sidebar at all.
			// No cap — the rail is its own scroll container.
			var subs, opts, defs []Entry
			for _, e := range p.Entries {
				if e.Anchor == "" {
					continue
				}
				switch e.Kind {
				case "subcommand", "method":
					subs = append(subs, e)
				case "option":
					opts = append(opts, e)
				default:
					defs = append(defs, e)
				}
			}

			var see []seeLink
			for _, name := range p.SeeAlso {
				if u, ok := s.urlFor[name]; ok {
					see = append(see, seeLink{Name: name, URL: "../" + u})
				} else {
					see = append(see, seeLink{Name: name, URL: ""})
				}
			}
			see = filterLinked(see)

			var also []string
			for _, n := range p.Names {
				if n != p.Title {
					also = append(also, n)
				}
			}
			// The count must come from `also`, not p.Names: p.Title is only
			// sometimes one of the names, so p.Names is not a reliable total.
			if len(also) > 12 {
				also = append(also[:12:12], fmt.Sprintf("and %d more", len(also)-12))
			}

			var kwLinks []seeLink
			for _, k := range p.Keywords {
				if id := kwAnchor[strings.ToLower(k)]; id != "" {
					kwLinks = append(kwLinks, seeLink{Name: k, URL: "keywords/#k-" + id})
				} else {
					kwLinks = append(kwLinks, seeLink{Name: k})
				}
			}

			toc := buildTOC(p.Root)
			// tcllib's pages, via doctools, already carry a COPYRIGHT section in
			// the body. Printing the header attribution as well would say the
			// same thing twice under two identical headings.
			credits := dedupeStrings(p.Copyright)
			for _, t := range toc {
				if strings.EqualFold(t.Text, "Copyright") {
					credits = nil
					break
				}
			}

			view := struct {
				pageView
				Title     string
				ManualURL string
				Dist      string
				AlsoNames []string
			}{
				pageView: pageView{
					Page:        p,
					Body:        template.HTML(markOptional(body.String())),
					TOC:         toc,
					Copyright:   credits,
					Subcmds:     subs,
					Options:     opts,
					Defs:        defs,
					DefsLabel:   defsLabel(defs),
					SeeAlso:     see,
					Root:        "..",
					HasDemos:    len(s.Demos) > 0,
					HasKeywords: hasKeywords,
					Keywords:    kwLinks,
				},
				Title:     p.Title + " \u2014 " + p.Manual,
				ManualURL: m.Slug + "/",
				Dist:      p.Source,
				AlsoNames: also,
			}
			if err := renderTo(tmpl, "page", filepath.Join(outDir, url), view); err != nil {
				return err
			}
			ix.AddPage(p, url)
		}

		// Per-manual index, including subcommands and options.
		var entries []indexEntry
		for _, p := range m.Pages {
			url := s.pageURL[p]
			anchor := map[string]string{}
			for _, e := range p.Entries {
				if _, dup := anchor[e.Name]; !dup && e.Name != p.Title {
					anchor[e.Name] = e.Anchor
				}
			}
			// Every name on the NAME line, linked to its own definition where
			// the page has one rather than to the top of the page.
			named := map[string]bool{}
			primary := len(entries) // the row the page's subcommands hang off
			for i, n := range p.Names {
				named[n] = true
				if n == p.Title {
					primary = len(entries) + i
				}
				u := url
				if a, ok := anchor[n]; ok {
					u = url + "#" + a
				}
				entries = append(entries, indexEntry{Name: n, URL: u, Desc: p.Summary})
			}
			// Subcommands and methods hang off the command they belong to
			// instead of taking a row each: "string cat" becomes a link
			// labelled "cat" under "string", not a second entry under S.
			var subs []subLink
			for _, e := range p.Entries {
				if e.Name == p.Title || named[e.Name] {
					continue
				}
				if e.Kind != "subcommand" && e.Kind != "method" {
					continue
				}
				subs = append(subs, subLink{
					Name: subLabel(e.Name, p.Names), URL: url + "#" + e.Anchor,
				})
			}
			// Where every subcommand shares a qualifier that is not one of the
			// page's own names -- "::tcl::build-info clang", "... commit" --
			// strip that too, so the run reads the way string's does.
			if pre := commonSubPrefix(subs); pre != "" {
				for i := range subs {
					subs[i].Name = strings.TrimPrefix(subs[i].Name, pre)
				}
			}
			if len(subs) > 0 {
				entries[primary].Subs = subs
			}
		}
		// A manual that is really just one page is described by that page's own
		// summary better than by anything generic. Multi-page manuals have no
		// such line, and get none rather than boilerplate.
		summary := ""
		if len(m.Pages) == 1 && !strings.EqualFold(m.Pages[0].Summary, m.Name) {
			summary = m.Pages[0].Summary
		}
		// Attribution, pooled across the manual the way upstream's contents
		// pages do it: each page credits only its own authors, so the manual's
		// full credit is the union of them.
		var credits []string
		for _, p := range m.Pages {
			credits = append(credits, p.Copyright...)
		}
		credits = groupCredits(dedupeStrings(credits))

		iv := indexView{
			Title: m.Name, Manual: m.Name, Dist: m.Source, Summary: summary,
			Groups: groupByLetter(entries), Root: "..", Count: len(entries),
			HasDemos: len(s.Demos) > 0, HasKeywords: hasKeywords, Copyright: credits,
		}
		if err := renderTo(tmpl, "index", filepath.Join(outDir, m.Slug, "index.html"), iv); err != nil {
			return err
		}
	}

	if hasKeywords {
		kw.Title = "Keywords"
		kw.Root = ".."
		kw.HasDemos = len(s.Demos) > 0
		kw.HasKeywords = true
		if err := renderTo(tmpl, "keywords", filepath.Join(outDir, "keywords", "index.html"), kw); err != nil {
			return err
		}
		fmt.Printf("keyword index: %d keywords, %d page references\n", kw.Count, kw.Pairs)
	}

	// Landing page.
	home := homeView{
		Title: "Tcl/Tk reference", Root: ".",
		HasDemos: len(s.Demos) > 0, HasKeywords: hasKeywords,
	}
	for _, m := range s.Manuals {
		n := 0
		for _, p := range m.Pages {
			n += len(p.Names) + len(p.Entries)
		}
		home.Manuals = append(home.Manuals, manualCard{
			Name: m.Name, URL: m.Slug + "/", Entries: n, Dist: m.Source,
			Desc: plural(len(m.Pages), "page", "pages") + ", " + plural(n, "entry", "entries"),
		})
		home.Pages += len(m.Pages)
		home.Entries += n
	}

	// Group by distribution: with tcllib bundled into the source tree, nine
	// manuals in ten come from it, which a flat A-Z list hides completely.
	pagesIn := map[string]int{}
	byFamily := map[string][]manualCard{}
	for i, m := range s.Manuals {
		byFamily[m.Family] = append(byFamily[m.Family], home.Manuals[i])
		pagesIn[m.Family] += len(m.Pages)
	}
	for fam, cards := range byFamily {
		home.Sections = append(home.Sections, manualSection{
			Label: fam, ID: slug(fam), Manuals: cards,
			Desc: plural(len(cards), "manual", "manuals") + ", " +
				plural(pagesIn[fam], "page", "pages"),
		})
	}
	sort.Slice(home.Sections, func(i, j int) bool {
		a, b := home.Sections[i], home.Sections[j]
		if ra, rb := distRank(a.Label), distRank(b.Label); ra != rb {
			return ra < rb
		}
		return a.Label < b.Label
	})

	home.Featured = pickFeatured(home.Manuals)

	// Demonstration scripts, if any were given. They are not manual pages, so
	// they get their own section rather than a place in the distribution list.
	if len(s.Demos) > 0 {
		dv := demosView{Title: "Tk demonstrations", Root: "..", HasDemos: true, HasKeywords: hasKeywords}
		for _, d := range s.Demos {
			if d.Program {
				dv.Programs = append(dv.Programs, d)
			} else {
				dv.Packages = append(dv.Packages, d)
			}
		}
		if err := renderTo(tmpl, "demos", filepath.Join(outDir, "demos", "index.html"), dv); err != nil {
			return err
		}
		for _, d := range s.Demos {
			view := demoView{demosView: dv, Demo: d, Body: template.HTML(escapeSource(d.Source))}
			view.Title = d.Name + " — Tk demonstrations"
			if err := renderTo(tmpl, "demo", filepath.Join(outDir, filepath.FromSlash(d.URL)), view); err != nil {
				return err
			}
			ix.AddDemo(d)
		}
	}

	if err := renderTo(tmpl, "home", filepath.Join(outDir, "index.html"), home); err != nil {
		return err
	}

	stats, err := ix.Write(outDir)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d pages across %d manuals\n", len(s.Pages), len(s.Manuals))
	fmt.Printf("search index: %d names, %d docs, %d terms in %d shards\n",
		stats["names"], stats["docs"], stats["terms"], stats["shards"])
	return nil
}

func filterLinked(in []seeLink) []seeLink {
	var out []seeLink
	for _, l := range in {
		if l.URL != "" {
			out = append(out, l)
		}
	}
	return out
}

func renderTo(t *template.Template, name, path string, data any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return t.ExecuteTemplate(f, name, data)
}
