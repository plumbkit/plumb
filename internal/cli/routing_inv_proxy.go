package cli

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
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
