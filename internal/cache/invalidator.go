package cache

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Invalidator subscribes to LSP server notifications, evicts cache entries
// when a document changes, and stores the latest diagnostics per document.
//
// It holds two independent per-URI snapshots: the PUSH snapshot (diags,
// populated by textDocument/publishDiagnostics via Handle) and the PULL snapshot
// (pullDiags, populated by textDocument/diagnostic reports via RecordPull*, see
// invalidator_pull.go). The readers below expose the deduplicated union of the
// two, so a URI reported by either channel — or both — presents as one view.
//
// Concurrency: Handle, the RecordPull* methods, and all accessor methods are
// safe for concurrent use; every field below is guarded by diagsMu.
type Invalidator struct {
	cache     *Cache
	diagsMu   sync.RWMutex
	diags     map[string][]protocol.Diagnostic // push snapshot, keyed by document URI
	diagTimes map[string]time.Time             // last publishDiagnostics time per URI
	subs      map[string][]chan struct{}       // WaitDiagnostics subscribers

	// Pull-diagnostics state (see invalidator_pull.go), also guarded by diagsMu.
	pullDiags     map[string][]protocol.Diagnostic // pull snapshot per URI
	pullResultIDs map[string]string                // last non-empty result ID per URI
	pullTimes     map[string]time.Time             // last pull update time per URI
}

// NewInvalidator creates an Invalidator backed by c.
// Register its Handle method via adapter.Subscribe to receive notifications.
func NewInvalidator(c *Cache) *Invalidator {
	return &Invalidator{
		cache:         c,
		diags:         make(map[string][]protocol.Diagnostic),
		diagTimes:     make(map[string]time.Time),
		pullDiags:     make(map[string][]protocol.Diagnostic),
		pullResultIDs: make(map[string]string),
		pullTimes:     make(map[string]time.Time),
	}
}

// Handle processes one server-initiated notification. It evicts cache entries
// and stores diagnostics on textDocument/publishDiagnostics; it is a no-op
// for all other methods.
func (inv *Invalidator) Handle(method string, params json.RawMessage) {
	if method != protocol.MethodPublishDiagnostics {
		return
	}
	var p protocol.PublishDiagnosticsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	if p.URI == "" {
		return
	}
	inv.cache.InvalidateByPath(p.URI)

	inv.diagsMu.Lock()
	inv.diags[p.URI] = p.Diagnostics
	inv.diagTimes[p.URI] = time.Now()
	for _, ch := range inv.subs[p.URI] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	delete(inv.subs, p.URI)
	inv.diagsMu.Unlock()
}

// Diagnostics returns a copy of the latest diagnostics for the given URI: the
// deduplicated union of its push and pull snapshots. Returns nil if neither
// channel has reported on that URI yet.
func (inv *Invalidator) Diagnostics(uri string) []protocol.Diagnostic {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	return inv.mergedLocked(uri)
}

// WaitDiagnostics blocks until the language server publishes diagnostics for
// uri, then returns a copy. Returns immediately if uri is already tracked.
// Returns (nil, ctx.Err()) when the context is cancelled or times out.
//
// The subscriber channel is registered while diagsMu is held, so no
// publishDiagnostics notification can be missed between the "not tracked"
// check and the channel registration.
func (inv *Invalidator) WaitDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	ch := make(chan struct{}, 1)

	inv.diagsMu.Lock()
	if d, ok := inv.diags[uri]; ok {
		out := make([]protocol.Diagnostic, len(d))
		copy(out, d)
		inv.diagsMu.Unlock()
		return out, nil
	}
	if inv.subs == nil {
		inv.subs = make(map[string][]chan struct{})
	}
	inv.subs[uri] = append(inv.subs[uri], ch)
	inv.diagsMu.Unlock()

	defer func() {
		inv.diagsMu.Lock()
		chans := inv.subs[uri]
		for i, c := range chans {
			if c == ch {
				inv.subs[uri] = append(chans[:i], chans[i+1:]...)
				break
			}
		}
		if len(inv.subs[uri]) == 0 {
			delete(inv.subs, uri)
		}
		inv.diagsMu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ch:
		return inv.Diagnostics(uri), nil
	}
}

// WaitNextDiagnostics blocks until the language server publishes the next
// diagnostics notification for uri, then returns a copy. Unlike WaitDiagnostics
// it never returns immediately when the URI is already tracked — it always waits
// for the next publishDiagnostics push, making it suitable for post-write refresh
// where the cached (pre-write) snapshot must not be returned.
//
// On context cancellation or timeout the most-recent diagnostics for uri are
// returned alongside ctx.Err().
func (inv *Invalidator) WaitNextDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	ch := make(chan struct{}, 1)

	inv.diagsMu.Lock()
	if inv.subs == nil {
		inv.subs = make(map[string][]chan struct{})
	}
	inv.subs[uri] = append(inv.subs[uri], ch)
	inv.diagsMu.Unlock()

	defer func() {
		inv.diagsMu.Lock()
		chans := inv.subs[uri]
		for i, c := range chans {
			if c == ch {
				inv.subs[uri] = append(chans[:i], chans[i+1:]...)
				break
			}
		}
		if len(inv.subs[uri]) == 0 {
			delete(inv.subs, uri)
		}
		inv.diagsMu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return inv.Diagnostics(uri), ctx.Err()
	case <-ch:
		return inv.Diagnostics(uri), nil
	}
}

// Tracked reports whether the language server has ever reported diagnostics for
// uri via either channel (a pushed publishDiagnostics or a pull report,
// including an empty full report — a real "no issues" answer). It is cheaper
// than AllDiagnostics()[uri] for an existence check.
func (inv *Invalidator) Tracked(uri string) bool {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	if _, ok := inv.diags[uri]; ok {
		return true
	}
	_, ok := inv.pullDiags[uri]
	return ok
}

// AllDiagnostics returns a copy of every URI → diagnostics entry received so
// far, each as the deduplicated union of its push and pull snapshots.
func (inv *Invalidator) AllDiagnostics() map[string][]protocol.Diagnostic {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	out := make(map[string][]protocol.Diagnostic, len(inv.diags)+len(inv.pullDiags))
	for uri := range inv.diags {
		out[uri] = inv.mergedLocked(uri)
	}
	for uri := range inv.pullDiags {
		if _, done := out[uri]; !done {
			out[uri] = inv.mergedLocked(uri)
		}
	}
	return out
}

// AllDiagnosticTimes returns a copy of the last-received timestamp for each
// tracked URI — the more recent of its push and pull update times. Use
// alongside AllDiagnostics to detect entries that may be stale relative to the
// file's current mtime.
func (inv *Invalidator) AllDiagnosticTimes() map[string]time.Time {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	out := make(map[string]time.Time, len(inv.diagTimes)+len(inv.pullTimes))
	for k, v := range inv.diagTimes {
		out[k] = v
	}
	for k, v := range inv.pullTimes {
		if cur, ok := out[k]; !ok || v.After(cur) {
			out[k] = v
		}
	}
	return out
}
