package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// routingProxy implements lsp.Client by dispatching each call to the gopls
// instance for the workspace containing the URI in the method's params.
//
// Methods without a natural URI argument (Initialize, Shutdown, WorkspaceSymbols,
// Subscribe, Capabilities) fall back to the connection's "primary" workspace —
// the first one resolved for the connection. This preserves the existing
// behaviour for workspace-wide queries while making URI-bearing tools
// multi-workspace aware: a single MCP connection can query and edit symbols
// in any number of projects without pre-declaring an "active" one.
//
// Pool acquisition is idempotent and fast (map lookup + mutex) for workspaces
// already started; new workspaces incur a one-time gopls startup cost.
//
// Concurrency: all methods are safe for concurrent use.
type routingProxy struct {
	pool *workspacePool

	mu          sync.RWMutex
	primaryRoot string
	primaryLang string
	primary     *clientProxy
	guard       func(string) error
	// onActivate, when set, is invoked the first time a secondary language
	// server under the primary root serves a request, so the session can list
	// every active LSP. Guarded by mu; nil-safe.
	onActivate func(language string)
}

func newRoutingProxy(pool *workspacePool) *routingProxy {
	return &routingProxy{
		pool:    pool,
		primary: &clientProxy{},
	}
}

func (r *routingProxy) setBoundaryGuard(guard func(string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard = guard
}

// setActivateHook wires the callback fired when a secondary language server
// first serves a request under the primary root. Pass nil to clear it (done on
// a workspace re-pin so a switched connection starts with a clean adapter set).
func (r *routingProxy) setActivateHook(fn func(language string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onActivate = fn
}

// setPrimary records the connection's primary workspace. Idempotent — only
// the first call wins so the fallback target stays stable across the
// connection's lifetime.
func (r *routingProxy) setPrimary(root, language string, p *clientProxy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primaryRoot == "" {
		r.primaryRoot = root
		r.primaryLang = language
		r.primary = p
	}
}

// resetPrimary unconditionally repoints the primary workspace. Unlike
// setPrimary (first-wins, kept stable for the connection's lifetime), this is
// used by a deliberate workspace re-pin — session_start called with an explicit
// workspace that differs from the current one — to switch the connection's LSP
// routing to a different project.
func (r *routingProxy) resetPrimary(root, language string, p *clientProxy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primaryRoot = root
	r.primaryLang = language
	r.primary = p
}

// primaryClient returns the primary workspace's adapter or an error.
func (r *routingProxy) primaryClient() (lsp.Client, error) {
	r.mu.RLock()
	p := r.primary
	r.mu.RUnlock()
	if c := p.get(); c != nil {
		return c, nil
	}
	return nil, fmt.Errorf("LSP server not yet ready")
}

// route returns the Client responsible for the workspace containing uri.
// Falls back to the primary if uri is empty or workspace resolution fails.
func (r *routingProxy) route(ctx context.Context, uri string) (lsp.Client, error) {
	if uri == "" {
		return r.primaryClient()
	}
	path := strings.TrimPrefix(uri, "file://")
	r.mu.RLock()
	guard := r.guard
	r.mu.RUnlock()
	if guard != nil {
		if err := guard(path); err != nil {
			return nil, err
		}
	}
	root, language, err := r.pool.Detect(filepath.Dir(path))
	if err != nil {
		return r.primaryClient()
	}

	// Pick the language by file extension first (so a .html file in a Go root
	// reaches the HTML server), falling back to the root's primary language for
	// files no enabled language owns (e.g. a .md next to .go still goes to gopls,
	// which simply ignores it). When neither yields a real language, there is no
	// server for this file — defer to the primary.
	targetLang := language
	if fileLang := r.pool.fileLanguage(path); fileLang != "" {
		targetLang = fileLang
	}
	if targetLang == "" || targetLang == LanguageNone {
		return r.primaryClient()
	}

	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if root == primaryRoot && targetLang == primaryLang {
		if c := primary.get(); c != nil {
			return c, nil
		}
	}
	// On-demand routing acquire: not a pinned primary workspace/language, so
	// pass pin=false. The entry is never torn down by the refcount path for a
	// never-pinned (root, language); it lives until daemon shutdown (pre-refcount
	// behaviour) — the same lifecycle as a cross-workspace on-demand entry.
	e, err := r.pool.acquireLang(ctx, root, targetLang, false)
	if err != nil {
		return nil, fmt.Errorf("acquiring %s for %s: %w", targetLang, root, err)
	}
	if c := e.proxy.get(); c != nil {
		r.noteActivated(root, targetLang)
		return c, nil
	}
	return nil, fmt.Errorf("LSP server not yet ready for %s", root)
}

// noteActivated reports a secondary language server coming live under the
// connection's primary root, so the session record can surface every active
// LSP (not just the primary). A no-op for the primary language itself and when
// no callback is wired. See routingProxy.onActivate.
func (r *routingProxy) noteActivated(root, language string) {
	r.mu.RLock()
	cb := r.onActivate
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	r.mu.RUnlock()
	if cb == nil || root != primaryRoot || language == primaryLang {
		return
	}
	cb(language)
}

// ─── lsp.Client implementation ─────────────────────────────────────────

// Workspace-wide / lifecycle methods stick to the primary.
func (r *routingProxy) Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error) {
	c, err := r.primaryClient()
	if err != nil {
		return nil, err
	}
	return c.Initialize(ctx, params)
}

func (r *routingProxy) Initialized(ctx context.Context) error {
	c, err := r.primaryClient()
	if err != nil {
		return err
	}
	return c.Initialized(ctx)
}

func (r *routingProxy) Shutdown(ctx context.Context) error {
	c, err := r.primaryClient()
	if err != nil {
		return err
	}
	return c.Shutdown(ctx)
}

func (r *routingProxy) Exit(ctx context.Context) error {
	c, err := r.primaryClient()
	if err != nil {
		return err
	}
	return c.Exit(ctx)
}

func (r *routingProxy) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	c, err := r.primaryClient()
	if err != nil {
		return nil, err
	}
	return c.WorkspaceSymbols(ctx, params)
}

