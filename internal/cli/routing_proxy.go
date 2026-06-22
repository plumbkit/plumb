package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
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
	// wsRoot is the connection's workspace root and discovered the child language
	// roots found beneath it at attach (the monorepo case: core/build.zig +
	// app/Package.swift under one .plumb/ root). They drive WorkspaceSymbols
	// fan-out so a no-file symbol query spans every detected language, not just
	// the elected primary. Both guarded by mu; nil/empty for a single-language root.
	wsRoot     string
	discovered []discoveredRoot
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

// setDiscovered records the connection's workspace root and the child language
// roots discovered beneath it, for WorkspaceSymbols fan-out. Always called at
// attach/re-pin (with nil for a single-language root) so a deliberate switch
// clears any stale set. Guarded by mu.
func (r *routingProxy) setDiscovered(wsRoot string, ds []discoveredRoot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wsRoot = wsRoot
	r.discovered = ds
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

// primaryClient returns the primary workspace's adapter or an error. If the
// pinned primary entry is hibernated (the idle janitor / LRU eviction stopped
// its server — only Java hibernates today), it is woken via acquireLang,
// mirroring route()'s on-demand acquire — so a URI-less call (WorkspaceSymbols,
// the lifecycle methods) wakes the server instead of failing with the
// misleading "not yet ready". A genuinely cold first start (no primary pinned
// yet) still returns that error.
func (r *routingProxy) primaryClient(ctx context.Context) (lsp.Client, error) {
	r.mu.RLock()
	p := r.primary
	root := r.primaryRoot
	lang := r.primaryLang
	r.mu.RUnlock()
	if c := p.get(); c != nil {
		p.touch()
		return c, nil
	}
	if root == "" || lang == "" {
		return nil, fmt.Errorf("LSP server not yet ready")
	}
	e, err := r.pool.acquireLang(ctx, root, lang, false)
	if err != nil {
		return nil, fmt.Errorf("waking primary %s for %s: %w", lang, root, err)
	}
	if c := e.proxy.get(); c != nil {
		e.proxy.touch()
		return c, nil
	}
	return nil, fmt.Errorf("LSP server not yet ready")
}

// route returns the Client responsible for the workspace containing uri.
// Falls back to the primary if uri is empty or workspace resolution fails.
func (r *routingProxy) route(ctx context.Context, uri string) (lsp.Client, error) {
	if uri == "" {
		return r.primaryClient(ctx)
	}
	path := paths.URIToPath(uri)
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
		return r.primaryClient(ctx)
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
		return r.primaryClient(ctx)
	}

	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if root == primaryRoot && targetLang == primaryLang {
		if c := primary.get(); c != nil {
			primary.touch()
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
		e.proxy.touch()
		r.noteActivated(root, targetLang)
		return c, nil
	}
	return nil, fmt.Errorf("LSP server not yet ready for %s", root)
}

// noteActivated reports a secondary language server coming live for a file
// inside the connection's pinned workspace, so the session record can surface
// every active LSP (not just the primary). It fires for any language other than
// the primary whose file resolves to the primary root OR a directory beneath it
// — a secondary's own root marker (e.g. index.html for HTML) makes Detect carve
// out a sub-root (site/), so a strict root== check would miss it. It does NOT
// fire for a genuinely different project reached by cross-workspace routing.
// A no-op for the primary language and when no callback is wired.
func (r *routingProxy) noteActivated(root, language string) {
	r.mu.RLock()
	cb := r.onActivate
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	r.mu.RUnlock()
	if cb == nil || language == primaryLang || !withinRoot(root, primaryRoot) {
		return
	}
	cb(language)
}

// withinRoot reports whether path is root itself or a descendant directory of it.
func withinRoot(path, root string) bool {
	if root == "" {
		return false
	}
	return path == root || strings.HasPrefix(path, root+"/")
}

// ─── lsp.Client implementation ─────────────────────────────────────────

// Workspace-wide / lifecycle methods stick to the primary.
func (r *routingProxy) Initialize(ctx context.Context, params protocol.InitializeParams) (*protocol.InitializeResult, error) {
	c, err := r.primaryClient(ctx)
	if err != nil {
		return nil, err
	}
	return c.Initialize(ctx, params)
}

func (r *routingProxy) Initialized(ctx context.Context) error {
	c, err := r.primaryClient(ctx)
	if err != nil {
		return err
	}
	return c.Initialized(ctx)
}

func (r *routingProxy) Shutdown(ctx context.Context) error {
	c, err := r.primaryClient(ctx)
	if err != nil {
		return err
	}
	return c.Shutdown(ctx)
}

func (r *routingProxy) Exit(ctx context.Context) error {
	c, err := r.primaryClient(ctx)
	if err != nil {
		return err
	}
	return c.Exit(ctx)
}

func (r *routingProxy) WorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams) ([]protocol.SymbolInformation, error) {
	r.mu.RLock()
	discovered := r.discovered
	wsRoot := r.wsRoot
	r.mu.RUnlock()
	// Single-language root: keep the primary-only behaviour.
	if len(discovered) == 0 {
		c, err := r.primaryClient(ctx)
		if err != nil {
			return nil, err
		}
		return c.WorkspaceSymbols(ctx, params)
	}
	return r.fanOutWorkspaceSymbols(ctx, params, wsRoot, discovered)
}

// lsTarget is a (root, language) pair to query during workspace-symbol fan-out.
type lsTarget struct {
	root     string
	language string
}

