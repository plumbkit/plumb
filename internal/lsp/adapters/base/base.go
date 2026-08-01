// Package base holds the half of a plumb LSP adapter that is identical across
// every language server: the JSON-RPC plumbing behind the 23 lsp.Client
// methods, the negotiated-capability cache, the notification fan-out, and the
// server-request handler that records watcher registrations.
//
// # The no-extra-exported-methods rule
//
// Adapter exposes EXACTLY the 23 methods of lsp.Client and nothing more. This
// is a hard constraint, not a stylistic preference.
//
// Go promotes every exported method of an embedded type into its embedder, and
// plumb resolves optional adapter capabilities STRUCTURALLY rather than by
// declaration: internal/cli/pool_adapters.go asserts lsp.PullInitializer,
// internal/cli/routing_proxy_pull.go and internal/lsp/conformance/conformance.go
// assert the document-pull shape (SupportsPullDiagnostics + Diagnostic) and the
// workspace-pull shape (WorkspaceDiagnostic). One extra exported method here is
// therefore promoted into every embedding adapter at once — a single stray
// SupportsPullDiagnostics would opt language servers that answer -32601 to
// textDocument/diagnostic into the pull model. That regression compiles,
// changes no call site, and surfaces only as broken diagnostics at runtime.
//
// So every escape hatch this package offers an adapter is a package-level
// FUNCTION — Call, CallPtr, CallRaw, Notify, Wrap. Functions are never
// promoted. An adapter needing a method beyond lsp.Client declares it in its
// own package, implemented over those helpers; see the typescript adapter's
// Diagnostic. conformance.TestAdapters_OptionalInterfaceSurface fails loudly
// if this rule is broken.
package base

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/lsp/watcher"
)

// Adapter is the shared half of a plumb LSP adapter. Language adapters embed
// *Adapter and add only what is genuinely server-specific: their
// DefaultInitParams, their initialisation-options structs, and any optional
// capability method (which must live in their own package — see the package
// doc).
//
// Concurrency: all exported methods are safe for concurrent use. Adapter holds
// mutexes and is used only through a pointer — never copy it, and never embed
// it by value.
type Adapter struct {
	conn jsonrpc.Caller

	// server is the language-server binary name. It prefixes every error this
	// adapter returns, so the string is user-visible and pinned by
	// conformance.RunErrorContract.
	server string

	watcher watcher.Filter

	capsMu sync.RWMutex
	caps   *protocol.ServerCapabilities

	subMu sync.RWMutex
	subID atomic.Int64
	subs  map[int64]func(string, json.RawMessage)
}

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever an embedding adapter is used as an
// lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn, labelling every error it returns with
// server. The caller must call Initialize before any query method.
//
// New installs BOTH transport handlers — the notification fan-out and the
// server-request handler. Wiring them here rather than leaving it to each
// adapter's own New is what makes the "forgot SetRequestHandler" bug
// unrepresentable: dispatch and handleServerRequest are unexported, so even
// though they are promoted into an embedder they are not selectable from
// another package and cannot be re-wired (or forgotten) there.
func New(conn jsonrpc.Caller, server string) *Adapter {
	a := &Adapter{
		conn:   conn,
		server: server,
		subs:   make(map[int64]func(string, json.RawMessage)),
	}
	conn.SetNotificationHandler(a.dispatch)
	conn.SetRequestHandler(a.handleServerRequest)
	return a
}

// ── Escape hatches ───────────────────────────────────────────────────────────
//
// These are functions, not methods, so that embedding an *Adapter cannot leak
// them into an adapter's exported surface. See the package doc.

// Call sends a request and decodes the reply into a fresh T, wrapping any
// transport failure as "<server> <label>: <cause>".
//
// T is the decode target verbatim: for a slice T a null reply still yields a
// nil slice, exactly as a hand-written adapter method does.
func Call[T any](ctx context.Context, a *Adapter, label, method string, params any) (T, error) {
	var result T
	if err := a.conn.Call(ctx, method, params, &result); err != nil {
		var zero T
		return zero, a.wrap(label, err)
	}
	return result, nil
}

// CallPtr is Call for the methods that return a pointer to a decoded struct.
// On success the returned pointer is never nil; on failure it is nil and the
// error carries the adapter's label.
func CallPtr[T any](ctx context.Context, a *Adapter, label, method string, params any) (*T, error) {
	var result T
	if err := a.conn.Call(ctx, method, params, &result); err != nil {
		return nil, a.wrap(label, err)
	}
	return &result, nil
}

