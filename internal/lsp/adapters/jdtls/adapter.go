package jdtls

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
)

// jdtlsInitOptions holds jdtls-specific initialisation options.
// jdtls reads most project config from pom.xml / build.gradle; these options
// let callers pass a Java-home override when JAVA_HOME is not set.
type jdtlsInitOptions struct {
	Settings *jdtlsSettings `json:"settings,omitempty"`
}

type jdtlsSettings struct {
	Java *jdtlsJavaSettings `json:"java,omitempty"`
}

// jdtlsJavaSettings mirrors the eclipse.jdt.ls java settings namespace.
type jdtlsJavaSettings struct {
	// Home overrides JAVA_HOME for the jdtls process. Leave empty to let jdtls
	// discover the JDK from JAVA_HOME or its own detection logic.
	Home string `json:"home,omitempty"`
}

// Adapter implements lsp.Client for Eclipse JDT Language Server (jdtls).
//
// jdtls uses the same JSON-RPC 2.0 / LSP protocol as gopls and pyright but
// requires a -data <dir> process argument (passed by the pool) and may send
// client/registerCapability requests for watched-file patterns during init.
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
}

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	a := &Adapter{
		conn: conn,
		subs: make(map[int64]func(string, json.RawMessage)),
	}
	conn.SetNotificationHandler(a.dispatch)
	conn.SetRequestHandler(a.handleServerRequest)
	return a
}

// handleServerRequest responds to server-initiated requests. jdtls sends
// client/registerCapability to register file-watcher patterns; we accept and
// record the glob patterns so DidChangeWatchedFiles can filter events.
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

// DefaultInitParams returns InitializeParams suitable for jdtls.
// rootURI must be a file:// URI pointing to the Java project root (the
// directory containing pom.xml or build.gradle).
// javaHome may be empty; if set it overrides JAVA_HOME inside jdtls.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	var opts jdtlsInitOptions
	if home := os.Getenv("JAVA_HOME"); home != "" {
		opts.Settings = &jdtlsSettings{Java: &jdtlsJavaSettings{Home: home}}
	}
	return protocol.InitializeParams{
		ProcessID:             protocol.ProcessID(),
		ClientInfo:            &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:               rootURI,
		Capabilities:          protocol.DefaultClientCapabilities(),
		InitializationOptions: opts,
	}
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

// Initialize sends the initialize request and stores the server capabilities.
func (a *Adapter) Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error) {
	var result protocol.InitializeResult
	if err := a.conn.Call(ctx, protocol.MethodInitialize, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls initialize: %w", err)
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
		return fmt.Errorf("jdtls initialized: %w", err)
	}
	return nil
}

// Shutdown requests a clean shutdown.
func (a *Adapter) Shutdown(ctx context.Context) error {
	if err := a.conn.Call(ctx, protocol.MethodShutdown, nil, nil); err != nil {
		return fmt.Errorf("jdtls shutdown: %w", err)
	}
	return nil
}

// Exit sends the exit notification.
func (a *Adapter) Exit(ctx context.Context) error {
	if err := a.conn.Notify(ctx, protocol.MethodExit, nil); err != nil {
		return fmt.Errorf("jdtls exit: %w", err)
	}
	return nil
}

// ── Document lifecycle ───────────────────────────────────────────────────────

// DidOpen notifies jdtls that a document has been opened.
func (a *Adapter) DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidOpen, params); err != nil {
		return fmt.Errorf("jdtls didOpen: %w", err)
	}
	return nil
}

// DidChange notifies jdtls of document changes.
func (a *Adapter) DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidChange, params); err != nil {
		return fmt.Errorf("jdtls didChange: %w", err)
	}
	return nil
}

// DidClose notifies jdtls that a document has been closed.
func (a *Adapter) DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error {
	if err := a.conn.Notify(ctx, protocol.MethodDidClose, params); err != nil {
		return fmt.Errorf("jdtls didClose: %w", err)
	}
	return nil
}

