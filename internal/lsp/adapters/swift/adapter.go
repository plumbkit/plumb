package swift

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/lsp/watcher"
)

// Adapter implements lsp.Client for sourcekit-lsp.
//
// sourcekit-lsp expects a rootUri pointing at the SwiftPM package root (the
// directory containing Package.swift) and derives per-file compiler arguments
// from the package's build plan. It registers file watchers dynamically via
// client/registerCapability, which the adapter answers so DidChangeWatchedFiles
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

	// openMu guards open, the set of documents the adapter has sent didOpen for.
	// sourcekit-lsp serves per-document requests (symbols, definition, references,
	// hover, hierarchies) only for documents opened via textDocument/didOpen — for
	// an unopened file it replies -32001 "No language service for <uri> found".
	// plumb's external-edit model uses didChangeWatchedFiles, not the open-document
	// lifecycle, so the adapter opens a file lazily before the first query and
	// keeps it open; DidChangeWatchedFiles drops a stale copy so the next query
	// reopens it. Mirrors the html adapter.
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

// handleServerRequest responds to server-initiated requests. sourcekit-lsp uses
// client/registerCapability to register file watchers; we accept and record the
// glob patterns so DidChangeWatchedFiles can filter events.
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

// DefaultInitParams returns InitializeParams suitable for sourcekit-lsp.
// rootURI must be a file:// URI pointing to the SwiftPM package root.
// sourcekit-lsp needs no initialization options for plumb's use — it derives
// build settings from the package — so none are sent.
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
		return nil, fmt.Errorf("sourcekit-lsp initialize: %w", err)
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
		return fmt.Errorf("sourcekit-lsp initialized: %w", err)
	}
	return nil
}

// Shutdown requests a clean shutdown.
func (a *Adapter) Shutdown(ctx context.Context) error {
	if err := a.conn.Call(ctx, protocol.MethodShutdown, nil, nil); err != nil {
		return fmt.Errorf("sourcekit-lsp shutdown: %w", err)
	}
	return nil
}

// Exit sends the exit notification.
func (a *Adapter) Exit(ctx context.Context) error {
	if err := a.conn.Notify(ctx, protocol.MethodExit, nil); err != nil {
		return fmt.Errorf("sourcekit-lsp exit: %w", err)
	}
	return nil
}

// ── Document lifecycle ───────────────────────────────────────────────────────

// DidOpen notifies sourcekit-lsp that a document has been opened.
func (a *Adapter) DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidOpen, params); err != nil {
		return fmt.Errorf("sourcekit-lsp didOpen: %w", err)
	}
	return nil
}

// ensureOpen makes sure uri's current on-disk content is open on the server.
// sourcekit-lsp serves per-document requests only for documents opened via
// textDocument/didOpen — for an unopened file it replies -32001 "No language
// service for <uri> found". plumb opens the file lazily before the first query
// and keeps it open. Already-open documents are left untouched. Safe for
// concurrent use.
func (a *Adapter) ensureOpen(ctx context.Context, uri string) error {
	a.openMu.Lock()
	defer a.openMu.Unlock()
	if a.open[uri] {
		return nil
	}
	content, err := os.ReadFile(strings.TrimPrefix(uri, "file://"))
	if err != nil {
		return fmt.Errorf("sourcekit-lsp open %s: %w", uri, err)
	}
	if err := a.conn.Notify(ctx, protocol.MethodDidOpen, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{URI: uri, LanguageID: "swift", Version: 1, Text: string(content)},
	}); err != nil {
		return fmt.Errorf("sourcekit-lsp didOpen: %w", err)
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

// DidChange notifies sourcekit-lsp of document changes.
func (a *Adapter) DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidChange, params); err != nil {
		return fmt.Errorf("sourcekit-lsp didChange: %w", err)
	}
	return nil
}

// DidClose notifies sourcekit-lsp that a document has been closed.
func (a *Adapter) DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidClose, params); err != nil {
		return fmt.Errorf("sourcekit-lsp didClose: %w", err)
	}
	return nil
}

