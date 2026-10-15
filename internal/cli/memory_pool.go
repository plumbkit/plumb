package cli

import (
	"log/slog"
	"sync"

	"github.com/plumbkit/plumb/internal/memory"
)

// memoryIndexPool manages one memory.Index per workspace root, shared across all
// connections to that workspace (the index is a single-writer SQLite handle, so
// sharing avoids redundant handles and reindexes). Indexes are opened lazily on
// first Acquire and live until daemon shutdown.
//
// Concurrency: all methods are safe for concurrent use.
type memoryIndexPool struct {
	mu      sync.Mutex
	indexes map[string]*memory.Index
	// flight lets an index be opened without p.mu (PLAN-536); closed marks the
	// pool shut, so an open that finishes afterwards closes its index.
	flight keyedFlight
	closed bool
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
	go func() {
		if _, rerr := ix.Reindex(workspace); rerr != nil {
			slog.Debug("memory: initial reindex", "workspace", workspace, "err", rerr)
		}
	}()
	return ix
}

// CloseAll closes every open index. Called by the daemon on shutdown.
func (p *memoryIndexPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, ix := range p.indexes {
		_ = ix.Close()
	}
	p.indexes = make(map[string]*memory.Index)
}
