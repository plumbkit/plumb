package tools

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/plumbkit/plumb/internal/textfmt"
	"github.com/plumbkit/plumb/internal/topology"
)

// context_docs.go — document sections as evidence.
//
// A section is chosen in two steps, both from what the call already knows. The
// index's headings pick the candidate files: an explicit document seed, or a file
// with a heading that shares a term with the task prose or a seed's name. Then each
// candidate is read once, from one snapshot, and its sections are scored on their
// own text, so a rule stated in a paragraph is found when only its file's heading
// matched. The scope filter (root, within, corpora, plumb's own state) judges every
// file before it is read, and a sensitive file is named and never read.

const (
	contextMaxDocFiles    = 3
	contextMaxDocSections = 4
	// contextSectionText bounds the text of one section that is scored.
	contextSectionText = 4096
	// contextSectionScan bounds the index headings examined for one call.
	contextSectionScan = 20000
	// contextDocMinScore is the least a section from a file that was not itself a
	// seed must score: a heading term is worth 2, a term in its text 1.
	contextDocMinScore = 2
)

// docQuery is the set of terms a section is judged against: the task prose's
// meaningful tokens and the tokens of the seeds' names. Prose only ever scores a
// candidate; it never makes one a seed.
type docQuery struct {
	rk     ranker
	tokens map[string]bool
}

func newDocQuery(seeds []contextSeed, req contextRequest) docQuery {
	q := docQuery{rk: newRanker(req.Task, req.Intent, nil), tokens: map[string]bool{}}
	for t := range q.rk.task {
		q.tokens[t] = true
	}
	for _, s := range seeds {
		name := s.Name
		if s.Kind == seedFile {
			name = strings.TrimSuffix(path.Base(s.Path), path.Ext(s.Path))
		}
		for _, t := range q.rk.tokens(name) {
			q.tokens[t] = true
		}
	}
	return q
}

// hits counts the distinct query terms text contains.
func (q docQuery) hits(text string) int {
	seen := map[string]bool{}
	for _, t := range q.rk.tokens(textfmt.TruncateBytes(text, contextSectionText)) {
		if q.tokens[t] {
			seen[t] = true
		}
	}
	return len(seen)
}

// docFile is one candidate document: the headings the index holds for it.
type docFile struct {
	path     string
	seeded   bool
	headHits int
	headings []topology.Node
}

// docCandidates groups the index's headings by file, drops the files the scope
// does not admit (counting them, never naming them) and ranks the rest: explicit
// seeds first, then by how many headings match, then by path. Only a seed or a file
// with a matching heading is a candidate.
func (q docQuery) docCandidates(secs []topology.Node, seeded map[string]bool, scope contextScope) (files []*docFile, excluded int) {
	byPath := map[string]*docFile{}
	skipped := map[string]bool{}
	for _, n := range secs {
		if n.Language != "markdown" || corpusOfPath(n.Path) != corpusDocs || skipped[n.Path] {
			continue
		}
		f := byPath[n.Path]
		if f == nil {
			if ok, _ := scope.allows(n.Path, corpusDocs); !ok || plumbStateReason(n.Path) != "" {
				skipped[n.Path] = true
				continue
			}
			f = &docFile{path: n.Path, seeded: seeded[n.Path]}
			byPath[n.Path] = f
			files = append(files, f)
		}
		f.headings = append(f.headings, n)
		f.headHits += q.hits(n.Name)
	}
	files = slices.DeleteFunc(files, func(f *docFile) bool { return !f.seeded && f.headHits == 0 })
	slices.SortFunc(files, func(a, b *docFile) int {
		if a.seeded != b.seeded {
			if a.seeded {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b.headHits, a.headHits), cmp.Compare(a.path, b.path))
	})
	return files, len(skipped)
}

// docSection is one scored section of a candidate document.
type docSection struct {
	file    *docFile
	node    topology.Node
	score   int
	headHit int
	textHit int
	text    string // the section's own text, below its heading and above the next
}

// docSource is a candidate document as one snapshot of it reads.
type docSource struct {
	lines    []string
	headings []topology.Node
	changed  bool // the file differs from what the index parsed
}

// openDoc reads a candidate once and returns the headings that describe those
// bytes: the index's when the index parsed exactly them, else the snapshot's own
// re-extracted, so a section is never sliced by a stale span (Invariant 5).
func openDoc(ctx context.Context, store *topology.Store, f *docFile, abs string, budget *sourceBudget) (*docSource, string) {
	info, err := os.Stat(abs)
	if err != nil {
		return nil, "cannot be read"
	}
	if !budget.take(info.Size()) {
		return nil, fmt.Sprintf("the source-read budget (%dx max_bytes, shared with the bodies and memories) was spent", contextSourceReadFactor)
	}
	lines, snap, err := snapshotLines(abs)
	if err != nil {
		return nil, "cannot be read"
	}
	src := &docSource{lines: lines, headings: f.headings}
	hash, ok, herr := store.IndexedContentHash(ctx, f.path)
	if herr == nil && ok && hash == snap.sha {
		return src, ""
	}
	nodes, err := store.ExtractSource(ctx, f.path, []byte(strings.Join(lines, "\n")))
	if err != nil || len(nodes) == 0 {
		return nil, "changed since it was indexed and its sections cannot be re-read"
	}
	src.headings = slices.DeleteFunc(nodes, func(n topology.Node) bool { return n.Kind != topology.KindSection })
	src.changed = true
	return src, ""
}