// fanOutWorkspaceSymbols queries every language server in a monorepo root — the
// discovered child roots plus any already-attached entry under the workspace —
// and merges the results, deduplicating by symbol identity. Lazily-attached
// children are warmed on the first such query (then cached), so a no-file symbol
// search spans every detected language. A server that errors or is not yet ready
// is skipped; the merged result wins over a single failure, and only an
// all-error fan-out surfaces an error.
func (r *routingProxy) fanOutWorkspaceSymbols(ctx context.Context, params protocol.WorkspaceSymbolParams, wsRoot string, discovered []discoveredRoot) ([]protocol.SymbolInformation, error) {
	var (
		merged   []protocol.SymbolInformation
		seen     = map[string]bool{}
		firstErr error
		gotAny   bool
	)
	for _, t := range r.symbolTargets(wsRoot, discovered) {
		syms, ready, err := r.symbolsFrom(ctx, t, params)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if !ready {
			continue
		}
		gotAny = true
		for _, sym := range syms {
			k := symbolKey(sym)
			if seen[k] {
				continue
			}
			seen[k] = true
			merged = append(merged, sym)
		}
	}
	if !gotAny && firstErr != nil {
		return nil, firstErr
	}
	return merged, nil
}

// symbolTargets is the deduplicated (root, language) set to query for a
// workspace-wide symbol search: the discovered child roots first, then any other
// language server already attached under the workspace root (a lazily-routed
// secondary, or the elected primary itself).
func (r *routingProxy) symbolTargets(wsRoot string, discovered []discoveredRoot) []lsTarget {
	seen := map[lsTarget]bool{}
	var targets []lsTarget
	add := func(root, language string) {
		t := lsTarget{root: root, language: language}
		if !seen[t] {
			seen[t] = true
			targets = append(targets, t)
		}
	}
	for _, d := range discovered {
		add(d.root, d.language)
	}
	for _, e := range r.pool.entriesUnderRoot(wsRoot) {
		add(e.root, e.language)
	}
	return targets
}

// symbolsFrom acquires (without pinning) the server for one target and queries
// it. ready is false when the server is not yet warm (treat as no results, not
// an error); err is the query/acquire failure.
func (r *routingProxy) symbolsFrom(ctx context.Context, t lsTarget, params protocol.WorkspaceSymbolParams) (syms []protocol.SymbolInformation, ready bool, err error) {
	e, err := r.pool.acquireLang(ctx, t.root, t.language, false)
	if err != nil {
		return nil, false, err
	}
	c := e.proxy.get()
	if c == nil {
		return nil, false, nil
	}
	syms, err = c.WorkspaceSymbols(ctx, params)
	if err != nil {
		return nil, false, err
	}
	return syms, true, nil
}

// symbolKey identifies a symbol for fan-out deduplication: name, kind, and
// source location. Distinct servers cover disjoint subtrees so collisions are
// rare, but a file on a root boundary could surface twice.
func symbolKey(s protocol.SymbolInformation) string {
	loc := s.Location
	return fmt.Sprintf("%s\x00%d\x00%s\x00%d\x00%d", s.Name, s.Kind, loc.URI, loc.Range.Start.Line, loc.Range.Start.Character)
}

func (r *routingProxy) Capabilities() *protocol.ServerCapabilities {
	c, err := r.primaryClient(context.Background())
	if err != nil {
		return nil
	}
	return c.Capabilities()
}

func (r *routingProxy) Subscribe(handler func(string, json.RawMessage)) func() {
	c, err := r.primaryClient(context.Background())
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
		path := paths.URIToPath(ev.URI)
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

// ─── Pull diagnostics (textDocument/diagnostic) ────────────────────────────
//
// routingProxy is the per-connection LSP handle the diagnostics tool is
// constructed with. The tool type-asserts that handle to its pullDiagnoser
// interface (SupportsPullDiagnostics + Diagnostic) and, for an untracked file a
// pull-only server never pushed on, requests diagnostics directly. The proxy
// satisfies that interface structurally by delegating to the per-file adapter:
// before these methods existed the assertion failed at runtime and the pull
// path was dormant live. The path is purely additive — the tool only reaches it
// when the push cache is empty for an untracked URI and the routed adapter both
// implements pull and reports the server advertised it.

// pullCapableClient is the optional pull-diagnostics surface an underlying
// adapter (zls, typescript-language-server) may expose. Resolved structurally
// from the routed lsp.Client.
type pullCapableClient interface {
	SupportsPullDiagnostics() bool
	Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

// SupportsPullDiagnostics reports whether the connection's primary adapter
// supports the LSP 3.17 pull model. URI-less by nature (the diagnostics tool
// calls it before it has routed a specific file), so it consults the primary —
// the same fallback every URI-less routingProxy method uses. Nil/err-safe:
// returns false whenever the primary is not ready or does not implement pull.
func (r *routingProxy) SupportsPullDiagnostics() bool {
	c, err := r.primaryClient(context.Background())
	if err != nil {
		return false
	}
	pc, ok := c.(pullCapableClient)
	return ok && pc.SupportsPullDiagnostics()
}

// Diagnostic routes the pull request to the adapter owning params' URI and
// delegates. Returns a wrapped error when the routed adapter does not implement
// the pull model, so the diagnostics tool falls back to its push (open-and-wait)
// path rather than surfacing a hard failure.
func (r *routingProxy) Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	c, err := r.route(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	pc, ok := c.(pullCapableClient)
	if !ok {
		return nil, fmt.Errorf("pull diagnostics unsupported for %s", params.TextDocument.URI)
	}
	return pc.Diagnostic(ctx, params)
}

var _ lsp.Client = (*routingProxy)(nil)
