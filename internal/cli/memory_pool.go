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
	p.mu.Lock()
	defer p.mu.Unlock()
	if ix, ok := p.indexes[workspace]; ok {
		return ix
	}
	ix, err := memory.OpenIndex(workspace)
	if err != nil {
		slog.Warn("memory: open index", "workspace", workspace, "err", err)
		return nil
	}
	p.indexes[workspace] = ix
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
	for _, ix := range p.indexes {
		_ = ix.Close()
	}
	p.indexes = make(map[string]*memory.Index)
}
