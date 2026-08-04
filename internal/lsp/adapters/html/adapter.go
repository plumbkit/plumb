package html

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Adapter implements lsp.Client for vscode-html-language-server.
//
// The server expects a rootUri pointing at the project root and, like the other
// vscode-languageserver-based servers, registers file watchers dynamically via
// client/registerCapability — which the embedded base answers so
// DidChangeWatchedFiles events are filtered to the registered globs.
//
// The 23 lsp.Client methods come from base.Adapter, which labels each error
// "vscode-html-language-server <label>: <cause>". This package overrides the
// per-document queries the server answers only for open documents (it has no
// filesystem access) and DocumentSymbols, whose reply needs a union decode.
// The rename and hierarchy prepares are NOT among them: the HTML server answers
// those from the document it already holds.
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct {
	*base.Adapter

	// open is held as a NAMED field, never embedded: embedding it would promote
	// Ensure and Refresh into this adapter's exported surface, which plumb
	// resolves structurally (see the base package doc).
	open *base.OpenTracker
}

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	b := base.New(conn, "vscode-html-language-server")
	return &Adapter{Adapter: b, open: base.NewOpenTracker(b, "html")}
}

// DefaultInitParams returns InitializeParams suitable for
// vscode-html-language-server. rootURI must be a file:// URI pointing to the
// project root. No initialization options are required for plumb's use — the
// document-symbol, hover, and completion providers work with the defaults.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
	}
}

// ── Document lifecycle ───────────────────────────────────────────────────────

// DidChangeWatchedFiles notifies the server that one or more files changed on
// disk, first dropping any lazily-opened copy of a changed document so the next
// query reopens it with fresh content. The tracker sees the UNFILTERED changes:
// plumb's copy is stale whatever the server's watcher globs say. The base then
// filters and forwards, unaffected by this — params is a value.
func (a *Adapter) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	a.open.Refresh(ctx, params.Changes)
	return a.Adapter.DidChangeWatchedFiles(ctx, params)
}

// ── Queries ──────────────────────────────────────────────────────────────────

// DocumentSymbols returns all symbols in the document.
//
// vscode-html-language-server returns the legacy flat SymbolInformation[] shape
// (range under location.range) rather than the hierarchical DocumentSymbol[]
// (range under .range). Decoding straight into []DocumentSymbol silently drops
// every range to the zero value (all symbols at L1). We decode the
// DocumentSymbol | SymbolInformation union and map location.range → Range so
// outlines carry real line numbers regardless of which shape the server sends.
func (a *Adapter) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	raw, err := base.CallRaw(ctx, a.Adapter, "documentSymbol", protocol.MethodDocumentSymbols, params)
	if err != nil {
		return nil, err
	}
	return decodeDocumentSymbolUnion(raw)
}

// htmlSymbolNode is a hybrid of DocumentSymbol and SymbolInformation: it carries
// both the hierarchical `range`/`children` and the flat `location`, so one
// decode handles whichever shape the server sends.
type htmlSymbolNode struct {
	Name     string              `json:"name"`
	Kind     protocol.SymbolKind `json:"kind"`
	Detail   string              `json:"detail"`
	Range    protocol.Range      `json:"range"`
	Location protocol.Location   `json:"location"`
	Children []htmlSymbolNode    `json:"children"`
}

// decodeDocumentSymbolUnion parses either shape into []protocol.DocumentSymbol,
// preferring the hierarchical range and falling back to the flat
// location.range when the former is absent (zero).
func decodeDocumentSymbolUnion(raw json.RawMessage) ([]protocol.DocumentSymbol, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var nodes []htmlSymbolNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server documentSymbol: decoding symbols: %w", err)
	}
	return htmlNodesToSymbols(nodes), nil
}

func htmlNodesToSymbols(nodes []htmlSymbolNode) []protocol.DocumentSymbol {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]protocol.DocumentSymbol, 0, len(nodes))
	for _, n := range nodes {
		rng := n.Range
		if rng == (protocol.Range{}) {
			rng = n.Location.Range // flat SymbolInformation shape
		}
		out = append(out, protocol.DocumentSymbol{
			Name:           n.Name,
			Detail:         n.Detail,
			Kind:           n.Kind,
			Range:          rng,
			SelectionRange: rng,
			Children:       htmlNodesToSymbols(n.Children),
		})
	}
	return out
}

// Definition returns the definition location(s) for the symbol at pos.
//
// Unlike swift and zig this decodes a plain []Location: the HTML server answers
// the array shape.
func (a *Adapter) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.Definition(ctx, params)
}

// References returns all references to the symbol at pos.
func (a *Adapter) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.References(ctx, params)
}

// Hover returns hover information at pos.
func (a *Adapter) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.Hover(ctx, params)
}
