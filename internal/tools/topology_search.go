package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/golimpio/plumb/internal/topology"
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
    }
  },
  "required": ["query"]
}`)

// TopologySearch performs a ranked FTS5 search over the topology index.
//
// Concurrency: Execute is safe for concurrent use.
type TopologySearch struct {
	storeFn func() *topology.Store
}

// NewTopologySearch returns a new TopologySearch tool.
// storeFn returns the current topology.Store for the session, or nil if disabled.
func NewTopologySearch(storeFn func() *topology.Store) *TopologySearch {
	return &TopologySearch{storeFn: storeFn}
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
	results, runErr := t.run(ctx, store, a)
	if runErr != nil {
		return "", runErr
	}
	return formatTopologySearchResults(results, a), nil
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

func (t *TopologySearch) run(ctx context.Context, store *topology.Store, a topologySearchArgs) ([]topology.SearchResult, error) {
	if store == nil {
		return nil, nil
	}
	opts := topology.SearchOpts{
		Kinds:    a.Kinds,
		Language: a.Language,
		Limit:    a.Limit,
		Snippets: a.IncludeSnippets,
	}
	return store.Search(ctx, a.Query, opts)
}

func formatTopologySearchResults(results []topology.SearchResult, a topologySearchArgs) string {
	if results == nil {
		return "topology indexing is disabled for this session\n" +
			"Set [topology] enabled = true in .plumb/config.toml to enable."
	}
	if len(results) == 0 {
		return fmt.Sprintf("topology_search: no results for %q", a.Query)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "topology search: %d result(s) for %q (source=topology, mode=ranked)\n\n", len(results), a.Query)
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
