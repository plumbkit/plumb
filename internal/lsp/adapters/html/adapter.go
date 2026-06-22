package html

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/lsp/watcher"
	"github.com/plumbkit/plumb/internal/paths"
)

// Adapter implements lsp.Client for vscode-html-language-server.
//
// The server expects a rootUri pointing at the project root and, like the other
// vscode-languageserver-based servers, registers file watchers dynamically via
// client/registerCapability — which the adapter answers so DidChangeWatchedFiles
// events are filtered to the registered globs.
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct {
	conn    jsonrpc.Caller
	watcher watcher.Filter

	capsMu sync.RWMutex
	caps   *protocol.ServerCapabilities

	subMu sync.RWMutex
	subID atomic.Int64
	subs  map[int64]func(string, json.RawMessage)

	// openMu guards open, the set of documents the adapter has sent didOpen
	// for. The HTML server has no filesystem access and answers document
	// queries (symbols, hover, diagnostics) only for open documents, so the
	// adapter opens a file from disk on first query and keeps it open;
	// DidChangeWatchedFiles closes any stale copy so the next query reopens it.
	openMu sync.Mutex
	open   map[string]bool
}

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	a := &Adapter{
		conn: conn,
		subs: make(map[int64]func(string, json.RawMessage)),
		open: make(map[string]bool),
	}
	conn.SetNotificationHandler(a.dispatch)
	conn.SetRequestHandler(a.handleServerRequest)
	return a
}

// handleServerRequest responds to server-initiated requests.
// vscode-html-language-server uses client/registerCapability to register file
// watchers; we accept and record the glob patterns so DidChangeWatchedFiles can
// filter events.
func (a *Adapter) handleServerRequest(_ context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case protocol.MethodRegisterCapability:
		a.watcher.Register(params)
		return nil, nil
	case protocol.MethodUnregisterCapability:
		a.watcher.Unregister(params)
		return nil, nil
	default:
		return nil, &jsonrpc.MethodNotFoundError{Method: method}
	}
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

// ── Lifecycle ────────────────────────────────────────────────────────────────

// Initialize sends the initialize request and stores the server capabilities.
func (a *Adapter) Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error) {
	var result protocol.InitializeResult
	if err := a.conn.Call(ctx, protocol.MethodInitialize, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server initialize: %w", err)
	}
	a.capsMu.Lock()
	caps := result.Capabilities
	a.caps = &caps
	a.capsMu.Unlock()
	return &result, nil
}

// Initialized sends the initialized notification.
func (a *Adapter) Initialized(ctx context.Context) error {
	if err := a.conn.Notify(ctx, protocol.MethodInitialized, struct{}{}); err != nil {
		return fmt.Errorf("vscode-html-language-server initialized: %w", err)
	}
	return nil
}

// Shutdown requests a clean shutdown.
func (a *Adapter) Shutdown(ctx context.Context) error {
	if err := a.conn.Call(ctx, protocol.MethodShutdown, nil, nil); err != nil {
		return fmt.Errorf("vscode-html-language-server shutdown: %w", err)
	}
	return nil
}

// Exit sends the exit notification.
func (a *Adapter) Exit(ctx context.Context) error {
	if err := a.conn.Notify(ctx, protocol.MethodExit, nil); err != nil {
		return fmt.Errorf("vscode-html-language-server exit: %w", err)
	}
	return nil
}

// ── Document lifecycle ───────────────────────────────────────────────────────

// DidOpen notifies the server that a document has been opened.
func (a *Adapter) DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidOpen, params); err != nil {
		return fmt.Errorf("vscode-html-language-server didOpen: %w", err)
	}
	return nil
}

// ensureOpen makes sure uri's current on-disk content is open on the server.
// vscode-html-language-server has no filesystem access — it answers document
// queries only for documents opened via textDocument/didOpen — so plumb opens
// the file lazily before the first query and keeps it open. Already-open
// documents are left untouched. Safe for concurrent use.
func (a *Adapter) ensureOpen(ctx context.Context, uri string) error {
	a.openMu.Lock()
	defer a.openMu.Unlock()
	if a.open[uri] {
		return nil
	}
	content, err := os.ReadFile(paths.URIToPath(uri))
	if err != nil {
		return fmt.Errorf("vscode-html-language-server open %s: %w", uri, err)
	}
	if err := a.conn.Notify(ctx, protocol.MethodDidOpen, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, LanguageID: "html", Version: 1, Text: string(content)},
	}); err != nil {
		return fmt.Errorf("vscode-html-language-server didOpen: %w", err)
	}
	a.open[uri] = true
	return nil
}

// refreshOpen closes any open document that changed on disk so the next query
// reopens it with fresh content — didChangeWatchedFiles does not update the
// server's open-document copy.
func (a *Adapter) refreshOpen(ctx context.Context, changes []protocol.FileEvent) {
	a.openMu.Lock()
	defer a.openMu.Unlock()
	for _, c := range changes {
		if a.open[c.URI] {
			_ = a.conn.Notify(ctx, protocol.MethodDidClose, protocol.DidCloseTextDocumentParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: c.URI},
			})
			delete(a.open, c.URI)
		}
	}
}

