package cli

import (
	"log/slog"
	"sync"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/langsupport"
	"github.com/golimpio/plumb/internal/topology"
	"github.com/golimpio/plumb/internal/topology/extractors/golang"
	"github.com/golimpio/plumb/internal/topology/extractors/treesitter"
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

// extractorCtors maps a language name to its structural-extractor constructor.
// A language is indexed only when its langsupport entry has a non-EngineNone
// structural engine AND a constructor here. This is the seam for moving a
// language onto a different engine (regex → tree-sitter): change the
// langsupport entry and point the constructor here at the new extractor.
var extractorCtors = map[string]func() topology.Extractor{
	"go":         func() topology.Extractor { return golang.New() },
	"python":     func() topology.Extractor { return treesitter.NewPython() },
	"typescript": func() topology.Extractor { return typescript.New() },
	"rust":       func() topology.Extractor { return treesitter.NewRust() },
	"zig":        func() topology.Extractor { return treesitter.NewZig() },
	"kotlin":     func() topology.Extractor { return treesitter.NewKotlin() },
	"swift":      func() topology.Extractor { return treesitter.NewSwift() },
	"java":       func() topology.Extractor { return treesitter.NewJava() },
}

// buildExtractors instantiates the structural extractors for every language the
// langsupport registry marks indexable, in registry order.
func buildExtractors() []topology.Extractor {
	out := make([]topology.Extractor, 0, len(extractorCtors))
	for _, l := range langsupport.All() {
		if l.Structural == langsupport.EngineNone {
			continue
		}
		if ctor, ok := extractorCtors[l.Name]; ok {
			out = append(out, ctor())
		}
	}
	return out
}
