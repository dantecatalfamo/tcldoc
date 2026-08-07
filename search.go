package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The search design has two tiers, which is what keeps the JavaScript small.
//
// Tier 1 (names.json, loaded once on first keystroke): every page name, every
// ensemble subcommand, every widget option. A few thousand short records. This
// answers the overwhelmingly common query — "where is `string trim`" — with a
// pure prefix match and no network round trip after the initial load.
//
// Tier 2 (idx/<prefix>.json, fetched per query term): a classic inverted index
// over the full prose, sharded by the first two characters of each term so the
// client only ever downloads the postings it actually needs. A query for
// "virtual event" fetches two small files instead of a multi-megabyte blob.

// NameRec references its page by index into Docs rather than repeating the URL
// and summary, which cuts the eagerly-loaded index by roughly 40%.
type NameRec struct {
	Name   string `json:"n"`
	Doc    int    `json:"d"`
	Anchor string `json:"a,omitempty"`
	Kind   string `json:"k,omitempty"`
}

type DocRec struct {
	URL     string `json:"u"`
	Title   string `json:"t"`
	Manual  string `json:"m"`
	Summary string `json:"s"`
}

type posting struct {
	Doc int
	TF  int
}

type Indexer struct {
	Names    []NameRec
	Docs     []DocRec
	postings map[string]map[int]int // term -> docID -> term frequency
}

func NewIndexer() *Indexer {
	return &Indexer{postings: map[string]map[int]int{}}
}

var wordRe = regexp.MustCompile(`[a-z0-9_]+(?:::[a-z0-9_]+)*`)

// stopwords are dropped from the full-text index. Deliberately short: technical
// prose uses words like "if", "for" and "while" as identifiers, so anything that
// doubles as a Tcl command is kept.
var stopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "of": true,
	"to": true, "in": true, "is": true, "are": true, "be": true, "was": true,
	"it": true, "that": true, "this": true, "with": true, "as": true,
	"by": true, "on": true, "at": true, "from": true, "not": true,
	"which": true, "will": true, "would": true, "can": true, "may": true,
	"has": true, "have": true, "been": true, "were": true, "its": true,
	"but": true, "they": true, "them": true, "then": true, "than": true,
	"such": true, "any": true, "all": true, "one": true, "also": true,
	"there": true, "these": true, "those": true, "into": true, "other": true,
}

func tokenize(s string) []string {
	return wordRe.FindAllString(strings.ToLower(s), -1)
}

// AddPage registers one page in both tiers.
func (ix *Indexer) AddPage(p *Page, url string) {
	id := len(ix.Docs)
	ix.Docs = append(ix.Docs, DocRec{
		URL: url, Title: p.Title, Manual: p.Manual, Summary: p.Summary,
	})

	anchored := map[string]bool{}
	for _, e := range p.Entries {
		// Skip entries that merely repeat the page's own title.
		if e.Name == p.Title {
			continue
		}
		anchored[e.Name] = true
		ix.Names = append(ix.Names, NameRec{
			Name: e.Name, Doc: id, Anchor: e.Anchor, Kind: e.Kind,
		})
	}
	// A name that has its own definition on the page is already covered by the
	// anchored record above; adding the page-level one too would put the same
	// name in the results twice, with the vaguer link ranked higher.
	for _, n := range p.Names {
		if anchored[n] {
			continue
		}
		ix.Names = append(ix.Names, NameRec{Name: n, Doc: id})
	}

	// Full text: names and summary weighted more heavily by simple repetition,
	// which is enough given how short these documents are.
	var sb strings.Builder
	for i := 0; i < 4; i++ {
		sb.WriteString(strings.Join(p.Names, " ") + " " + p.Summary + " ")
	}
	sb.WriteString(strings.Join(p.Keywords, " ") + " ")
	collectText(p.Root, &sb)

	for _, tok := range tokenize(sb.String()) {
		if len(tok) < 2 || stopwords[tok] {
			continue
		}
		m := ix.postings[tok]
		if m == nil {
			m = map[int]int{}
			ix.postings[tok] = m
		}
		m[id]++
	}
}

func collectText(n *Node, sb *strings.Builder) {
	if n.Tag != "" {
		sb.WriteString(stripTags(n.Tag) + " ")
	}
	if n.Text != "" {
		sb.WriteString(stripTags(n.Text) + " ")
	}
	for _, c := range n.Children {
		collectText(c, sb)
	}
}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func stripTags(s string) string { return tagRe.ReplaceAllString(s, " ") }

func shardKey(term string) string {
	k := term
	if len(k) > 2 {
		k = k[:2]
	}
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		c := k[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "__"
	}
	return b.String()
}

// Write emits the name index, the document table and the sharded postings.
func (ix *Indexer) Write(dir string) (stats map[string]int, err error) {
	sort.Slice(ix.Names, func(i, j int) bool {
		a, b := ix.Names[i], ix.Names[j]
		if la, lb := len(a.Name), len(b.Name); la != lb {
			return la < lb
		}
		return a.Name < b.Name
	})

	if err = writeJSON(filepath.Join(dir, "names.json"), ix.Names); err != nil {
		return nil, err
	}
	if err = writeJSON(filepath.Join(dir, "docs.json"), ix.Docs); err != nil {
		return nil, err
	}

	// Shard postings by term prefix.
	shards := map[string]map[string][][2]int{}
	nDocs := float64(len(ix.Docs))
	for term, docs := range ix.postings {
		// A term in almost every document carries no signal.
		if float64(len(docs)) > nDocs*0.6 {
			continue
		}
		key := shardKey(term)
		if shards[key] == nil {
			shards[key] = map[string][][2]int{}
		}
		ids := make([]int, 0, len(docs))
		for d := range docs {
			ids = append(ids, d)
		}
		sort.Ints(ids)
		// Precompute a rounded tf-idf weight so the client does no math.
		idf := math.Log(1 + nDocs/float64(len(docs)))
		list := make([][2]int, 0, len(ids))
		for _, d := range ids {
			w := int(math.Round(10 * idf * (1 + math.Log(float64(docs[d])))))
			if w < 1 {
				w = 1
			}
			list = append(list, [2]int{d, w})
		}
		shards[key][term] = list
	}

	idxDir := filepath.Join(dir, "idx")
	if err = os.MkdirAll(idxDir, 0o755); err != nil {
		return nil, err
	}
	total := 0
	for key, terms := range shards {
		if err = writeJSON(filepath.Join(idxDir, key+".json"), terms); err != nil {
			return nil, err
		}
		total += len(terms)
	}
	return map[string]int{
		"names": len(ix.Names), "docs": len(ix.Docs),
		"terms": total, "shards": len(shards),
	}, nil
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
