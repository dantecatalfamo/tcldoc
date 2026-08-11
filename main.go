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
	"time"
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
	version := flag.String("version", "", `version label for the site, e.g. "Tcl/Tk 9.0.3"`)
	serve := flag.String("serve", "", "after building, serve on this address (e.g. :8080)")
	quiet := flag.Bool("quiet", false, "suppress per-page warnings")
	var licenses multiFlag
	flag.Var(&licenses, "license", `license file to reproduce verbatim, "Name=path" or a bare path (repeatable); a license.terms near each -src is found automatically`)
	xref := flag.Bool("xref", false, "prototype: auto-link emboldened command names in prose")
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
	site.Version = *version
	if site.Version == "" {
		site.Version = inferVersion(pages)
	}
	if site.Version != "" {
		fmt.Printf("version: %s\n", site.Version)
	}

	if len(demoDirs) > 0 {
		site.Demos, err = discoverDemos(demoDirs)
		if err != nil {
			log.Fatalf("tcldoc: %v", err)
		}
		fmt.Printf("found %d demonstration scripts\n", len(site.Demos))
	}

	site.Licenses = collectLicenses(srcs, licenses)
	if len(site.Licenses) > 0 {
		fmt.Printf("license: %d distinct license file(s)\n", len(site.Licenses))
	}
	site.Repos = collectRepos(pages)
	site.Xref = *xref

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

// --- licenses --------------------------------------------------------------

// licenseDoc is one distribution's verbatim license text. The Tcl license --
// which Tcl, Tk, the Tk demos and tcllib all ship -- grants the right to
// distribute the documentation "provided that ... this notice is included
// verbatim in any distributions". This site is such a distribution, so the
// text is reproduced on a /license/ page.
type licenseDoc struct {
	Title string
	ID    string // fragment-safe, unique within the page
	Text  string
}

// collectLicenses gathers license files from the -license flags (explicit, and
// their names win) and by discovery next to each -src, de-duplicated by content
// so the single Tcl license the whole corpus shares is printed once.
func collectLicenses(srcs, flags multiFlag) []licenseDoc {
	seen := map[string]int{} // content key -> index in out
	var out []licenseDoc
	add := func(title, path string) {
		b, err := os.ReadFile(path)
		if err != nil {
			return
		}
		text := strings.TrimRight(string(b), "\n")
		trimmed := strings.TrimSpace(text)
		// Skip empties and critcl's "<<Undefined>>" placeholder stubs.
		if len(trimmed) < 40 || strings.Contains(trimmed, "<<Undefined>>") {
			return
		}
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(trimmed)))
		if i, ok := seen[key]; ok {
			// Same text already collected. An explicit -license name supersedes a
			// title derived from discovery.
			if title != "" {
				out[i].Title = title
			}
			return
		}
		if title == "" {
			if strings.Contains(text, "The following terms apply to all files") {
				title = "Tcl/Tk License"
			} else {
				title = "License terms"
			}
		}
		id, base, n := slug(title), slug(title), 2
		for {
			clash := false
			for _, e := range out {
				if e.ID == id {
					clash = true
					break
				}
			}
			if !clash {
				break
			}
			id = base + "-" + strconv.Itoa(n)
			n++
		}
		seen[key] = len(out)
		out = append(out, licenseDoc{Title: title, ID: id, Text: text})
	}

	// Discovery first, so the core licenses next to -src lead the page; explicit
	// -license entries follow (and their names win on any duplicate).
	for _, root := range srcs {
		for _, path := range licenseCandidates(root) {
			add("", path)
		}
	}
	for _, f := range flags {
		name, path := "", f
		if i := strings.IndexByte(f, '='); i >= 0 {
			name, path = strings.TrimSpace(f[:i]), f[i+1:]
		}
		add(name, path)
	}
	return out
}

