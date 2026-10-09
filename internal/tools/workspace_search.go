package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
)

var workspaceSearchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Free-text question, e.g. \"daemon locking\"; ranked, token-aware, not a regex."
    },
    "corpora": {
      "type": "array",
      "items": {"type": "string", "enum": ["code", "docs", "memory"]},
      "description": "Corpora to search: code, docs, memory (default all three)."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 100,
      "description": "Merged results returned (default 20)."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)

// WorkspaceSearch is the ranked-discovery broker over the existing indexed
// corpora: code and docs via the topology FTS5 index, memories via the memory
// FTS5 index. It is approximate by design — results are ranked and labelled,
// never exhaustive. Concurrency: stateless; safe for concurrent use (the
// underlying stores serialise their own access).
type WorkspaceSearch struct {
	ws      WorkspaceFn
	storeFn func() *topology.Store
	memFn   func() *memory.Index
}

// NewWorkspaceSearch returns a new WorkspaceSearch tool over the connection's
// topology store accessor.
func NewWorkspaceSearch(ws WorkspaceFn, storeFn func() *topology.Store) *WorkspaceSearch {
	return &WorkspaceSearch{ws: ws, storeFn: storeFn}
}

// WithMemoryIndex wires the per-connection memory FTS index accessor.
func (t *WorkspaceSearch) WithMemoryIndex(fn func() *memory.Index) *WorkspaceSearch {
	t.memFn = fn
	return t
}

func (*WorkspaceSearch) Name() string                 { return "workspace_search" }
func (*WorkspaceSearch) InputSchema() json.RawMessage { return workspaceSearchSchema }

func (*WorkspaceSearch) Description() string {
	return "Ranked discovery across indexed code symbols, doc sections (Markdown, HTML) and project memories, for a conceptual question such as \"where is daemon locking handled?\". Each hit is labelled with corpus, source, field, score and why it matched, and the header reports each index's freshness. Approximate by design and never proof of absence: for exact matches use search_in_files."
}

type workspaceSearchArgs struct {
	Query   string   `json:"query"`
	Corpora []string `json:"corpora"`
	Limit   int      `json:"limit"`
}

func parseWorkspaceSearchArgs(raw json.RawMessage) (workspaceSearchArgs, error) {
	var a workspaceSearchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("workspace_search: invalid arguments: %w", err)
	}
	return a, nil
}

