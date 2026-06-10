package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/topology"
)

var workspaceSearchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Free-text discovery query, e.g. \"daemon locking\" or \"workspace pool\". Token-aware and ranked; not a regex and not an exact scan."
    },
    "corpora": {
      "type": "array",
      "items": {"type": "string", "enum": ["code", "docs", "memory"]},
      "description": "Restrict the search to these corpora. Omit to search all three: code (indexed symbols), docs (indexed Markdown/HTML sections), memory (project memories)."
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 100,
      "description": "Maximum number of merged results to return. Default 20."
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
	return "Ranked discovery across the workspace's indexed corpora: code symbols, doc sections (Markdown/HTML), and project memories. " +
		"Use workspace_search when you have a conceptual question (\"where is daemon locking handled?\") and want likely starting points. " +
		"Use search_in_files for exact literal or regex matches over current file contents (audits, replacement prep) — this tool is approximate by design and never a proof of absence. " +
		"Discovery ladder: workspace_search → topology/LSP tools → search_in_files → bounded read_file. " +
		"Results are FTS5-ranked within each corpus and interleaved; every hit is labelled with corpus, source, field, score, and why it matched, and the header reports per-corpus index freshness (exact_match=false always)."
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
		return fmt.Errorf("workspace_search: query is required")
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
	corpus  string // code | docs | memory
	source  string // topology-fts | memory-fts
	path    string // workspace-relative; "" for memories
	line    int    // 0 when unknown
	label   string // symbol "name (kind)" or memory name
	field   string
	score   float64
	snippet string
	why     string
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

// searchTopology queries the topology FTS index once and partitions the hits
// into the code and docs corpora (doc sections and Markdown/HTML files are
// docs; everything else is code).
func (t *WorkspaceSearch) searchTopology(ctx context.Context, a workspaceSearchArgs) (code, docs []wsHit, status string) {
	if !a.wants("code") && !a.wants("docs") {
		return nil, nil, "skipped"
	}
	store := t.storeFn()
	if store == nil {
		return nil, nil, "missing"
	}
	status = topologyIndexStatus(store)
	results, err := store.Search(ctx, a.Query, topology.SearchOpts{Limit: a.Limit * 2, Snippets: true})
	if err != nil {
		return nil, nil, status
	}
	for _, r := range results {
		h := wsHit{
			source:  "topology-fts",
			path:    r.Node.Path,
			line:    r.Node.StartLine,
			label:   fmt.Sprintf("%s (%s)", r.Node.Name, r.Node.Kind),
			field:   r.Field,
			score:   r.Score,
			snippet: r.Snippet,
		}
		if isDocNode(r.Node) {
			h.corpus, h.why = "docs", docWhy(r.Field)
			if a.wants("docs") {
				docs = append(docs, h)
			}
			continue
		}
		h.corpus, h.why = "code", codeWhy(r.Field)
		if a.wants("code") {
			code = append(code, h)
		}
	}
	return code, docs, status
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
		ws = t.ws()
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
	switch store.Status().IndexerState {
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