// licenseCandidates lists likely license.terms paths for one -src: the nearest
// one at or above the source directory -- a Homebrew bottle keeps it at the
// prefix, above share/man -- plus one under each immediate child directory, the
// way a source tree keeps a license.terms under each of tcl*/ and tk*/.
func licenseCandidates(root string) []string {
	info, err := os.Stat(root)
	if err != nil {
		return nil
	}
	dir := root
	if !info.IsDir() {
		dir = filepath.Dir(root)
	}
	var cands []string
	for d, i := dir, 0; i < 5; i++ {
		p := filepath.Join(d, "license.terms")
		if fileExists(p) {
			cands = append(cands, p)
			break
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			if p := filepath.Join(dir, e.Name(), "license.terms"); fileExists(p) {
				cands = append(cands, p)
			}
		}
	}
	return cands
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
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

// --- version ---------------------------------------------------------------

var versionNum = regexp.MustCompile(`^\d+(\.\d+)*$`)

// inferVersion reads the release out of the pages themselves.
//
// .TH's version field is archaeological: it records when each command was
// introduced, so one Tcl tree carries everything from 7.0 to 9.0. The newest
// value a distribution carries is nonetheless the release it was built from,
// and Tcl and Tk agree on it. Only the series, though — "9.0.3" appears nowhere
// in the corpus, so naming a patch level still needs -version.
//
// Read from Tcl and Tk only: a tcllib page's version is its own package's.
func inferVersion(pages []*Page) string {
	newest := map[string][]int{}
	for _, p := range pages {
		if p.Source != "Tcl" && p.Source != "Tk" || !versionNum.MatchString(p.Version) {
			continue
		}
		v := versionParts(p.Version)
		if compareVersions(v, newest[p.Source]) > 0 {
			newest[p.Source] = v
		}
	}
	tcl, tk := versionString(newest["Tcl"]), versionString(newest["Tk"])
	switch {
	case tcl != "" && tcl == tk:
		return "Tcl/Tk " + tcl
	case tcl != "" && tk != "":
		return "Tcl " + tcl + " / Tk " + tk
	case tcl != "":
		return "Tcl " + tcl
	case tk != "":
		return "Tk " + tk
	}
	return ""
}

func versionParts(v string) []int {
	var out []int
	for _, f := range strings.Split(v, ".") {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func versionString(v []int) string {
	var b []string
	for _, n := range v {
		b = append(b, strconv.Itoa(n))
	}
	return strings.Join(b, ".")
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
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
	Version     string // shown on every page; -version, since .TH cannot supply it
	Pages       []*Page
	Manuals     []*manual
	Demos       []*demo
	Licenses    []licenseDoc      // verbatim license text(s), reproduced on /license/
	Repos       []repo            // upstream repositories, linked from the footer
	Xref        bool              // prototype: auto-link command names in prose
	urlFor      map[string]string // command name -> relative URL (with anchor)
	linkTargets map[string]string // name -> URL, only when one page/entry owns it
	pageURL     map[*Page]string
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
	// tclsh and wish declare separate manuals of one page each. They are the
	// same thing -- the interpreters you invoke -- and upstream files them
	// together under this name.
	if m == "Tcl Applications" || m == "Tk Applications" {
		return "Tcl/Tk Applications"
	}
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
	// linkTargets underpins -xref: a name is a safe link target only when exactly
	// one thing in the corpus -- one page, or one entry (subcommand, method) --
	// carries it. So "chan configure", which just one page defines, links to its
	// definition; but "configure" (a method on forty-one pages) and "interp" (the
	// core command and a tcllib package both) are ambiguous and left alone.
	counts := map[string]int{}
	nameURL := map[string]string{}
	for _, p := range pages {
		u := s.pageURL[p]
		seen := map[string]bool{}
		record := func(name, url string) {
			if seen[name] {
				return
			}
			seen[name] = true
			counts[name]++
			if _, ok := nameURL[name]; !ok {
				nameURL[name] = url
			}
		}
		for _, n := range p.Names {
			record(n, u)
		}
		for _, e := range p.Entries {
			if e.Anchor != "" {
				record(e.Name, u+"#"+e.Anchor)
			}
		}
	}
	s.linkTargets = map[string]string{}
	for n, c := range counts {
		if c == 1 {
			s.linkTargets[n] = nameURL[n]
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

// xrefCode matches a bold inline run -- the form an emboldened command name
// takes in the body. Its text carries no nested tags.
var xrefCode = regexp.MustCompile(`<code>([^<]+)</code>`)

// xrefName accepts command-shaped names -- one or more space-separated tokens,
// each an identifier possibly namespaced -- so an ensemble subcommand like
// "chan configure" or "string cat" qualifies, while an emboldened literal such
// as "0" or "{}" never does.
var xrefName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(::[A-Za-z0-9_]+)*( [A-Za-z_][A-Za-z0-9_]*)*$`)

// crossReference links emboldened command names in a page's prose to the page
// that documents them (prototype, off unless -xref). It is deliberately
// conservative: it links a <code> run only when its text is a page name that
// exactly one page owns (see linkTargets) and is not the page itself, and it
// never touches text inside a code block, an existing link, or a definition
// label. selfURL and the map values are root-relative; body links sit one
// directory down, hence the "../".
func crossReference(body, selfURL string, targets map[string]string) (string, int) {
	guarded := guardedRanges(body)
	inGuarded := func(pos int) bool {
		for _, r := range guarded {
			if pos >= r[0] && pos < r[1] {
				return true
			}
		}
		return false
	}
	var out strings.Builder
	last, n := 0, 0
	for _, m := range xrefCode.FindAllStringSubmatchIndex(body, -1) {
		start, end, ns, ne := m[0], m[1], m[2], m[3]
		name := body[ns:ne]
		u, ok := targets[name]
		// u == selfURL skips only the pointless whole-page self-link (a page's
		// own name); a same-page subcommand carries a #anchor, so it still links
		// to its definition here.
		if !ok || u == selfURL || !xrefName.MatchString(name) {
			continue
		}
		if inGuarded(start) {
			continue
		}
		out.WriteString(body[last:start])
		out.WriteString(`<a class="xref" href="../` + u + `">` + body[start:end] + `</a>`)
		last, n = end, n+1
	}
	out.WriteString(body[last:])
	return out.String(), n
}

// guardedRanges are byte spans cross-referencing must not touch: code blocks,
// existing links, and definition labels.
func guardedRanges(body string) [][2]int {
	var out [][2]int
	for _, se := range [][2]string{{"<pre", "</pre>"}, {"<a", "</a>"}, {"<dt", "</dt>"}} {
		open, clo := se[0], se[1]
		for i := 0; ; {
			s := strings.Index(body[i:], open)
			if s < 0 {
				break
			}
			s += i
			e := strings.Index(body[s:], clo)
			if e < 0 {
				break
			}
			e = s + e + len(clo)
			out = append(out, [2]int{s, e})
			i = e
		}
	}
	return out
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
//
// It is also deliberately short of things the sections below already show
// well. Tcl and Tk lists ten manuals and Bundled packages five, all of them
// visible without scrolling, so featuring TDBC or Thread bought a duplicate
// rather than a shortcut. The one section a reader cannot take in at a glance
// is tcllib's 136, and no single tcllib manual represents the rest of them --
// so none is picked, rather than an arbitrary one.
var featuredManuals = []string{
	// The language and the widgets.
	"Tcl Built-In Commands",
	"Tk Built-In Commands",
	"Tk Themed Widget",
	"TclOO Commands",
	// The C API: 190 pages, a fifth of the site, and the reason a reader is
	// here at all if they are embedding Tcl rather than scripting it.
	"Tcl Library Procedures",
	"Tk Library Procedures",
	// What most scripts reach for beyond the core: http, msgcat, tcltest.
	"Tcl Bundled Packages",
	// Where you actually start: tclsh and wish.
	"Tcl/Tk Applications",
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

// pageIsCAPI reports whether a page documents the C API. That is a per-page
// property -- section 3 is the C library -- not a per-manual one: the Tk Themed
// Widget manual files eighteen Ttk_* C functions in among the ttk widget
// commands, so its family is not "C API" yet those pages still are. The badge
// keys on this, while the manual's family (home-page grouping, index heading)
// stays on the majority-section heuristic.
func pageIsCAPI(p *Page) bool { return p.Section == "3" }

// indexRowBadge reports whether a C API page's row in its manual's index should
// carry the badge. On a pure C API group the index heading already reads "C API"
// and every row is a function, so badging each row is noise; a mixed manual (Tk
// Themed Widget files Ttk_* C functions among the ttk widget commands) has no
// such heading cue, so there each C API row is flagged in place. The page itself
// always carries the badge at its top -- see pageIsCAPI at the call site.
func indexRowBadge(m *manual, p *Page) bool { return pageIsCAPI(p) && m.Family != "C API" }

// repoSources maps distributions to their upstream repository, in the order the
// footer lists them. A source that ships inside a larger project points at that
// project's repo, so many collapse together: TclOO, Zipfs, http, tcltest and the
// other core libraries are all the Tcl repository. Match is case-insensitive.
var repoSources = []struct {
	Repo    repo
	Sources []string
}{
	{repo{"Tcl", "https://core.tcl-lang.org/tcl/"}, []string{
		"Tcl", "TclOO", "Zipfs", "Tcl-Extensions", "tcltest", "http", "msgcat",
		"platform", "platform::shell", "dde", "registry", "cookiejar", "tcl_idna",
	}},
	{repo{"Tk", "https://core.tcl-lang.org/tk/"}, []string{"Tk"}},
	{repo{"tcllib", "https://core.tcl-lang.org/tcllib/"}, []string{"tcllib"}},
	{repo{"tklib", "https://core.tcl-lang.org/tklib/"}, []string{"tklib"}},
	{repo{"incr Tcl", "https://core.tcl-lang.org/itcl/"}, []string{"itcl", "itk", "iwidgets"}},
	{repo{"tdbc", "https://core.tcl-lang.org/tdbc/"}, []string{"tdbc"}},
	{repo{"Thread", "https://core.tcl-lang.org/thread/"}, []string{"Thread", "Tcl Threading"}},
	{repo{"SQLite", "https://sqlite.org/"}, []string{"sqlite3", "sqlite"}},
}

// collectRepos returns the upstream repositories for the distributions present
// in the corpus, de-duplicated (bundled libraries collapse onto their parent's
// repo) and in repoSources order. An unrecognised source contributes no link --
// there is no repository to guess at -- rather than a wrong one.
func collectRepos(pages []*Page) []repo {
	present := map[string]bool{}
	for _, p := range pages {
		if p.Source != "" {
			present[strings.ToLower(p.Source)] = true
		}
	}
	var out []repo
	for _, rs := range repoSources {
		for _, s := range rs.Sources {
			if present[strings.ToLower(s)] {
				out = append(out, rs.Repo)
				break
			}
		}
	}
	return out
}

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
	// C API prototypes come out of the synopsis, whose heading is generic; label
	// them by what they are rather than the section they were found in.
	if len(defs) > 0 {
		allFuncs := true
		for _, e := range defs {
			if e.Kind != "function" {
				allFuncs = false
				break
			}
		}
		if allFuncs {
			return "Functions"
		}
	}
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
	// version and generated are functions rather than fields on every view: each
	// is one string for the whole build, and threading them through five view
	// types to reach a template that every page shares would be pure ceremony.
	// The timestamp is captured once so every page in a build agrees on it.
	generated := time.Now().UTC().Format("2006-01-02 15:04 UTC")
	tmpl, err := template.New("site").
		Funcs(template.FuncMap{
			"plural":     plural,
			"version":    func() string { return s.Version },
			"generated":  func() string { return generated },
			"hasLicense": func() bool { return len(s.Licenses) > 0 },
			"repos":      func() []repo { return s.Repos },
		}).
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
	ix.AddKeywords(kw, "keywords/index.html")

	// Documentation pages.
	xrefCount := 0
	for _, m := range s.Manuals {
		for _, p := range m.Pages {
			url := s.pageURL[p]
			var body strings.Builder
			renderKids(p.Root, &body)
			bodyHTML := body.String()
			if s.Xref {
				var nx int
				bodyHTML, nx = crossReference(bodyHTML, url, s.linkTargets)
				xrefCount += nx
			}

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
				CAPI      bool
				AlsoNames []string
			}{
				pageView: pageView{
					Page:        p,
					Body:        template.HTML(markOptional(bodyHTML)),
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
				CAPI:      pageIsCAPI(p),
				AlsoNames: also,
			}
			if err := renderTo(tmpl, "page", filepath.Join(outDir, url), view); err != nil {
				return err
			}
			ix.AddPage(p, url, pageIsCAPI(p))
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
				entries = append(entries, indexEntry{Name: n, URL: u, Desc: p.Summary, CAPI: indexRowBadge(m, p)})
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
			Title: m.Name, Manual: m.Name, Dist: m.Source, CAPI: m.Family == "C API", Summary: summary,
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

	if len(s.Licenses) > 0 {
		lv := licenseView{
			Title: "License", Root: "..",
			HasDemos: len(s.Demos) > 0, HasKeywords: hasKeywords,
			Licenses: s.Licenses,
		}
		if err := renderTo(tmpl, "license", filepath.Join(outDir, "license", "index.html"), lv); err != nil {
			return err
		}
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
			Name: m.Name, URL: m.Slug + "/", Entries: n, Dist: m.Source, Family: m.Family,
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
	if s.Xref {
		fmt.Printf("cross-references: %d prose links added\n", xrefCount)
	}
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
