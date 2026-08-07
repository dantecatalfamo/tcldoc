# tcldoc

A static site generator for the Tcl/Tk manual pages. Reads Ousterhout-format
troff directly, emits plain HTML, and ships one small JavaScript file for search.

## Usage

```sh
go build -o tcldoc .

# From a Tcl/Tk source tree (needs both, laid out side by side)
./tcldoc -src ~/tcl-src -out ./site

# Or point at man page directories explicitly
./tcldoc -src ~/tcl-src/tcl9.0.4/doc -src ~/tcl-src/tk9.0.4/doc -out ./site

# Build and preview
./tcldoc -src ~/tcl-src -out ./site -serve :8080

# Include Tk's demonstration scripts
./tcldoc -src ~/tcl-src -demos ~/tcl-src/tk9.0.4/library/demos -out ./site
```

`-src` is repeatable and accepts either a directory to walk or a single file.
It recognises `.n`, `.1`, `.3` and the Homebrew-suffixed `.ntcl` / `.ntk`
variants. Deploy the output directory to any static host.

`-demos` is repeatable too, and points at a Tk demonstration directory —
`library/demos` in a source tree, `lib/tk*/demos` in an installed prefix. Those
are Tcl scripts rather than troff, so they never touch the manual-page parser:
they get their own section, and their names and source join the search index.
The per-script blurbs come from the directory's own `README`, which describes
each runnable program. Images the demos load are not copied; the scripts are
published to be read, not run from the browser.

## What it produces

Against the current Tcl and Tk sources (438 pages):

| | |
|---|---|
| Pages | 438, across 13 manuals |
| Addressable entries | 4,184 |
| Search index | 17,001 terms in 687 shards |
| First-search payload | ~50 KB gzipped |
| Per-query payload | one shard, typically 5–15 KB gzipped |
| Output size | ~8 MB |

The official site indexes about 440 page-level names. This addresses 4,184,
because ensemble subcommands (`string cat`), object methods and widget options
(`-takefocus`) each get their own anchor and their own search entry.

**Distributions.** `.TH`'s source field records which distribution a page ships
in, and that is the only per-page record of provenance — the man directories
sort by section, not by origin, so `mann/` holds Tcl, Tk, tcllib, itcl and the
rest side by side. The landing page groups manuals into `Tcl and Tk`, `tcllib`
and `Bundled packages`; an unrecognised source keeps its own name rather than
being guessed into a family. Every page and manual index shows its distribution
beside the breadcrumb. It is worth knowing that a Homebrew `tcl-tk` prefix
bundles tcllib, so nine manuals in ten there are tcllib rather than core.

The per-manual A-Z index lists commands, with their subcommands grouped as a
run of short links beneath each one. Options and the other per-page definitions
are deliberately left out of it: they outnumber the commands nine to one, and an
index in which `string` is buried between `%S` and `-nocase` is not an index.
They remain anchored, listed in the sidebar of the page that defines them, and
searchable.

## Architecture

```
main.go     discovery, planning, output, collision guard
demos.go    Tk demonstration scripts: discovery, README blurbs
parse.go    troff -> document tree; entry and anchor extraction
inline.go   font state machine, escapes, macro argument splitting
render.go   document tree -> HTML; A-Z index grouping
search.go   two-tier search index
templates/  html/template definitions
assets/     stylesheet and search client
```

**Parsing.** The corpus uses 32 block macros, 4 font escapes and about 15
special characters — a small enough vocabulary to target directly rather than
reaching for groff. `.QW`, `.PQ`, `.AP`, `.OP`, `.SO`/`.SE` and `.CS`/`.CE` are
Tcl's own additions and are implemented from their definitions in
`doc/man.macros`. Inlined macro definitions are skipped, so installed pages
parse as well as source ones.

**Search.** Two tiers, which is what keeps the JavaScript under 200 lines:

1. `names.json` — every page, subcommand, method and option. Loaded once on
   first keystroke, answers prefix and substring queries locally. This covers
   almost every real lookup with no further requests.
2. `idx/<prefix>.json` — an inverted index over the full prose, sharded by the
   first two characters of each term. A query fetches only the shards for the
   words typed, so payload scales with the query rather than the corpus.
   Weights are precomputed tf-idf, so the client does no scoring maths.

Everything except search works with JavaScript disabled: every page, every
per-manual A–Z index, and every cross-reference is a plain link.

**Notation.** troff distinguishes literal syntax (bold) from substitutable
arguments (italic); Tcl marks optional groups with `?question marks?`. All three
render as distinct, colour-coded elements (`<code>`, `<var>`, `.opt`) with a
legend on every page. The official converter renders the first two as generic
bold and italic and leaves the third as plain text.

## Validation

The generator is checked against the full corpus:

- all 438 pages parse with no warnings
- no unrendered troff escapes survive into the output
- all 444 output pages are well-formed (no tag nesting errors)
- all 27,676 internal links resolve, including every anchor
- all 4,184 search entries point at an anchor that exists

Two corpus quirks worth knowing about, both caught by that checking:

- Seven Tk pages declare a multi-word `.TH` name (`.TH tk accessible n`), so
  page identity comes from the source filename, not from `.TH`. A collision
  guard warns rather than silently overwriting if two pages ever collide.
- `options.n` documents option aliases inline (`.OP "\-background or \-bg"`).
  Both spellings are indexed and both resolve to the same anchor.
- An *installed* manual tree writes one complete copy of a multi-name page per
  name on its NAME line, so `library.n` arrives nineteen times over — as
  `auto_execok.n`, `parray.n`, `writeFile.n` and so on. Copies are recognised by
  content and collapsed onto the one whose filename matches its `.TH` name.
  Without that, every copy is a page in its own right and contributes all
  eighteen of its names to the manual index, which listed `writeFile` 38 times.

## Known limitations

- Cross-references inside prose are not auto-linked; only `SEE ALSO` and
  standard-option lists become links. Auto-linking every mention of a command
  name is doable but produces noisy false positives in code examples.
- `.ta` tab stops are ignored, so a few wide tabular displays inside `.nf`
  blocks are less tidy than groff would render them.
- No keyword index yet. `.SH KEYWORDS` is parsed and shown per page, and the
  keywords feed the full-text index, but there is no aggregated keyword page
  equivalent to the official site's `Keywords/` section.
- Bundled packages (itcl, tdbc, sqlite, thread) are only included if their
  `pkgs/` directories are present in the source tree you point at.