// sections scores every heading of the snapshot on its own text.
func (s *docSource) sections(q docQuery, f *docFile) []docSection {
	heads := slices.Clone(s.headings)
	slices.SortStableFunc(heads, func(a, b topology.Node) int { return cmp.Compare(a.StartLine, b.StartLine) })
	out := make([]docSection, 0, len(heads))
	for i, n := range heads {
		end := n.EndLine
		if i+1 < len(heads) && heads[i+1].StartLine-1 < end {
			end = heads[i+1].StartLine - 1
		}
		text, _ := sliceLines(s.lines, n.StartLine+1, end)
		if strings.TrimSpace(text) == "" {
			continue // a heading with nothing under it states no constraint
		}
		sec := docSection{file: f, node: n, text: text, headHit: q.hits(n.Name), textHit: q.hits(text)}
		sec.score = 2*sec.headHit + sec.textHit
		if f.seeded || sec.score >= contextDocMinScore {
			out = append(out, sec)
		}
	}
	return out
}

// docConstraints retrieves the document sections that bear on the seeds. A missing
// or failing index gives none, and says so.
func (c *ContextCollector) docConstraints(ctx context.Context, pack *contextPack, scope contextScope, index contextIndex, req contextRequest, reads *sourceBudget) ([]contextConstraint, []string) {
	if !scope.corpusAllowed(corpusDocs) {
		return nil, nil
	}
	if !index.usable() {
		return nil, []string{"document sections were not collected: they come from the topology index, which is unavailable or failing"}
	}
	secs, err := index.store.NodesByKind(ctx, topology.KindSection)
	if err != nil {
		return nil, []string{"document sections were not collected: the index could not list them"}
	}
	var gaps []string
	if len(secs) > contextSectionScan {
		secs = secs[:contextSectionScan]
		gaps = append(gaps, fmt.Sprintf("document sections: only the first %d headings were considered", contextSectionScan))
	}
	q := newDocQuery(pack.Seeds, req)
	files, excluded := q.docCandidates(secs, docSeeds(pack.Seeds), scope)
	if excluded > 0 {
		gaps = append(gaps, fmt.Sprintf("document sections: %d document(s) outside within/corpora were left out", excluded))
	}
	if len(files) > contextMaxDocFiles {
		files = files[:contextMaxDocFiles]
	}
	found, readGaps := c.readDocSections(ctx, index.store, pack.Root, q, files, reads)
	return found, append(gaps, readGaps...)
}

// docSeeds is the set of documents the caller named as seeds.
func docSeeds(seeds []contextSeed) map[string]bool {
	out := map[string]bool{}
	for _, s := range seeds {
		if s.Kind == seedFile && corpusOfPath(s.Path) == corpusDocs {
			out[s.Path] = true
		}
	}
	return out
}

// readDocSections reads the candidates (a sensitive one is named, not read) out of
// the call's shared source-read budget, and keeps the best-scoring sections under
// the section cap.
func (c *ContextCollector) readDocSections(ctx context.Context, store *topology.Store, root string, q docQuery, files []*docFile, budget *sourceBudget) ([]contextConstraint, []string) {
	var out []contextConstraint
	var gaps []string
	var pool []docSection
	for _, f := range files {
		abs := absUnder(root, f.path)
		if c.sensitive != nil && c.sensitive(ctx, abs, "") {
			out = append(out, contextConstraint{Corpus: corpusDocs, Path: f.path, Label: f.path, Withheld: true})
			continue
		}
		src, why := openDoc(ctx, store, f, abs, budget)
		if src == nil {
			gaps = append(gaps, fmt.Sprintf("document sections: %s was not read: %s", textfmt.TerminalSafeLine(f.path), why))
			continue
		}
		pool = append(pool, src.sections(q, f)...)
		if src.changed {
			gaps = append(gaps, fmt.Sprintf("document sections: %s changed since it was indexed; its sections come from the current file", textfmt.TerminalSafeLine(f.path)))
		}
	}
	slices.SortStableFunc(pool, func(a, b docSection) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.file.path, b.file.path), cmp.Compare(a.node.StartLine, b.node.StartLine))
	})
	room := max(contextMaxDocSections-len(out), 0)
	if len(pool) > room {
		gaps = append(gaps, fmt.Sprintf("document sections: %d further matching section(s) are not listed (cap %d)", len(pool)-room, contextMaxDocSections))
		pool = pool[:room]
	}
	for _, s := range pool {
		out = append(out, s.constraint())
	}
	return out, gaps
}

// constraint is the section as the pack presents it.
func (s docSection) constraint() contextConstraint {
	why := "seed document"
	if s.score > 0 {
		why = fmt.Sprintf("matches the task and seed names (heading %d, text %d)", s.headHit, s.textHit)
	}
	return contextConstraint{
		Corpus: corpusDocs, Path: s.file.path, Why: why, Excerpt: excerptOf(s.text),
		Label: fmt.Sprintf("%s#%s (lines %d–%d)", s.file.path, s.node.Name, s.node.StartLine, s.node.EndLine),
	}
}
