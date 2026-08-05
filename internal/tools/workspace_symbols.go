package tools

import (
	"context"
	"encoding/json"
	"errors"
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
      "description": "Symbol name or substring to search for (case-insensitive)"
    },
    "uri": {
      "type": "string",
      "description": "Optional: restrict the search to this ONE document (absolute path, file:// URI, or workspace-relative path). Omit it to search the whole workspace."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)

// WorkspaceSymbols searches for symbols by name across the entire workspace,
// or — when a uri is given — within that one document.
type WorkspaceSymbols struct {
	client  lsp.Client
	cache   *cache.Cache
	ttl     time.Duration
	timeout time.Duration
	// ws resolves the pinned workspace root and serves BOTH modes, in two
	// distinct ways: the workspace-wide search filters results to the root
	// (dropping dependency-cache and stdlib hits), while a uri-scoped call
	// anchors a workspace-relative uri against it. Nil-safe — no filtering, no
	// anchoring.
	ws     WorkspaceFn
	topo   topologyStoreFn
	warmup LSPWarmupFn // may be nil; distinguishes a warming server from an unavailable one in the fallback note
	xcode  XcodeHintFn
	proof  XcodeProofFn
}

// WithXcodeHint wires guidance for empty SourceKit-LSP results in bare Xcode projects.
func (t *WorkspaceSymbols) WithXcodeHint(fn XcodeHintFn) *WorkspaceSymbols {
	t.xcode = fn
	return t
}

// WithXcodeProof records non-empty SourceKit-LSP workspace-symbol results.
func (t *WorkspaceSymbols) WithXcodeProof(fn XcodeProofFn) *WorkspaceSymbols {
	t.proof = fn
	return t
}

// WithTopologyFallback wires the topology index as a fallback for when the
// language server errors or times out. Returns the tool for chaining.
func (t *WorkspaceSymbols) WithTopologyFallback(fn topologyStoreFn) *WorkspaceSymbols {
	t.topo = fn
	return t
}

// WithLSPWarmup wires the warm-up probe so the topology-fallback note says
// "still warming — retry shortly" instead of "LSP unavailable" while the
// primary server's handshake completes. Nil-safe; returns the tool for chaining.
func (t *WorkspaceSymbols) WithLSPWarmup(fn LSPWarmupFn) *WorkspaceSymbols {
	t.warmup = fn
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
	return "Search for symbols (functions, types, variables, constants) by name or substring across the entire workspace — instant, uses the LSP index. " +
		"Pass uri to restrict the search to that one document instead. " +
		"Returns names, kinds, and source locations."
}

type workspaceSymbolsArgs struct {
	Query string `json:"query"`
	URI   string `json:"uri"`
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
	// The workspace-wide search has no single target file, so the warm-up probe
	// inspects the connection primary (empty uri).
	note := topologyFallbackNoteFor(t.warmup, "")
	return formatTopologyMatches(note, fmt.Sprintf("Found %d symbol(s) matching %q", len(nodes), query), nodes), true
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

func hasSwiftWorkspaceSymbol(symbols []protocol.SymbolInformation) bool {
	for _, symbol := range symbols {
		if strings.HasSuffix(strings.ToLower(symbol.Location.URI), ".swift") {
			return true
		}
	}
	return false
}

func (t *WorkspaceSymbols) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a workspaceSymbolsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("workspace_symbols: invalid arguments: %w", err)
	}
	if a.Query == "" {
		return "", errors.New("workspace_symbols: query must not be empty")
	}
	if a.URI != "" {
		return t.inFile(ctx, toFileURIAnchored(a.URI, t.ws), a.Query)
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

	recordXcodeProof(t.proof, hasSwiftWorkspaceSymbol(syms))
	var result string
	if len(syms) == 0 {
		if out, ok := t.topologyFillTreeSitter(ctx, a.Query); ok {
			return out, nil
		}
		result = appendXcodeHint(fmt.Sprintf("No symbols found matching %q.", a.Query), "", t.xcode)
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

// inFile serves the uri-scoped mode: a single-document symbol search over the
// language server's documentSymbol tree, with the topology index as the
// fallback when the server errors or times out.
func (t *WorkspaceSymbols) inFile(ctx context.Context, uri, query string) (string, error) {
	lspCtx, cancel := withLSPDeadline(ctx, t.timeout)
	defer cancel()
	out, err := t.inDocument(lspCtx, uri, query)
	if err != nil {
		if IsWorkspaceBoundaryError(err) {
			return "", err
		}
		if fb, ok := t.topologyFallbackInFile(ctx, uri, query); ok {
			return fb, nil
		}
		return "", err
	}
	return out, nil
}

// topologyFallbackInFile answers an in-file symbol search from the topology
// index. ok is false when topology is unavailable or has not indexed the file,
// so the caller surfaces the original LSP error instead. It is the file-scoped
// counterpart of topologyFallback, which searches the whole index.
func (t *WorkspaceSymbols) topologyFallbackInFile(ctx context.Context, uri, query string) (string, bool) {
	store := activeTopology(t.topo)
	if store == nil {
		return "", false
	}
	nodes, err := store.SymbolsInFile(ctx, uri)
	if err != nil || len(nodes) == 0 {
		return "", false
	}
	matches := filterTopologyByName(nodes, query)
	note := topologyFallbackNoteFor(t.warmup, uri)
	return formatTopologyMatches(note, fmt.Sprintf("Symbols matching %q in %s", query, uri), matches), true
}

func (t *WorkspaceSymbols) inDocument(ctx context.Context, uri, query string) (string, error) {
	// Cache the full symbol list per document; filtering is client-side.
	key := uri + ":docSymbols"
	var syms []protocol.DocumentSymbol

	if t.cache != nil {
		if v, ok := t.cache.Get(key); ok {
			syms = v.([]protocol.DocumentSymbol)
		}
	}
	if syms == nil {
		var err error
		syms, err = t.client.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err != nil {
			return "", lspTimeoutErr("workspace_symbols", t.timeout, err)
		}
		if t.cache != nil {
			t.cache.Set(key, syms, t.ttl)
		}
	}

	if len(syms) == 0 {
		// Server answered empty — fall back to the structural Map for file types
		// the workspace LSP does not cover (e.g. .html in a Go repo).
		if fb, ok := t.topologyFallbackInFile(ctx, uri, query); ok {
			return fb, nil
		}
	}
	matches := flatFilterSymbols(syms, query)
	if len(matches) == 0 {
		return fmt.Sprintf("No symbols matching %q in %s.", query, uri), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Symbols matching %q in %s:\n\n", query, uri)
	for _, s := range matches {
		fmt.Fprintf(&sb, "- %s (%s) at line %d\n",
			s.Name, symbolKindName(s.Kind), s.Range.Start.Line+1)
	}
	return sb.String(), nil
}
