package cache

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// Invalidator subscribes to LSP server notifications, evicts cache entries
// when a document changes, and stores the latest diagnostics per document.
//
// Concurrency: Handle and all accessor methods are safe for concurrent use.
type Invalidator struct {
	cache     *Cache
	diagsMu   sync.RWMutex
	diags     map[string][]protocol.Diagnostic // keyed by document URI
	diagTimes map[string]time.Time             // last publishDiagnostics time per URI
	subs      map[string][]chan struct{}       // WaitDiagnostics subscribers
}

// NewInvalidator creates an Invalidator backed by c.
// Register its Handle method via adapter.Subscribe to receive notifications.
func NewInvalidator(c *Cache) *Invalidator {
	return &Invalidator{
		cache:     c,
		diags:     make(map[string][]protocol.Diagnostic),
		diagTimes: make(map[string]time.Time),
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

// Diagnostics returns a copy of the latest diagnostics for the given URI.
// Returns nil if no diagnostics have been received for that URI yet.
func (inv *Invalidator) Diagnostics(uri string) []protocol.Diagnostic {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	d := inv.diags[uri]
	if d == nil {
		return nil
	}
	out := make([]protocol.Diagnostic, len(d))
	copy(out, d)
	return out
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

// Tracked reports whether the language server has ever published diagnostics
// for uri. It is cheaper than AllDiagnostics()[uri] for an existence check.
func (inv *Invalidator) Tracked(uri string) bool {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	_, ok := inv.diags[uri]
	return ok
}

// AllDiagnostics returns a copy of every URI → diagnostics entry received so far.
func (inv *Invalidator) AllDiagnostics() map[string][]protocol.Diagnostic {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	out := make(map[string][]protocol.Diagnostic, len(inv.diags))
	for uri, d := range inv.diags {
		cp := make([]protocol.Diagnostic, len(d))
		copy(cp, d)
		out[uri] = cp
	}
	return out
}

// AllDiagnosticTimes returns a copy of the last-received timestamp for each
// tracked URI. Use alongside AllDiagnostics to detect entries that may be
// stale relative to the file's current mtime.
func (inv *Invalidator) AllDiagnosticTimes() map[string]time.Time {
	inv.diagsMu.RLock()
	defer inv.diagsMu.RUnlock()
	out := make(map[string]time.Time, len(inv.diagTimes))
	for k, v := range inv.diagTimes {
		out[k] = v
	}
	return out
}
