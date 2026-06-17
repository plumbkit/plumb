package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/topology"
)

var workspaceSymbolsSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Symbol name or substring to search for across the entire workspace"
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)

// WorkspaceSymbols searches for symbols by name across the entire workspace.
type WorkspaceSymbols struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	ws      WorkspaceFn // used to filter out dependency-cache hits
	topo    topologyStoreFn
}

// WithTopologyFallback wires the topology index as a fallback for when the
// language server errors or times out. Returns the tool for chaining.
func (t *WorkspaceSymbols) WithTopologyFallback(fn topologyStoreFn) *WorkspaceSymbols {
	t.topo = fn
	return t
}

// NewWorkspaceSymbols creates a WorkspaceSymbols tool. ws may be nil, in
// which case no workspace-scoping filter is applied.
func NewWorkspaceSymbols(client lsp.Client, c *cache.Cache, ttl, timeout time.Duration, ws WorkspaceFn) *WorkspaceSymbols {
	return &WorkspaceSymbols{client: client, cache: c, ttl: ttl, timeout: timeout, ws: ws}
}

func (t *WorkspaceSymbols) Name() string                 { return "workspace_symbols" }
func (t *WorkspaceSymbols) InputSchema() json.RawMessage { return workspaceSymbolsSchema }
func (t *WorkspaceSymbols) Description() string {
	return "No native Claude Code equivalent. " +
		"Search for symbols (functions, types, variables, constants) by name or substring across the entire workspace — instant, uses the LSP index. " +
		"Prefer this over search_in_files or grep when looking up a symbol by name. " +
		"Returns names, kinds, and source locations."
}

type workspaceSymbolsArgs struct {
	Query string `json:"query"`
}

// topologyFallback answers a workspace-wide symbol search from the topology
// index. ok is false when topology is unavailable or returns nothing, so the
// caller surfaces the original LSP error instead of an empty index result.
func (t *WorkspaceSymbols) topologyFallback(ctx context.Context, query string) (string, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return "", false
	}
	results, err := store.Search(ctx, query, topology.SearchOpts{Limit: 100})
	if err != nil || len(results) == 0 {
		return "", false
	}
	nodes := make([]topology.Node, 0, len(results))
	for _, r := range results {
		nodes = append(nodes, r.Node)
	}
	return formatTopologyMatches(fmt.Sprintf("Found %d symbol(s) matching %q", len(nodes), query), nodes), true
}

// topologyFillTreeSitter supplements an empty-but-no-error LSP result with index
// hits for tree-sitter-backed languages. Lazy servers (zls and the other
// on-demand indexers) only return workspace/symbol hits for files they have
// already analysed, so a freshly-attached session legitimately returns [] for a
// symbol that exists — short-circuiting "No symbols found" when the Map knows it.
// Native-AST languages (Go via gopls, which indexes the whole workspace eagerly)
// are excluded: an empty authoritative answer there must not be supplanted by
// approximate index matches.
func (t *WorkspaceSymbols) topologyFillTreeSitter(ctx context.Context, query string) (string, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return "", false
	}
	results, err := store.Search(ctx, query, topology.SearchOpts{Limit: 100})
	if err != nil || len(results) == 0 {
		return "", false
	}
	nodes := make([]topology.Node, 0, len(results))
	for _, r := range results {
		if lang, ok := langsupport.ByName(r.Node.Language); ok && lang.Structural == langsupport.EngineTreeSitter {
			nodes = append(nodes, r.Node)
		}
	}
	if len(nodes) == 0 {
		return "", false
	}
	return formatTopologyFill(fmt.Sprintf("Found %d symbol(s) matching %q", len(nodes), query), nodes), true
}

func (t *WorkspaceSymbols) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a workspaceSymbolsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("workspace_symbols: invalid arguments: %w", err)
	}
	if a.Query == "" {
		return "", fmt.Errorf("workspace_symbols: query must not be empty")
	}

	key := "wsSymbols:" + a.Query
	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			return v.(string), nil
		}
	}

	lspCtx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	syms, err := t.client.WorkspaceSymbols(lspCtx, protocol.WorkspaceSymbolParams{Query: a.Query})
	if err != nil {
		if out, ok := t.topologyFallback(ctx, a.Query); ok {
			return out, nil
		}
		return "", lspTimeoutErr("workspace_symbols", t.timeout, err)
	}

	// Drop dependency-cache and stdlib hits so results stay focused on the
	// user's own code.
	if t.ws != nil {
		ws := t.ws()
		filtered := syms[:0]
		for _, s := range syms {
			if isInWorkspace(s.Location.URI, ws) {
				filtered = append(filtered, s)
			}
		}
		syms = filtered
	}

	var result string
	if len(syms) == 0 {
		if out, ok := t.topologyFillTreeSitter(ctx, a.Query); ok {
			return out, nil
		}
		result = fmt.Sprintf("No symbols found matching %q.", a.Query)
	} else {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Found %d symbol(s) matching %q:\n\n", len(syms), a.Query)
		for _, s := range syms {
			fmt.Fprintf(&sb, "- %s (%s) at %s:%d\n",
				s.Name, symbolKindName(s.Kind),
				s.Location.URI, s.Location.Range.Start.Line+1)
		}
		result = sb.String()
	}

	if t.cache != nil {
		t.cache.Set(key, result, t.ttl)
	}
	return result, nil
}