// DidChangeWatchedFiles notifies sourcekit-lsp that one or more files changed on
// disk. Events are filtered to only those matching its registered glob patterns.
func (a *Adapter) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	a.refreshOpen(ctx, params.Changes)
	params.Changes = a.watcher.FilterEvents(params.Changes)
	if len(params.Changes) == 0 {
		return nil
	}
	if err := a.conn.Notify(ctx, protocol.MethodDidChangeWatchedFiles, params); err != nil {
		return fmt.Errorf("sourcekit-lsp didChangeWatchedFiles: %w", err)
	}
	return nil
}

// ── Queries ──────────────────────────────────────────────────────────────────

// DocumentSymbols returns all symbols in the document.
func (a *Adapter) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result []protocol.DocumentSymbol
	if err := a.conn.Call(ctx, protocol.MethodDocumentSymbols, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp documentSymbol: %w", err)
	}
	return result, nil
}

// WorkspaceSymbols searches for symbols matching the query.
func (a *Adapter) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	var result []protocol.SymbolInformation
	if err := a.conn.Call(ctx, protocol.MethodWorkspaceSymbols, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp workspaceSymbol: %w", err)
	}
	return result, nil
}

// Definition returns the definition location(s) for the symbol at pos.
func (a *Adapter) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	// definition returns Location | Location[] | LocationLink[] | null (and some
	// servers a bare single Location), so decode the union, not just []Location.
	var raw json.RawMessage
	if err := a.conn.Call(ctx, protocol.MethodDefinition, params, &raw); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp definition: %w", err)
	}
	locs, err := protocol.DecodeLocations(raw)
	if err != nil {
		return nil, fmt.Errorf("sourcekit-lsp definition: %w", err)
	}
	return locs, nil
}

// References returns all references to the symbol at pos.
func (a *Adapter) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result []protocol.Location
	if err := a.conn.Call(ctx, protocol.MethodReferences, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp references: %w", err)
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
		return nil, fmt.Errorf("sourcekit-lsp hover: %w", err)
	}
	return &result, nil
}

// ── Edits ────────────────────────────────────────────────────────────────────

// PrepareRename checks whether rename is valid at pos.
func (a *Adapter) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result protocol.PrepareRenameResult
	if err := a.conn.Call(ctx, protocol.MethodPrepareRename, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp prepareRename: %w", err)
	}
	return &result, nil
}

// Rename performs a workspace-wide rename.
func (a *Adapter) Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	var result protocol.WorkspaceEdit
	if err := a.conn.Call(ctx, protocol.MethodRename, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp rename: %w", err)
	}
	return &result, nil
}

// ── Call hierarchy ────────────────────────────────────────────────────────────

func (a *Adapter) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result []protocol.CallHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodPrepareCallHierarchy, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp prepareCallHierarchy: %w", err)
	}
	return result, nil
}

func (a *Adapter) IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	var result []protocol.CallHierarchyIncomingCall
	if err := a.conn.Call(ctx, protocol.MethodCallHierarchyIncoming, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp callHierarchy/incomingCalls: %w", err)
	}
	return result, nil
}

func (a *Adapter) OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	var result []protocol.CallHierarchyOutgoingCall
	if err := a.conn.Call(ctx, protocol.MethodCallHierarchyOutgoing, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp callHierarchy/outgoingCalls: %w", err)
	}
	return result, nil
}

// ── Type hierarchy ────────────────────────────────────────────────────────────

func (a *Adapter) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	if err := a.ensureOpen(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodPrepareTypeHierarchy, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp prepareTypeHierarchy: %w", err)
	}
	return result, nil
}

func (a *Adapter) Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodTypeHierarchySuper, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp typeHierarchy/supertypes: %w", err)
	}
	return result, nil
}

func (a *Adapter) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodTypeHierarchySub, params, &result); err != nil {
		return nil, fmt.Errorf("sourcekit-lsp typeHierarchy/subtypes: %w", err)
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