// CallRaw sends a request and returns the reply undecoded, for adapters whose
// server answers a union type that needs inspecting before it can be unmarshalled.
func CallRaw(ctx context.Context, a *Adapter, label, method string, params any) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := a.conn.Call(ctx, method, params, &raw); err != nil {
		return nil, a.wrap(label, err)
	}
	return raw, nil
}

// Notify sends a notification, wrapping any transport failure with label.
func Notify(ctx context.Context, a *Adapter, label, method string, params any) error {
	return a.wrap(label, a.conn.Notify(ctx, method, params))
}

// Wrap renders err as "<server> <label>: <cause>" using a's server name, for
// adapter-side failures that never reach the transport (reading a document off
// disk, say). It returns nil for a nil err.
func Wrap(a *Adapter, label string, err error) error { return a.wrap(label, err) }

func (a *Adapter) wrap(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", a.server, label, err)
}

// ── Lifecycle ────────────────────────────────────────────────────────────────

// Initialize sends the initialize request and stores the server capabilities.
func (a *Adapter) Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error) {
	result, err := CallPtr[protocol.InitializeResult](ctx, a, "initialize", protocol.MethodInitialize, params)
	if err != nil {
		return nil, err
	}
	caps := result.Capabilities
	a.capsMu.Lock()
	a.caps = &caps
	a.capsMu.Unlock()
	return result, nil
}

// Initialized sends the initialized notification.
func (a *Adapter) Initialized(ctx context.Context) error {
	return Notify(ctx, a, "initialized", protocol.MethodInitialized, struct{}{})
}

// Shutdown requests a clean shutdown.
//
// This one stays longhand rather than routing through Call: shutdown takes no
// params and expects no result, and a generic helper would hand the transport a
// non-nil result pointer to decode into.
func (a *Adapter) Shutdown(ctx context.Context) error {
	return a.wrap("shutdown", a.conn.Call(ctx, protocol.MethodShutdown, nil, nil))
}

// Exit sends the exit notification.
func (a *Adapter) Exit(ctx context.Context) error {
	return Notify(ctx, a, "exit", protocol.MethodExit, nil)
}

// ── Document lifecycle ───────────────────────────────────────────────────────

// DidOpen notifies the server that a document has been opened.
func (a *Adapter) DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error {
	return Notify(ctx, a, "didOpen", protocol.MethodDidOpen, params)
}

// DidChange notifies the server of document changes. plumb uses full-document
// sync for every adapter, so callers send the complete text in each event.
func (a *Adapter) DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error {
	return Notify(ctx, a, "didChange", protocol.MethodDidChange, params)
}

// DidClose notifies the server that a document has been closed.
func (a *Adapter) DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error {
	return Notify(ctx, a, "didClose", protocol.MethodDidClose, params)
}

// DidChangeWatchedFiles notifies the server that one or more files changed on
// disk. Events are filtered to those matching the globs the server registered
// via client/registerCapability; when nothing survives the filter this returns
// nil without touching the transport, so plumb stays quiet on unwatched writes.
func (a *Adapter) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	params.Changes = a.watcher.FilterEvents(params.Changes)
	if len(params.Changes) == 0 {
		return nil
	}
	return Notify(ctx, a, "didChangeWatchedFiles", protocol.MethodDidChangeWatchedFiles, params)
}

// ── Capabilities / subscriptions ─────────────────────────────────────────────

// Capabilities returns the negotiated server capabilities, or nil before Initialize.
// The returned pointer is shared with the adapter and with every other caller,
// so treat it as read-only. Initialize writes the cache once and nothing
// mutates it afterwards, which is what lets a caller read the pointee after
// this method has released the lock — an embedder that mutated it would
// introduce a data race no test here would catch.
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

// handleServerRequest responds to server-initiated requests. Every supported
// server uses client/registerCapability to register file-watcher patterns; we
// accept and record the globs so DidChangeWatchedFiles can filter events.
func (a *Adapter) handleServerRequest(_ context.Context, method string, params json.RawMessage) (any, error) {
	return lsp.HandleServerRequest(&a.watcher, method, params)
}