func (r *routingProxy) Capabilities() *protocol.ServerCapabilities {
	c, err := r.primaryClient()
	if err != nil {
		return nil
	}
	return c.Capabilities()
}

func (r *routingProxy) Subscribe(handler func(string, json.RawMessage)) func() {
	c, err := r.primaryClient()
	if err != nil {
		return func() {}
	}
	return c.Subscribe(handler)
}

// URI-bearing document methods route by URI.
func (r *routingProxy) DidOpen(ctx context.Context, params protocol.DidOpenTextDocumentParams) error {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return err
	}
	return c.DidOpen(ctx, params)
}

func (r *routingProxy) DidChange(ctx context.Context, params protocol.DidChangeTextDocumentParams) error {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return err
	}
	return c.DidChange(ctx, params)
}

func (r *routingProxy) DidClose(ctx context.Context, params protocol.DidCloseTextDocumentParams) error {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return err
	}
	return c.DidClose(ctx, params)
}

// DidChangeWatchedFiles groups events by routed workspace so each gopls instance
// only sees the events for files inside the workspace it manages.
func (r *routingProxy) DidChangeWatchedFiles(ctx context.Context, params protocol.DidChangeWatchedFilesParams) error {
	if len(params.Changes) == 0 {
		return nil
	}
	groups := make(map[lsp.Client][]protocol.FileEvent, 1)
	for _, ev := range params.Changes {
		path := strings.TrimPrefix(ev.URI, "file://")
		_, language, err := r.pool.Detect(filepath.Dir(path))
		if err == nil && language == LanguageNone {
			continue
		}
		c, err := r.route(ctx, ev.URI)
		if err != nil {
			return err
		}
		groups[c] = append(groups[c], ev)
	}
	var firstErr error
	for c, evs := range groups {
		if err := c.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{Changes: evs}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *routingProxy) DocumentSymbols(ctx context.Context, params protocol.DocumentSymbolParams) ([]protocol.DocumentSymbol, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.DocumentSymbols(ctx, params)
}

func (r *routingProxy) Definition(ctx context.Context, params protocol.DefinitionParams) ([]protocol.Location, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.Definition(ctx, params)
}

func (r *routingProxy) References(ctx context.Context, params protocol.ReferenceParams) ([]protocol.Location, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.References(ctx, params)
}

func (r *routingProxy) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.Hover(ctx, params)
}

func (r *routingProxy) PrepareRename(ctx context.Context, params protocol.PrepareRenameParams) (*protocol.PrepareRenameResult, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.PrepareRename(ctx, params)
}

func (r *routingProxy) Rename(ctx context.Context, params protocol.RenameParams) (*protocol.WorkspaceEdit, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.Rename(ctx, params)
}

func (r *routingProxy) PrepareCallHierarchy(ctx context.Context, params protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.PrepareCallHierarchy(ctx, params)
}

func (r *routingProxy) IncomingCalls(ctx context.Context, params protocol.CallHierarchyIncomingCallsParams) ([]protocol.CallHierarchyIncomingCall, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.IncomingCalls(ctx, params)
}

