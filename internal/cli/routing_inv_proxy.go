package cli

import (
	"context"
	"path/filepath"
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
	return guard(paths.URIToPath(uri))
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
	path := paths.URIToPath(uri)
	root, language, err := r.pool.Detect(filepath.Dir(path))
	targetLang := r.routeLang(path, language)
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
	for _, e := range r.pool.entriesUnderRoot(root) {
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
	inv := r.resolveInv(uri)
	if inv == nil {
		return nil, nil
	}
	return inv.WaitDiagnostics(ctx, uri)
}

func (r *routingInvProxy) WaitNextDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	if err := r.checkURI(uri); err != nil {
		return nil, err
	}
	inv := r.resolveInv(uri)
	if inv == nil {
		return nil, nil
	}
	return inv.WaitNextDiagnostics(ctx, uri)
}