// DidChange notifies the server of document changes.
func (a *Adapter) DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidChange, params); err != nil {
		return fmt.Errorf("vscode-html-language-server didChange: %w", err)
	}
	return nil
}

// DidClose notifies the server that a document has been closed.
func (a *Adapter) DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidClose, params); err != nil {
		return fmt.Errorf("vscode-html-language-server didClose: %w", err)
	}
	return nil
}

// DidChangeWatchedFiles notifies the server that one or more files changed on
// disk. Events are filtered to only those matching its registered glob patterns.
func (a *Adapter) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	a.refreshOpen(ctx, params.Changes)
	params.Changes = a.watcher.FilterEvents(params.Changes)
	if len(params.Changes) == 0 {
		return nil
	}
	if err := a.conn.Notify(ctx, protocol.MethodDidChangeWatchedFiles, params); err != nil {
		return fmt.Errorf("vscode-html-language-server didChangeWatchedFiles: %w", err)
	}
	return nil
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
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := a.conn.Call(ctx, protocol.MethodDocumentSymbols, params, &raw); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server documentSymbol: %w", err)
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

// WorkspaceSymbols searches for symbols matching the query. The HTML server does
// not implement workspace/symbol; the call forwards and returns whatever the
// server replies (typically an empty set).
func (a *Adapter) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	var result []protocol.SymbolInformation
	if err := a.conn.Call(ctx, protocol.MethodWorkspaceSymbols, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server workspaceSymbol: %w", err)
	}
	return result, nil
}

// Definition returns the definition location(s) for the symbol at pos.
func (a *Adapter) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result []protocol.Location
	if err := a.conn.Call(ctx, protocol.MethodDefinition, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server definition: %w", err)
	}
	return result, nil
}

// References returns all references to the symbol at pos.
func (a *Adapter) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result []protocol.Location
	if err := a.conn.Call(ctx, protocol.MethodReferences, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server references: %w", err)
	}
	return result, nil
}

// Hover returns hover information at pos.
func (a *Adapter) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result protocol.Hover
	if err := a.conn.Call(ctx, protocol.MethodHover, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server hover: %w", err)
	}
	return &result, nil
}

// ── Edits ────────────────────────────────────────────────────────────────────

// PrepareRename checks whether rename is valid at pos.
func (a *Adapter) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	var result protocol.PrepareRenameResult
	if err := a.conn.Call(ctx, protocol.MethodPrepareRename, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server prepareRename: %w", err)
	}
	return &result, nil
}

// Rename performs a workspace-wide rename. The HTML server scopes renames to
// matching tag pairs / linked editing ranges within a document.
func (a *Adapter) Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	var result protocol.WorkspaceEdit
	if err := a.conn.Call(ctx, protocol.MethodRename, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server rename: %w", err)
	}
	return &result, nil
}

// ── Call hierarchy ────────────────────────────────────────────────────────────

func (a *Adapter) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	var result []protocol.CallHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodPrepareCallHierarchy, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server prepareCallHierarchy: %w", err)
	}
	return result, nil
}

func (a *Adapter) IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	var result []protocol.CallHierarchyIncomingCall
	if err := a.conn.Call(ctx, protocol.MethodCallHierarchyIncoming, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server callHierarchy/incomingCalls: %w", err)
	}
	return result, nil
}

func (a *Adapter) OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	var result []protocol.CallHierarchyOutgoingCall
	if err := a.conn.Call(ctx, protocol.MethodCallHierarchyOutgoing, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server callHierarchy/outgoingCalls: %w", err)
	}
	return result, nil
}

// ── Type hierarchy ────────────────────────────────────────────────────────────

func (a *Adapter) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodPrepareTypeHierarchy, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server prepareTypeHierarchy: %w", err)
	}
	return result, nil
}

func (a *Adapter) Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodTypeHierarchySuper, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server typeHierarchy/supertypes: %w", err)
	}
	return result, nil
}

func (a *Adapter) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodTypeHierarchySub, params, &result); err != nil {
		return nil, fmt.Errorf("vscode-html-language-server typeHierarchy/subtypes: %w", err)
	}
	return result, nil
}

// ── Capabilities / subscriptions ─────────────────────────────────────────────

// Capabilities returns the negotiated server capabilities, or nil before Initialize.
func (a *Adapter) Capabilities() *protocol.ServerCapabilities {
	a.capsMu.RLock()
	defer a.capsMu.RUnlock()
	return a.caps
}

// Subscribe registers a notification handler and returns an unsubscribe func.
func (a *Adapter) Subscribe(handler func(string, json.RawMessage)) func() {
	id := a.subID.Add(1)
	a.subMu.Lock()
	a.subs[id] = handler
	a.subMu.Unlock()
	return func() {
		a.subMu.Lock()
		delete(a.subs, id)
		a.subMu.Unlock()
	}
}

// dispatch fans out a server notification to all subscribers.
func (a *Adapter) dispatch(method string, params json.RawMessage) {
	a.subMu.RLock()
	handlers := make([]func(string, json.RawMessage), 0, len(a.subs))
	for _, h := range a.subs {
		handlers = append(handlers, h)
	}
	a.subMu.RUnlock()
	for _, h := range handlers {
		h(method, params)
	}
}
