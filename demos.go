package main

import (
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A demo is one script from a Tk demonstration directory.
//
// Tk ships these under lib/tk*/demos rather than in a doc directory, and they
// are Tcl source rather than troff, so they arrive through -demos rather than
// -src and never pass through the manual-page parser.
type demo struct {
	Name    string // file name, e.g. "widget" or "floor.tcl"
	File    string // where it was read from
	Desc    string // from the directory's README, where it describes the script
	Source  string
	Lines   int
	Program bool // runnable on its own, rather than a package the demos load
	URL     string
}

// demoSkip are the files in a demo directory that are not demonstrations: the
// autoload index, the licence, and the README the descriptions come from.
var demoSkip = map[string]bool{
	"README": true, "license.terms": true, "tclIndex": true,
}

// demoAsset are the extensions of files a demo loads rather than is.
var demoAsset = map[string]bool{
	".png": true, ".gif": true, ".xbm": true, ".ppm": true, ".svg": true,
	".msg": true, ".terms": true, ".bmp": true, ".jpg": true,
}

func discoverDemos(roots []string) ([]*demo, error) {
	var out []*demo
	seen := map[string]bool{}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		descs := map[string]string{}
		if b, err := os.ReadFile(filepath.Join(root, "README")); err == nil {
			descs = parseDemoREADME(string(b))
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || demoSkip[name] || demoAsset[filepath.Ext(name)] ||
				strings.HasPrefix(name, ".") || seen[name] {
				continue
			}
			b, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				continue
			}
			seen[name] = true
			src := strings.ReplaceAll(string(b), "\r\n", "\n")
			out = append(out, &demo{
				Name: name,
				File: filepath.Join(root, name),
				Desc: descs[name],
				// A program is the thing you run; the .tcl files are the
				// procedure packages it loads, as the README explains.
				Program: filepath.Ext(name) != ".tcl",
				Source:  src,
				Lines:   strings.Count(strings.TrimRight(src, "\n"), "\n") + 1,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Program != out[j].Program {
			return out[i].Program // programs first
		}
		return out[i].Name < out[j].Name
	})

	// Filenames become URLs, with the same case-insensitive guard the manual
	// pages get: "floor.tcl" and a program named "floor" would collide.
	taken := map[string]bool{}
	for _, d := range out {
		base := slugFile(strings.TrimSuffix(d.Name, ".tcl"))
		file := base
		for n := 2; taken[strings.ToLower(file)]; n++ {
			file = base + "-" + strconv.Itoa(n)
		}
		taken[strings.ToLower(file)] = true
		d.URL = "demos/" + file + ".html"
	}
	return out, nil
}

// demoDesc matches a README entry: "hello -\t\tCreates a single button...",
// continued on the indented lines that follow.
var demoDesc = regexp.MustCompile(`^(\S+)[ \t]+-[ \t]+(.*)$`)

func parseDemoREADME(text string) map[string]string {
	out := map[string]string{}
	cur := ""
	for _, line := range strings.Split(text, "\n") {
		if m := demoDesc.FindStringSubmatch(line); m != nil {
			cur = m[1]
			out[cur] = strings.TrimSpace(m[2])
			continue
		}
		if cur == "" {
			continue
		}
		if strings.TrimSpace(line) == "" {
			cur = ""
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			out[cur] = strings.TrimSpace(out[cur] + " " + strings.TrimSpace(line))
		}
	}
	return out
}

// escapeSource renders a script for a <pre> block. No highlighting: nothing
// else on the site is highlighted, and a Tcl tokenizer is a lot of machinery
// for a section that is here to be read, not skimmed.
func escapeSource(s string) string { return html.EscapeString(strings.TrimRight(s, "\n")) }
