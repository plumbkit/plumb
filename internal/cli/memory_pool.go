package cli

import (
	"log/slog"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/memory"
)

// memoryIndexPool manages one memory.Index per workspace root, shared across all
// connections to that workspace (the index is a single-writer SQLite handle, so
// sharing avoids redundant handles and reindexes). Indexes are opened lazily on
// first Acquire and closed once no connection holds the workspace and it has
// gone storeIdleGrace unused (store_drain.go, PLAN-535), or at daemon shutdown.
//
// Concurrency: all methods are safe for concurrent use.
type memoryIndexPool struct {
	mu      sync.Mutex
	indexes map[string]*memory.Index
	// flight lets an index be opened without p.mu (PLAN-536); closed marks the
	// pool shut, so an open that finishes afterwards closes its index.
	flight keyedFlight
	closed bool
	// used stamps each workspace's last Acquire, so the store drain
	// (store_drain.go, PLAN-535) can close an idle, unattached workspace's index.
	used map[string]time.Time
	// openIndex opens an index; nil means memory.OpenIndex. A test seam.
	openIndex func(workspace string) (*memory.Index, error)
}

func newMemoryIndexPool() *memoryIndexPool {
	return &memoryIndexPool{indexes: make(map[string]*memory.Index)}
}

// Acquire returns the memory index for workspace, opening it (and kicking a
// one-shot background reindex from the markdown files) on first use. Returns nil
// when workspace is empty or the index cannot be opened.
func (p *memoryIndexPool) Acquire(workspace string) *memory.Index {
	if workspace == "" {
		return nil
	}
	for {
		p.mu.Lock()
		if ix, ok := p.indexes[workspace]; ok {
			p.used = stampUsedLocked(p.used, workspace)
			p.mu.Unlock()
			return ix
		}
		wait, leader := p.flight.begin(workspace)
		p.mu.Unlock()
		if !leader {
			<-wait
			continue
		}
		return p.openAndPublish(workspace)
	}
}

// openAndPublish opens workspace's index without p.mu, publishes it and ends the
// flight, and starts the one-shot background reindex. Caller holds the flight.
func (p *memoryIndexPool) openAndPublish(workspace string) *memory.Index {
	open := p.openIndex
	if open == nil {
		open = memory.OpenIndex
	}
	ended := false
	defer func() { // a panicking open must not wedge every later Get for workspace
		if !ended {
			p.mu.Lock()
			p.flight.end(workspace)
			p.mu.Unlock()
		}
	}()
	ix, err := open(workspace)
	p.mu.Lock()
	p.flight.end(workspace)
	ended = true
	closed := p.closed
	if err == nil && !closed {
		p.indexes[workspace] = ix
		p.used = stampUsedLocked(p.used, workspace)
	}
	p.mu.Unlock()
	if err != nil {
		slog.Warn("memory: open index", "workspace", workspace, "err", err)
		return nil
	}
	if closed {
		_ = ix.Close()
		return nil
	}
	ix.ReindexBackground(workspace) // joined by ix.Close (PLAN-535)
	return ix
}

// CloseAll closes every open index. Called by the daemon on shutdown. The
// indexes are closed outside p.mu: Close joins the background reindex.
func (p *memoryIndexPool) CloseAll() {
	p.mu.Lock()
	p.closed = true
	indexes := p.indexes
	p.indexes = make(map[string]*memory.Index)
	p.used = nil
	p.mu.Unlock()
	for _, ix := range indexes {
		_ = ix.Close()
	}
}

// reapIdle closes every workspace's index that no connection holds and that
// has gone storeIdleGrace without an Acquire (PLAN-535; see drainIdle). The
// close cancels and joins the background reindex. Returns how many it closed.
func (p *memoryIndexPool) reapIdle(now time.Time, held func(root string) bool) int {
	return drainIdle(drainSet[*memory.Index]{
		mu: &p.mu, stores: &p.indexes, used: &p.used, flight: &p.flight,
		close: func(_ string, ix *memory.Index) { _ = ix.Close() },
	}, now, held)
}