// DidChangeWatchedFiles notifies jdtls that one or more files changed on disk.
// Events are filtered to only those matching jdtls's registered glob patterns.
func (a *Adapter) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	params.Changes = a.watcher.FilterEvents(params.Changes)
	if len(params.Changes) == 0 {
		return nil
	}
	if err := a.conn.Notify(ctx, protocol.MethodDidChangeWatchedFiles, params); err != nil {
		return fmt.Errorf("jdtls didChangeWatchedFiles: %w", err)
	}
	return nil
}

// ── Queries ──────────────────────────────────────────────────────────────────

// DocumentSymbols returns all symbols in the document.
func (a *Adapter) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	var result []protocol.DocumentSymbol
	if err := a.conn.Call(ctx, protocol.MethodDocumentSymbols, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls documentSymbol: %w", err)
	}
	return result, nil
}

// WorkspaceSymbols searches for symbols matching the query.
func (a *Adapter) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	var result []protocol.SymbolInformation
	if err := a.conn.Call(ctx, protocol.MethodWorkspaceSymbols, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls workspaceSymbol: %w", err)
	}
	return result, nil
}

// Definition returns the definition location(s) for the symbol at pos.
func (a *Adapter) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	var result []protocol.Location
	if err := a.conn.Call(ctx, protocol.MethodDefinition, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls definition: %w", err)
	}
	return result, nil
}

// References returns all references to the symbol at pos.
func (a *Adapter) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	var result []protocol.Location
	if err := a.conn.Call(ctx, protocol.MethodReferences, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls references: %w", err)
	}
	return result, nil
}

// Hover returns hover information at pos.
func (a *Adapter) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	var result protocol.Hover
	if err := a.conn.Call(ctx, protocol.MethodHover, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls hover: %w", err)
	}
	return &result, nil
}

// ── Edits ────────────────────────────────────────────────────────────────────

// PrepareRename checks whether rename is valid at pos.
func (a *Adapter) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	var result protocol.PrepareRenameResult
	if err := a.conn.Call(ctx, protocol.MethodPrepareRename, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls prepareRename: %w", err)
	}
	return &result, nil
}

// Rename performs a workspace-wide rename.
func (a *Adapter) Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	var result protocol.WorkspaceEdit
	if err := a.conn.Call(ctx, protocol.MethodRename, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls rename: %w", err)
	}
	return &result, nil
}

// ── Call hierarchy ────────────────────────────────────────────────────────────

// PrepareCallHierarchy resolves the call-hierarchy item at pos.
func (a *Adapter) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	var result []protocol.CallHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodPrepareCallHierarchy, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls prepareCallHierarchy: %w", err)
	}
	return result, nil
}

// IncomingCalls returns the callers of item.
func (a *Adapter) IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	var result []protocol.CallHierarchyIncomingCall
	if err := a.conn.Call(ctx, protocol.MethodCallHierarchyIncoming, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls callHierarchy/incomingCalls: %w", err)
	}
	return result, nil
}

// OutgoingCalls returns the callees of item.
func (a *Adapter) OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	var result []protocol.CallHierarchyOutgoingCall
	if err := a.conn.Call(ctx, protocol.MethodCallHierarchyOutgoing, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls callHierarchy/outgoingCalls: %w", err)
	}
	return result, nil
}

// ── Type hierarchy ────────────────────────────────────────────────────────────

// PrepareTypeHierarchy resolves the type-hierarchy item at pos.
func (a *Adapter) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodPrepareTypeHierarchy, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls prepareTypeHierarchy: %w", err)
	}
	return result, nil
}

// Supertypes returns the supertypes of item.
func (a *Adapter) Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodTypeHierarchySuper, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls typeHierarchy/supertypes: %w", err)
	}
	return result, nil
}

// Subtypes returns the subtypes of item.
func (a *Adapter) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	var result []protocol.TypeHierarchyItem
	if err := a.conn.Call(ctx, protocol.MethodTypeHierarchySub, params, &result); err != nil {
		return nil, fmt.Errorf("jdtls typeHierarchy/subtypes: %w", err)
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
