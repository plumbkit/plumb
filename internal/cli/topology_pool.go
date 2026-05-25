package cli

import (
	"log/slog"
	"reflect"
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

// Reconcile applies a new global topology config to the pool when the global
// config reloads. It is a no-op when the topology config is unchanged (so it
// stays cheap across unrelated config edits). When topology is disabled it
// closes every open store; when it stays enabled but the tuning changed it
// re-opens each open store so the new settings (resync interval, excludes, size
// caps) take effect. Stores opened afterwards use the new config.
//
// This is safe to do live because the indexer is a background subsystem with no
// synchronous request path — no in-flight tool call fails when a store is
// closed and re-opened.
func (p *topologyPool) Reconcile(cfg config.TopologyConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if reflect.DeepEqual(cfg, p.cfg) {
		return
	}
	prevEnabled := p.cfg.Enabled
	p.cfg = cfg

	if !cfg.Enabled {
		for root, s := range p.stores {
			if err := s.Close(); err != nil {
				slog.Warn("topology: close on disable", "root", root, "err", err)
			}
			delete(p.stores, root)
		}
		slog.Info("topology: disabled via config reload; stores closed")
		return
	}

	if !prevEnabled {
		// Was disabled, now enabled: nothing open yet; new Acquires use the new
		// config. (Already-attached sessions pick it up on their next attach.)
		slog.Info("topology: enabled via config reload")
		return
	}

	// Still enabled, tuning changed: re-open each store with the new config.
	for root, s := range p.stores {
		if err := s.Close(); err != nil {
			slog.Warn("topology: close on reconfigure", "root", root, "err", err)
		}
		ns, err := topology.Open(root, cfg, buildExtractors())
		if err != nil {
			slog.Error("topology: reopen on reconcile failed", "root", root, "err", err)
			delete(p.stores, root)
			continue
		}
		p.stores[root] = ns
	}
	slog.Info("topology: reconfigured via config reload", "roots", len(p.stores))
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
	"typescript": func() topology.Extractor { return treesitter.NewTypeScript() },
	"tsx":        func() topology.Extractor { return typescript.New() },
	"javascript": func() topology.Extractor { return treesitter.NewJavaScript() },
	"rust":       func() topology.Extractor { return treesitter.NewRust() },
	"zig":        func() topology.Extractor { return treesitter.NewZig() },
	"kotlin":     func() topology.Extractor { return treesitter.NewKotlin() },
	"swift":      func() topology.Extractor { return treesitter.NewSwift() },
	"java":       func() topology.Extractor { return treesitter.NewJava() },
	"bash":       func() topology.Extractor { return treesitter.NewBash() },
	"hcl":        func() topology.Extractor { return treesitter.NewHCL() },
	"sql":        func() topology.Extractor { return treesitter.NewSQL() },
	"dockerfile": func() topology.Extractor { return treesitter.NewDockerfile() },
	"toml":       func() topology.Extractor { return treesitter.NewTOML() },
	"yaml":       func() topology.Extractor { return treesitter.NewYAML() },
	"markdown":   func() topology.Extractor { return treesitter.NewMarkdown() },
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
