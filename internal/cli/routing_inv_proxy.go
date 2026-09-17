package cli

import (
	"context"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

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
	// guard is the ctx-aware workspace boundary guard. The ctx matters: the
	// pull-RECORD path passes the calling agent's ctx, so its own policy decides
	// whether a report — including a server-supplied relatedDocuments key — may
	// enter a cache; the ctx-less cached-read surface passes context.Background,
	// which resolves to the pinned-policy union (see
	// connSession.invProxyBoundaryGuard).
	guard func(context.Context, string) error
	// workspaceFn resolves the CALLING logical agent's pinned workspace, or "".
	// The whole-workspace (URI-less) aggregate uses it to scope the result to the
	// calling agent's project instead of the connection's attach-time primary.
	workspaceFn func(context.Context) string
}

func newRoutingInvProxy(pool *workspacePool) *routingInvProxy {
	return &routingInvProxy{pool: pool}
}

// setBoundaryGuard wires the per-call workspace boundary guard. Mirrors
// routingProxy.setBoundaryGuard so cross-workspace diagnostics queries cannot
// reach another acquired adapter through the routing fallback path. Defence in
// depth: the diagnostics tool already enforces the boundary at its entry for
// every URI the CALLER names — which is why the guard must be ctx-aware here:
// the pull-record path also sees URIs the SERVER named (relatedDocuments keys,
// workspace-report items) and those must be judged against the calling agent's
// policy, not against every root any agent on this connection pinned.
func (r *routingInvProxy) setBoundaryGuard(guard func(context.Context, string) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard = guard
}

// setWorkspaceFn wires the per-call workspace accessor for the URI-less
// whole-workspace aggregate. Nil-safe.
func (r *routingInvProxy) setWorkspaceFn(fn func(context.Context) string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workspaceFn = fn
}

// checkURI applies the boundary guard to uri's path with NO attributable
// caller. The ctx-less cached-read surface (Tracked, Diagnostics) is the only
// caller: context.Background carries no logical agent, so the guard resolves it
// to the connection's pinned-policy union rather than any one agent's policy.
// Empty uri is allowed (callers treat "" as the workspace-aggregate request).
// Returns nil when no guard is set or when uri is in-bounds.
func (r *routingInvProxy) checkURI(uri string) error {
	return r.checkURIFor(context.Background(), uri)
}

// checkURIFor is checkURI under the caller's ctx. Every path that has one uses
// this form: the guard then resolves the CALLING agent's policy, so a URI the
// server supplied under a peer's root cannot be read from — or recorded into —
// that peer's cache.
func (r *routingInvProxy) checkURIFor(ctx context.Context, uri string) error {
	if uri == "" {
		return nil
	}
	r.mu.RLock()
	guard := r.guard
	r.mu.RUnlock()
	if guard == nil {
		return nil
	}
	return guard(ctx, paths.URIToPath(uri))
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
	path := paths.URIToPath(uri)
	return path == root || strings.HasPrefix(path, root+"/")
}

// resolveInv returns the invalidator that owns uri, or nil when no acquired
// workspace does.
//
// This is the single copy of the routing decision that Tracked, Diagnostics,
// WaitDiagnostics and WaitNextDiagnostics all need: resolve the file's language
// (extension first, then the detected root's primary), and if that (root,
// language) pair is not the connection's primary, look for an already-acquired
// pool entry for it. Each of those four methods used to carry its own copy, so a
// routing fix had to be made four times — and a fix applied to three of them
// would present as diagnostics that resolve correctly but never settle.
//
// An empty uri is the workspace-aggregate request (see checkURI) and always
// resolves to the primary, without consulting Detect.
func (r *routingInvProxy) resolveInv(uri string) *cache.Invalidator {
	r.mu.RLock()
	primaryRoot := r.primaryRoot
	primaryLang := r.primaryLang
	primary := r.primary
	r.mu.RUnlock()

	if uri == "" || primary == nil {
		return primary
	}
	// One policy resolution for both halves — see resolveFileTarget.
	root, targetLang, err := r.pool.resolveFileTarget(paths.URIToPath(uri))
	if err != nil || (root == primaryRoot && targetLang == primaryLang) {
		return primary
	}
	if e := r.pool.lookup(root, targetLang); e != nil {
		return e.inv
	}
	return nil
}

func (r *routingInvProxy) Tracked(uri string) bool {
	if err := r.checkURI(uri); err != nil {
		return false
	}
	// Tracked asks about one file, so the aggregate form has no answer.
	if uri == "" {
		return false
	}
	inv := r.resolveInv(uri)
	if inv == nil {
		return false
	}
	return inv.Tracked(uri)
}