func (a *workspaceSearchArgs) validate() error {
	if strings.TrimSpace(a.Query) == "" {
		return errors.New("workspace_search: query is required")
	}
	for _, c := range a.Corpora {
		if c != "code" && c != "docs" && c != "memory" {
			return fmt.Errorf("workspace_search: unknown corpus %q (want code, docs, or memory)", c)
		}
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	if a.Limit > 100 {
		a.Limit = 100
	}
	return nil
}

// wants reports whether the given corpus was requested (all corpora when the
// filter is empty).
func (a *workspaceSearchArgs) wants(corpus string) bool {
	if len(a.Corpora) == 0 {
		return true
	}
	for _, c := range a.Corpora {
		if c == corpus {
			return true
		}
	}
	return false
}

// wsHit is one merged broker result, carrying the labelling contract fields.
type wsHit struct {
	corpus    string // code | docs | memory
	source    string // topology-fts | memory-fts
	path      string // workspace-relative; "" for memories
	line      int    // 0 when unknown
	label     string // symbol "name (kind)" or memory name
	field     string
	score     float64
	snippet   string
	why       string
	coverage  float64
	isDemoted bool
}

func (t *WorkspaceSearch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseWorkspaceSearchArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	code, docs, topoStatus := t.searchTopology(ctx, a)
	mem, memStatus := t.searchMemory(ctx, a)
	merged := interleaveHits(a.Limit, code, docs, mem)
	return formatWorkspaceSearch(a, merged, topoStatus, memStatus), nil
}

// searchTopology queries the topology FTS index for the code and docs corpora.
// The two corpora get SEPARATE query budgets: docs is served by dedicated
// language-filtered queries (Markdown, HTML), not by whatever doc nodes happen
// to survive a shared ranked list — in a code-heavy repo the top hits are all
// code, and a shared budget would starve the docs corpus entirely.
func (t *WorkspaceSearch) searchTopology(ctx context.Context, a workspaceSearchArgs) (code, docs []wsHit, status string) {
	if !a.wants("code") && !a.wants("docs") {
		return nil, nil, "skipped"
	}
	store := t.storeFn()
	if store == nil {
		return nil, nil, "missing"
	}
	status = topologyIndexStatus(store)
	if a.wants("code") {
		code = t.searchCode(ctx, store, a)
	}
	if a.wants("docs") {
		docs = searchDocs(ctx, store, a)
	}
	return code, docs, status
}

// searchCode returns the code-corpus hits: one ranked topology query with doc
// nodes dropped (they are served by searchDocs' dedicated queries). Common
// import/package-only matches on conceptual (multi-term) queries are demoted so
// declarations with higher token coverage rank first, while exact-symbol and
// explicit import queries retain their top hits.
func (t *WorkspaceSearch) searchCode(ctx context.Context, store *topology.Store, a workspaceSearchArgs) []wsHit {
	fetchLimit := a.Limit * 4
	if fetchLimit < 40 {
		fetchLimit = 40
	}
	results, err := store.Search(ctx, a.Query, topology.SearchOpts{Limit: fetchLimit, Snippets: true})
	if err != nil {
		return nil
	}
	terms := queryTerms(a.Query)
	isExplicitImport := isExplicitImportQuery(terms)
	isMultiTerm := len(terms) > 1

	var code []wsHit
	for _, r := range results {
		if isDocNode(r.Node) {
			continue
		}
		h := topoHit(r)
		h.corpus, h.why = "code", codeWhy(r.Field)

		isImportOrPkg := r.Node.Kind == topology.KindImport || r.Node.Kind == topology.KindPackage
		matched := countMatchedTerms(r.Node, terms)
		var coverage float64
		if len(terms) > 0 {
			coverage = float64(matched) / float64(len(terms))
		}
		h.coverage = coverage

		// Demote common import/package-only matches during conceptual (multi-term)
		// discovery while preserving exact-symbol (single-term) or explicit import controls.
		if isMultiTerm && isImportOrPkg && !isExplicitImport && coverage < 0.5 {
			h.isDemoted = true
		}
		code = append(code, h)
	}

	sort.SliceStable(code, func(i, j int) bool {
		// Non-demoted declarations rank ahead of demoted imports.
		if code[i].isDemoted != code[j].isDemoted {
			return !code[i].isDemoted
		}
		// When query has multiple terms, higher token coverage ranks first.
		if isMultiTerm && code[i].coverage != code[j].coverage {
			return code[i].coverage > code[j].coverage
		}
		// Otherwise, rank by FTS score.
		return code[i].score > code[j].score
	})

	if len(code) > a.Limit {
		code = code[:a.Limit]
	}
	return code
}

func queryTerms(query string) []string {
	var terms []string
	seen := make(map[string]bool)
	for _, f := range strings.Fields(strings.ToLower(query)) {
		t := strings.Trim(f, "\"'`,;:.()[]{}*!?")
		if t != "" && !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	return terms
}

func isExplicitImportQuery(terms []string) bool {
	for _, t := range terms {
		if t == "import" || t == "imports" || t == "package" || t == "pkg" {
			return true
		}
	}
	return false
}

func countMatchedTerms(n topology.Node, terms []string) int {
	nameLower := strings.ToLower(n.Name)
	qualLower := strings.ToLower(n.Qualified)
	sigLower := strings.ToLower(n.Signature)
	docLower := strings.ToLower(n.Docstring)
	pathLower := strings.ToLower(n.Path)

	count := 0
	for _, t := range terms {
		if strings.Contains(nameLower, t) ||
			strings.Contains(qualLower, t) ||
			strings.Contains(sigLower, t) ||
			strings.Contains(docLower, t) ||
			strings.Contains(pathLower, t) {
			count++
		}
	}
	return count
}

// searchDocs returns the docs-corpus hits via per-language queries (the
// indexed doc languages), merged by score — scores from the same FTS index
// are comparable.
func searchDocs(ctx context.Context, store *topology.Store, a workspaceSearchArgs) []wsHit {
	var docs []wsHit
	for _, lang := range []string{"markdown", "html"} {
		results, err := store.Search(ctx, a.Query, topology.SearchOpts{Limit: a.Limit, Snippets: true, Language: lang})
		if err != nil {
			continue
		}
		for _, r := range results {
			h := topoHit(r)
			h.corpus, h.why = "docs", docWhy(r.Field)
			docs = append(docs, h)
		}
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].score > docs[j].score })
	return docs
}

// topoHit converts one topology search result into the broker's hit shape
// (corpus and why are set by the caller).
func topoHit(r topology.SearchResult) wsHit {
	selector := r.Node.Name
	if r.Node.Qualified != "" && strings.Contains(r.Node.Qualified, ".") {
		selector = r.Node.Qualified
	}
	return wsHit{
		source:  "topology-fts",
		path:    r.Node.Path,
		line:    r.Node.StartLine,
		label:   fmt.Sprintf("%s (%s)", selector, r.Node.Kind),
		field:   r.Field,
		score:   r.Score,
		snippet: r.Snippet,
	}
}

