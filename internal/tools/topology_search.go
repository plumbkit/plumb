package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/plumbkit/plumb/internal/topology"
)

var topologySearchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Search query. Terms are OR-matched against symbol names, tokenised identifiers (camelCase/snake_case split), qualified names, signatures, and docstrings."
    },
    "kinds": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Optional filter by node kinds: function, method, type, class, constant, variable, import, package, test."
    },
    "language": {
      "type": "string",
      "description": "Optional filter by language (e.g. 'go', 'python')."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of results to return. Default 20.",
      "default": 20
    },
    "include_snippets": {
      "type": "boolean",
      "description": "Include a short snippet showing the matching text. Default true.",
      "default": true
    },
    "rerank": {
      "type": "boolean",
      "description": "Re-rank FTS5 results by semantic similarity to the query (needs [semantics] enabled + an API key). Defaults to the [semantics].enabled config; pass false to force the plain FTS5 ranking, true to force re-rank when configured."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)

// TopologySearch performs a ranked FTS5 search over the topology index.
//
// Concurrency: Execute is safe for concurrent use.
type TopologySearch struct {
	storeFn func() *topology.Store
	semFn   func() SemanticRerankConfig // nil-safe; semantic re-rank disabled when unset
}

// NewTopologySearch returns a new TopologySearch tool.
// storeFn returns the current topology.Store for the session, or nil if disabled.
func NewTopologySearch(storeFn func() *topology.Store) *TopologySearch {
	return &TopologySearch{storeFn: storeFn}
}

// WithSemantics wires the semantic re-rank accessor (resolved live from
// [semantics] config by the daemon). Nil-safe; when unset, topology_search is
// the pure FTS5 baseline. Returns the receiver for chaining.
func (t *TopologySearch) WithSemantics(fn func() SemanticRerankConfig) *TopologySearch {
	t.semFn = fn
	return t
}

func (t *TopologySearch) semanticConfig() SemanticRerankConfig {
	if t.semFn == nil {
		return SemanticRerankConfig{}
	}
	return t.semFn()
}

func (*TopologySearch) Name() string                 { return "topology_search" }
func (*TopologySearch) InputSchema() json.RawMessage { return topologySearchSchema }
func (*TopologySearch) Description() string {
	return "Ranked FTS5 search over the topology index. Finds symbols, functions, types, " +
		"classes, and other named entities by name, tokenised identifier (camelCase/snake_case), " +
		"qualified name, signature, or docstring. Results include kind, file path, line range, " +
		"match field, score, and optional snippet. Source is 'topology' (approximate; use " +
		"search_in_files for exact filesystem matches). Returns a clear message when the index " +
		"is disabled or empty."
}

type topologySearchArgs struct {
	Query           string   `json:"query"`
	Kinds           []string `json:"kinds"`
	Language        string   `json:"language"`
	Limit           int      `json:"limit"`
	IncludeSnippets bool     `json:"include_snippets"`
	Rerank          *bool    `json:"rerank"` // nil = follow config; non-nil = force on/off
}

func (t *TopologySearch) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	a, err := parseTopologySearchArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	store := t.storeFn()
	if store == nil {
		return topologyDisabledMessage(), nil
	}

	// Re-rank when semantics is active and the caller did not opt out. When
	// re-ranking, over-fetch FTS5 candidates so the cosine pass has a pool.
	sem := t.semanticConfig()
	doRerank := sem.active() && (a.Rerank == nil || *a.Rerank)
	fetchLimit := a.Limit
	if doRerank && sem.candidates() > fetchLimit {
		fetchLimit = sem.candidates()
	}

	results, runErr := t.run(ctx, store, a, fetchLimit)
	if runErr != nil {
		return "", runErr
	}
	reranked := false
	if doRerank {
		results, reranked = rerankSearchResults(ctx, store, sem.Embedder, a.Query, results)
	}
	if len(results) > a.Limit {
		results = results[:a.Limit]
	}
	return formatTopologySearchResults(results, a, reranked), nil
}

func parseTopologySearchArgs(raw json.RawMessage) (topologySearchArgs, error) {
	var a topologySearchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("topology_search: invalid arguments: %w", err)
	}
	if a.Limit <= 0 {
		a.Limit = 20
	}
	return a, nil
}

func (a *topologySearchArgs) validate() error {
	if a.Query == "" {
		return fmt.Errorf("topology_search: query is required")
	}
	return nil
}

func (t *TopologySearch) run(ctx context.Context, store *topology.Store, a topologySearchArgs, fetchLimit int) ([]topology.SearchResult, error) {
	if store == nil {
		return nil, nil
	}
	opts := topology.SearchOpts{
		Kinds:    a.Kinds,
		Language: a.Language,
		Limit:    fetchLimit,
		Snippets: a.IncludeSnippets,
	}
	return store.Search(ctx, a.Query, opts)
}

func formatTopologySearchResults(results []topology.SearchResult, a topologySearchArgs, reranked bool) string {
	if len(results) == 0 {
		return fmt.Sprintf("topology_search: no results for %q", a.Query)
	}
	mode := "ranked"
	if reranked {
		mode = "fts+semantic"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology search: %d result(s) for %q (source=topology, mode=%s)\n\n", len(results), a.Query, mode)
	for _, r := range results {
		formatSearchResult(&sb, r, a.IncludeSnippets)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatSearchResult(sb *strings.Builder, r topology.SearchResult, includeSnippet bool) {
	n := r.Node
	fmt.Fprintf(sb, "  %s %s\n", string(n.Kind), n.Name)
	fmt.Fprintf(sb, "    path:  %s", n.Path)
	if n.StartLine > 0 {
		if n.EndLine > n.StartLine {
			fmt.Fprintf(sb, " L%d-%d", n.StartLine, n.EndLine)
		} else {
			fmt.Fprintf(sb, " L%d", n.StartLine)
		}
	}
	sb.WriteString("\n")
	if n.Signature != "" {
		fmt.Fprintf(sb, "    sig:   %s\n", n.Signature)
	}
	fmt.Fprintf(sb, "    field: %s  score: %.4f\n", r.Field, r.Score)
	if includeSnippet && r.Snippet != "" {
		fmt.Fprintf(sb, "    snip:  %s\n", r.Snippet)
	}
	sb.WriteString("\n")
}