func (r *routingInvProxy) Diagnostics(uri string) []protocol.Diagnostic {
	if err := r.checkURI(uri); err != nil {
		return nil
	}
	inv := r.resolveInv(uri)
	if inv == nil {
		return nil
	}
	return inv.Diagnostics(uri)
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
	for _, e := range r.pool.entriesUnderRoot(root) {
		if e.inv == p {
			continue
		}
		maps.Copy(merged, e.inv.AllDiagnostics())
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

// WaitForAnyDiagnostics blocks until ANY language server under this session
// republishes diagnostics for any file, or ctx expires.
//
// The post-write cross-file sweep uses this to end its settle grace early
// instead of sleeping it out. Without this passthrough the optional-interface
// assertion in waitForCrossFileSettle fails for every real session — WriteDeps.Diag
// is always this proxy, never a bare *cache.Invalidator — so the fix silently
// degraded to the fixed sleep it was written to remove, with the unit tests
// passing because they construct an Invalidator directly.
//
// Fans out over the same set AllDiagnostics folds (the primary plus any sibling
// server under the same root), because a cross-file break in a multi-language
// workspace can be published by a server other than the primary. The first wake
// wins; cancelling the derived context unsubscribes the rest.
func (r *routingInvProxy) WaitForAnyDiagnostics(ctx context.Context) error {
	r.mu.RLock()
	p := r.primary
	root := r.primaryRoot
	r.mu.RUnlock()
	if p == nil {
		<-ctx.Done() // nothing to wait on; honour the caller's ceiling
		return ctx.Err()
	}

	invs := []*cache.Invalidator{p}
	for _, e := range r.pool.entriesUnderRoot(root) {
		if e.inv != nil && e.inv != p {
			invs = append(invs, e.inv)
		}
	}
	if len(invs) == 1 {
		return p.WaitForAnyDiagnostics(ctx)
	}

	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(invs)) // buffered: no waiter goroutine can leak
	for _, inv := range invs {
		go func(i *cache.Invalidator) { done <- i.WaitForAnyDiagnostics(waitCtx) }(inv)
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
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
	for _, e := range r.pool.entriesUnderRoot(root) {
		if e.inv == p {
			continue
		}
		maps.Copy(merged, e.inv.AllDiagnosticTimes())
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

// AllDiagnosticsFor is the whole-workspace aggregate scoped to the CALLING
// agent's project. A URI-less diagnostics query used to answer from the
// connection's attach-time primary, so an agent pinned elsewhere was shown
// another project's (usually empty) report rather than its own.
func (r *routingInvProxy) AllDiagnosticsFor(ctx context.Context) map[string][]protocol.Diagnostic {
	root := r.agentWorkspace(ctx)
	if root == "" || root == r.connectionRoot() {
		return r.AllDiagnostics()
	}
	return aggregateDiagnosticsUnder(r.pool.entriesUnderRoot(root), root)
}

// AllDiagnosticTimesFor is the timestamp half of AllDiagnosticsFor.
func (r *routingInvProxy) AllDiagnosticTimesFor(ctx context.Context) map[string]time.Time {
	root := r.agentWorkspace(ctx)
	if root == "" || root == r.connectionRoot() {
		return r.AllDiagnosticTimes()
	}
	return aggregateTimesUnder(r.pool.entriesUnderRoot(root), root)
}

func (r *routingInvProxy) connectionRoot() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.primaryRoot
}

func (r *routingInvProxy) agentWorkspace(ctx context.Context) string {
	r.mu.RLock()
	fn := r.workspaceFn
	r.mu.RUnlock()
	if fn == nil {
		return ""
	}
	return fn(ctx)
}

// aggregateDiagnosticsUnder merges the diagnostics of every server attached
// under root and keeps only URIs inside it.
func aggregateDiagnosticsUnder(entries []*poolEntry, root string) map[string][]protocol.Diagnostic {
	merged := make(map[string][]protocol.Diagnostic)
	for _, e := range entries {
		if e == nil || e.inv == nil {
			continue
		}
		maps.Copy(merged, e.inv.AllDiagnostics())
	}
	out := make(map[string][]protocol.Diagnostic, len(merged))
	for uri, diags := range merged {
		if uriUnderRoot(uri, root) {
			out[uri] = diags
		}
	}
	return out
}

// aggregateTimesUnder is the timestamp half of aggregateDiagnosticsUnder.
func aggregateTimesUnder(entries []*poolEntry, root string) map[string]time.Time {
	merged := make(map[string]time.Time)
	for _, e := range entries {
		if e == nil || e.inv == nil {
			continue
		}
		maps.Copy(merged, e.inv.AllDiagnosticTimes())
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
	if err := r.checkURIFor(ctx, uri); err != nil {
		return nil, err
	}
	inv := r.resolveInv(uri)
	if inv == nil {
		return nil, nil
	}
	return inv.WaitDiagnostics(ctx, uri)
}

func (r *routingInvProxy) WaitNextDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	if err := r.checkURIFor(ctx, uri); err != nil {
		return nil, err
	}
	inv := r.resolveInv(uri)
	if inv == nil {
		return nil, nil
	}
	return inv.WaitNextDiagnostics(ctx, uri)
}