func (r *routingProxy) OutgoingCalls(ctx context.Context, params protocol.CallHierarchyOutgoingCallsParams) ([]protocol.CallHierarchyOutgoingCall, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.OutgoingCalls(ctx, params)
}

func (r *routingProxy) PrepareTypeHierarchy(ctx context.Context, params protocol.PrepareTypeHierarchyParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	return c.PrepareTypeHierarchy(ctx, params)
}

func (r *routingProxy) Supertypes(ctx context.Context, params protocol.TypeHierarchySupertypesParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.Supertypes(ctx, params)
}

func (r *routingProxy) Subtypes(ctx context.Context, params protocol.TypeHierarchySubtypesParams) ([]protocol.TypeHierarchyItem, error) {
	c, err := r.route(ctx, params.Item.URI)
	if err != nil {
		return nil, err
	}
	return c.Subtypes(ctx, params)
}

var _ lsp.Client = (*routingProxy)(nil)

// ─── routingInvProxy ─────────────────────────────────────────────────────

// routingInvProxy is a diagnosticsSource that dispatches Diagnostics(uri)
// to the invalidator of whichever workspace contains the URI. AllDiagnostics()
// returns the primary workspace's aggregate, since merging across unrelated
// projects would obscure provenance.
//
// Routing only inspects workspaces already acquired (pool.lookup). New
// workspaces are spun up by the routingProxy when a tool call lands on them;
// diagnostics for a never-touched workspace return empty rather than blocking
// to start gopls.
type routingInvProxy struct {
	pool *workspacePool

	mu          sync.RWMutex
	primaryRoot string
	primaryLang string
	primary     *cache.Invalidator
	guard       func(string) error
}

func newRoutingInvProxy(pool *workspacePool) *routingInvProxy {
	return &routingInvProxy{pool: pool}
}

// setBoundaryGuard wires the per-connection workspace boundary guard. Mirrors
// routingProxy.setBoundaryGuard so cross-workspace diagnostics queries cannot
// reach another acquired adapter through the routing fallback path. Defence in
// depth: the diagnostics tool already enforces the boundary at its entry.
func (r *routingInvProxy) setBoundaryGuard(guard func(string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard = guard
}

// checkURI applies the boundary guard to uri's path. Empty uri is allowed
// (callers treat "" as the workspace-aggregate request). Returns nil when no
// guard is set or when uri is in-bounds.
func (r *routingInvProxy) checkURI(uri string) error {
	if uri == "" {
		return nil
	}
	r.mu.RLock()
	guard := r.guard
	r.mu.RUnlock()
	if guard == nil {
		return nil
	}
	return guard(strings.TrimPrefix(uri, "file://"))
}

// timedDiagnosticsContract mirrors internal/tools' timedDiagnosticsSource
// shape (kept private here to avoid a cross-package import that would invert
// the existing layering). The compile-time assertion below keeps the routing
// proxy aligned with the consumer interface: if any of these methods are
// renamed or removed, the build fails here rather than silently disabling the
// staleness annotation downstream (the consumer is a type-assertion fallback,
// so a missing method would otherwise just degrade to plain formatting).
type timedDiagnosticsContract interface {
	Diagnostics(uri string) []protocol.Diagnostic
	AllDiagnostics() map[string][]protocol.Diagnostic
	Tracked(uri string) bool
	AllDiagnosticTimes() map[string]time.Time
}

var _ timedDiagnosticsContract = (*routingInvProxy)(nil)

func (r *routingInvProxy) setPrimary(root, language string, inv *cache.Invalidator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.primaryRoot == "" {
		r.primaryRoot = root
		r.primaryLang = language
		r.primary = inv
	}
}

// resetPrimary unconditionally repoints the primary diagnostic invalidator,
// mirroring routingProxy.resetPrimary for a deliberate workspace re-pin.
func (r *routingInvProxy) resetPrimary(root, language string, inv *cache.Invalidator) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.primaryRoot = root
	r.primaryLang = language
	r.primary = inv
}

// uriUnderRoot reports whether uri (file:// form) refers to a path under root.
func uriUnderRoot(uri, root string) bool {
	path := strings.TrimPrefix(uri, "file://")
	return path == root || strings.HasPrefix(path, root+"/")
}

// routeLang resolves the language whose invalidator owns path: the file's own
// language by extension, falling back to the root's primary (detectLang) for
// files no enabled language owns. Mirrors routingProxy.route's resolution so
// diagnostics land on the same server that produced them.
func (r *routingInvProxy) routeLang(path, detectLang string) string {
	if fl := r.pool.fileLanguage(path); fl != "" {
		return fl
	}
	return detectLang
}