// searchMemory queries the memory FTS index. A stale index still serves
// (honestly labelled) and kicks an async reindex to self-heal, mirroring
// search_memories' auto mode.
func (t *WorkspaceSearch) searchMemory(ctx context.Context, a workspaceSearchArgs) ([]wsHit, string) {
	if !a.wants("memory") {
		return nil, "skipped"
	}
	ws := ""
	if t.ws != nil {
		ws = t.ws(ctx)
	}
	ix := resolveMemoryIndex(t.memFn, ws)
	if ix == nil {
		return nil, "missing"
	}
	status := "fresh"
	if fresh, _ := ix.Fresh(ws); !fresh {
		status = "stale"
		ix.ReindexAsync(ws)
	}
	hits, err := ix.Search(ctx, a.Query, memory.SearchOpts{Limit: a.Limit, Snippets: true})
	if err != nil {
		return nil, status
	}
	out := make([]wsHit, 0, len(hits))
	for _, h := range hits {
		label := h.Name
		if h.Confidence != "" && h.Confidence != "user" {
			label += " [" + h.Confidence + "]"
		}
		snippet := h.Description
		if snippet == "" {
			snippet = h.Snippet
		}
		out = append(out, wsHit{
			corpus:  "memory",
			source:  "memory-fts",
			label:   label,
			field:   h.Field,
			score:   h.Score,
			snippet: snippet,
			why:     memoryWhy(h.Field),
		})
	}
	return out, status
}

// topologyIndexStatus maps the indexer state onto the broker's freshness
// vocabulary: idle means the watcher/indexer has caught up (fresh), a stopped
// or errored indexer may be serving an out-of-date snapshot (stale), and
// anything else is mid-build.
func topologyIndexStatus(store *topology.Store) string {
	return indexFreshness(store.Health())
}

func indexFreshness(h topology.Health) string {
	if h.Failing {
		// A retry cycle after a failure reports "running"; it is still serving
		// the pre-failure snapshot, which is stale, not building.
		return "stale"
	}
	switch h.State {
	case "idle":
		return "fresh"
	case "stopped", "error":
		return "stale"
	default:
		return "building"
	}
}

// isDocNode reports whether a topology node belongs to the docs corpus: a
// document section heading, or any node in a Markdown/HTML file.
func isDocNode(n topology.Node) bool {
	if n.Kind == topology.KindSection {
		return true
	}
	switch strings.ToLower(filepath.Ext(n.Path)) {
	case ".md", ".markdown", ".html", ".htm":
		return true
	}
	return false
}

func codeWhy(field string) string {
	switch field {
	case "name":
		return "symbol name match"
	case "name_tokens":
		return "symbol name token match"
	case "qualified":
		return "qualified name match"
	case "signature":
		return "signature match"
	case "docstring":
		return "doc comment match"
	default:
		return field + " match"
	}
}

func docWhy(field string) string {
	switch field {
	case "name", "name_tokens":
		return "heading match"
	case "docstring":
		return "section text match"
	default:
		return "document " + field + " match"
	}
}

func memoryWhy(field string) string {
	switch field {
	case "name", "tokens":
		return "memory name match"
	case "description":
		return "memory description match"
	case "body":
		return "memory body match"
	case "paths":
		return "memory path glob match"
	case "source_paths", "source_symbols":
		return "memory provenance match"
	default:
		return "memory " + field + " match"
	}
}

// interleaveHits merges the per-corpus result lists round-robin by rank, so
// the top hit of each corpus appears before any corpus' second hit — raw FTS5
// scores are not comparable across different indexes.
func interleaveHits(limit int, lists ...[]wsHit) []wsHit {
	var out []wsHit
	for i := 0; len(out) < limit; i++ {
		advanced := false
		for _, l := range lists {
			if i < len(l) {
				out = append(out, l[i])
				advanced = true
				if len(out) == limit {
					return out
				}
			}
		}
		if !advanced {
			break
		}
	}
	return out
}

func formatWorkspaceSearch(a workspaceSearchArgs, hits []wsHit, topoStatus, memStatus string) string {
	header := fmt.Sprintf("(mode=ranked, exact_match=false; index status: code/docs=%s memory=%s)", topoStatus, memStatus)
	if len(hits) == 0 {
		return fmt.Sprintf("No indexed matches for %q %s.\nThis is ranked discovery, not proof of absence — use search_in_files for an exact scan.", a.Query, header)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "workspace search: %d hit(s) for %q %s\n", len(hits), a.Query, header)
	for i, h := range hits {
		fmt.Fprintf(&sb, "\n%3d. [%s] %s", i+1, h.corpus, h.label)
		if h.path != "" {
			loc := h.path
			if h.line > 0 {
				loc = fmt.Sprintf("%s:%d", h.path, h.line)
			}
			fmt.Fprintf(&sb, " — %s", loc)
		}
		fmt.Fprintf(&sb, "\n     source=%s field=%s score=%.3f why=%s\n", h.source, h.field, h.score, h.why)
		if h.snippet != "" {
			fmt.Fprintf(&sb, "     %s\n", firstNonEmptyLine(h.snippet))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// firstNonEmptyLine keeps multi-line snippets to a single display line.
func firstNonEmptyLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
