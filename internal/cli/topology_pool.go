package cli

import (
	"log/slog"
	"sync"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/topology"
	"github.com/golimpio/plumb/internal/topology/extractors/golang"
	"github.com/golimpio/plumb/internal/topology/extractors/python"
	"github.com/golimpio/plumb/internal/topology/extractors/typescript"
)

// topologyPool manages one topology.Store per workspace root.
// The first call to Acquire for a given root creates and starts the store;
// subsequent calls return the existing instance. Stores run until StopAll.
//
// Concurrency: all methods are safe for concurrent use.
type topologyPool struct {
	mu     sync.Mutex
	stores map[string]*topology.Store
	cfg    config.TopologyConfig
}

func newTopologyPool(cfg config.TopologyConfig) *topologyPool {
	return &topologyPool{
		stores: make(map[string]*topology.Store),
		cfg:    cfg,
	}
}

// Acquire returns the Store for root, creating and starting it if necessary.
// Returns nil when topology is disabled or the store cannot be opened.
func (p *topologyPool) Acquire(root string) *topology.Store {
	if root == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.stores[root]; ok {
		return s
	}
	exts := buildExtractors()
	s, err := topology.Open(root, p.cfg, exts)
	if err != nil {
		slog.Error("topology: failed to open store", "root", root, "err", err)
		return nil
	}
	p.stores[root] = s
	slog.Info("topology: store opened", "root", root)
	return s
}

// StopAll stops all running indexers. Called by the daemon on shutdown.
func (p *topologyPool) StopAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for root, s := range p.stores {
		if err := s.Close(); err != nil {
			slog.Warn("topology: close store", "root", root, "err", err)
		}
		delete(p.stores, root)
	}
}

func buildExtractors() []topology.Extractor {
	return []topology.Extractor{
		golang.New(),
		python.New(),
		typescript.New(),
	}
}
