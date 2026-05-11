package cache

import (
	"encoding/json"
	"sync"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// Invalidator subscribes to LSP server notifications, evicts cache entries
// when a document changes, and stores the latest diagnostics per document.
//
// Concurrency: Handle and all accessor methods are safe for concurrent use.
type Invalidator struct {
	cache   *Cache
	diagsMu sync.RWMutex
	diags   map[string][]protocol.Diagnostic // keyed by document URI
}

// NewInvalidator creates an Invalidator backed by c.
// Register its Handle method via adapter.Subscribe to receive notifications.
func NewInvalidator(c *Cache) *Invalidator {
	return &Invalidator{
		cache: c,
		diags: make(map[string][]protocol.Diagnostic),
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