func (r *routingInvProxy) Tracked(uri string) bool {
	if err := r.checkURI(uri); err != nil {
		return false
	}
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if uri == "" || primary == nil {
		return false
	}
	path := strings.TrimPrefix(uri, "file://")
	root, language, err := r.pool.Detect(filepath.Dir(path))
	targetLang := r.routeLang(path, language)
	if err != nil || (root == primaryRoot && targetLang == primaryLang) {
		return primary.Tracked(uri)
	}
	if e := r.pool.lookup(root, targetLang); e != nil {
		return e.inv.Tracked(uri)
	}
	return false
}

func (r *routingInvProxy) Diagnostics(uri string) []protocol.Diagnostic {
	if err := r.checkURI(uri); err != nil {
		return nil
	}
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if uri == "" {
		if primary == nil {
			return nil
		}
		return primary.Diagnostics(uri)
	}
	path := strings.TrimPrefix(uri, "file://")
	root, language, err := r.pool.Detect(filepath.Dir(path))
	targetLang := r.routeLang(path, language)
	if err != nil || (root == primaryRoot && targetLang == primaryLang) {
		if primary == nil {
			return nil
		}
		return primary.Diagnostics(uri)
	}
	if e := r.pool.lookup(root, targetLang); e != nil {
		return e.inv.Diagnostics(uri)
	}
	return nil
}

func (r *routingInvProxy) AllDiagnostics() map[string][]protocol.Diagnostic {
	r.mu.RLock()
	p := r.primary
	root := r.primaryRoot
	r.mu.RUnlock()
	if p == nil {
		return nil
	}
	// Fold the primary first, then any other language servers under the same
	// root (e.g. HTML alongside Go), so the aggregate covers every server a
	// multi-language workspace is driving. AllDiagnostics returns a fresh map,
	// so mutating merged is safe.
	merged := p.AllDiagnostics()
	for _, e := range r.pool.entriesForRoot(root) {
		if e.inv == p {
			continue
		}
		for uri, diags := range e.inv.AllDiagnostics() {
			merged[uri] = diags
		}
	}
	if root == "" {
		return merged
	}
	out := make(map[string][]protocol.Diagnostic, len(merged))
	for uri, diags := range merged {
		if uriUnderRoot(uri, root) {
			out[uri] = diags
		}
	}
	return out
}

// AllDiagnosticTimes returns the last-received diagnostic timestamp for each
// tracked URI under the primary workspace root.
func (r *routingInvProxy) AllDiagnosticTimes() map[string]time.Time {
	r.mu.RLock()
	p := r.primary
	root := r.primaryRoot
	r.mu.RUnlock()
	if p == nil {
		return nil
	}
	merged := p.AllDiagnosticTimes()
	for _, e := range r.pool.entriesForRoot(root) {
		if e.inv == p {
			continue
		}
		for uri, t := range e.inv.AllDiagnosticTimes() {
			merged[uri] = t
		}
	}
	if root == "" {
		return merged
	}
	out := make(map[string]time.Time, len(merged))
	for uri, t := range merged {
		if uriUnderRoot(uri, root) {
			out[uri] = t
		}
	}
	return out
}

func (r *routingInvProxy) WaitDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	if err := r.checkURI(uri); err != nil {
		return nil, err
	}
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if primary == nil {
		return nil, nil
	}
	path := strings.TrimPrefix(uri, "file://")
	root, language, err := r.pool.Detect(filepath.Dir(path))
	targetLang := r.routeLang(path, language)
	if err != nil || (root == primaryRoot && targetLang == primaryLang) {
		return primary.WaitDiagnostics(ctx, uri)
	}
	if e := r.pool.lookup(root, targetLang); e != nil {
		return e.inv.WaitDiagnostics(ctx, uri)
	}
	return nil, nil
}

func (r *routingInvProxy) WaitNextDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	if err := r.checkURI(uri); err != nil {
		return nil, err
	}
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if primary == nil {
		return nil, nil
	}
	path := strings.TrimPrefix(uri, "file://")
	root, language, err := r.pool.Detect(filepath.Dir(path))
	targetLang := r.routeLang(path, language)
	if err != nil || (root == primaryRoot && targetLang == primaryLang) {
		return primary.WaitNextDiagnostics(ctx, uri)
	}
	if e := r.pool.lookup(root, targetLang); e != nil {
		return e.inv.WaitNextDiagnostics(ctx, uri)
	}
	return nil, nil
}
