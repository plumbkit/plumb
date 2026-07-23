package cli

import (
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/golang"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
	"github.com/plumbkit/plumb/internal/topology/extractors/wasmts"
)

// topologyPool manages one topology.Store per workspace root.
// The first Acquire for a given root creates and starts the store with the
// per-project config the caller passes; subsequent calls return the existing
// instance, re-opening it when the caller's config changed (so per-project
// tuning takes effect on attach and after a config reload). Stores run until
// StopAll.
//
// cfg is the global topology config, used only by Reconcile on a global reload.
// cfgs records the effective config each open store was opened with, so Acquire
// can tell when a re-open is needed.
//
// Concurrency: all methods are safe for concurrent use.
type topologyPool struct {
	mu     sync.Mutex
	stores map[string]*topology.Store
	cfgs   map[string]config.TopologyConfig
	cfg    config.TopologyConfig
}

func newTopologyPool(cfg config.TopologyConfig) *topologyPool {
	return &topologyPool{
		stores: make(map[string]*topology.Store),
		cfgs:   make(map[string]config.TopologyConfig),
		cfg:    cfg,
	}
}

// Acquire returns the Store for root, creating and starting it with cfg when
// necessary. cfg is the merged per-project topology config for root; when an
// existing store was opened with a different config, it is closed and re-opened
// so the new tuning (resync interval, excludes, size caps) takes effect.
// Returns nil when root is empty or the store cannot be opened.
func (p *topologyPool) Acquire(root string, cfg config.TopologyConfig) *topology.Store {
	if root == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.stores[root]; ok {
		if reflect.DeepEqual(p.cfgs[root], cfg) {
			return s
		}
		// Config differs from what the store was opened with — a first attach
		// carrying project tuning, or a post-reload re-acquire. Re-open so the
		// new settings take effect.
		if err := s.Close(); err != nil {
			slog.Warn("topology: close on reconfigure", "root", root, "err", err)
		}
		delete(p.stores, root)
		delete(p.cfgs, root)
	}
	s, err := topology.Open(root, cfg, buildExtractors())
	if err != nil {
		slog.Error("topology: failed to open store", "root", root, "err", err)
		return nil
	}
	p.stores[root] = s
	p.cfgs[root] = cfg
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
			delete(p.cfgs, root)
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

	// Still enabled, tuning changed: re-open each store with the new global
	// config. A root with a per-project override is re-tuned afterwards by its
	// session's reconcileTopologyStore (which re-acquires with the merged
	// config); a root with no live session stays on the global config until its
	// next attach.
	for root, s := range p.stores {
		if err := s.Close(); err != nil {
			slog.Warn("topology: close on reconfigure", "root", root, "err", err)
		}
		ns, err := topology.Open(root, cfg, buildExtractors())
		if err != nil {
			slog.Error("topology: reopen on reconcile failed", "root", root, "err", err)
			delete(p.stores, root)
			delete(p.cfgs, root)
			continue
		}
		p.stores[root] = ns
		p.cfgs[root] = cfg
	}
	slog.Info("topology: reconfigured via config reload", "roots", len(p.stores))
}

// topoStopAllGrace bounds StopAll's aggregate wait for every topology store to
// close. Store.Close() -> Indexer.Stop() waits on the background worker to drain
// its current operation (a resync or a per-file extract), which is otherwise
// unbounded: a wedged indexer would make the shutdown watchdog the only way out.
// Stores are closed concurrently, so this one budget covers the whole pool. A
// healthy indexer stops in microseconds; this only ever expires on a wedge, and
// then the store is abandoned (WAL-safe) rather than stalling daemon exit. Kept
// well under the shutdown watchdog — see shutdownHardDeadline.
const topoStopAllGrace = 2 * time.Second

// StopAll stops all running indexers. Called by the daemon on shutdown. Stores
// are closed concurrently under a single bounded wait (topoStopAllGrace): a
// wedged indexer is abandoned — and logged — rather than blocking daemon exit
// until the watchdog forces it, since the process is exiting and the leaked
// worker goroutine is reclaimed by exit. The map is snapshotted and cleared
// under the lock, then Close runs outside it (Close blocks on the indexer drain,
// which must not be held under p.mu).
func (p *topologyPool) StopAll() {
	p.mu.Lock()
	stores := make([]*topology.Store, 0, len(p.stores))
	for root, s := range p.stores {
		stores = append(stores, s)
		delete(p.stores, root)
		delete(p.cfgs, root)
	}
	p.mu.Unlock()

	if len(stores) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err != nil {
				slog.Warn("topology: close store", "err", err)
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	waitWithTimeout(done, topoStopAllGrace, "topology pool")
}

// extractorCtors maps a language name to its structural-extractor constructor.
// A language is indexed only when its langsupport entry has a non-EngineNone
// structural engine AND a constructor here. This is the seam for moving a
// language onto a different engine (regex → tree-sitter): change the
// langsupport entry and point the constructor here at the new extractor.
var extractorCtors = map[string]func() topology.Extractor{
	"go":         func() topology.Extractor { return golang.New() },
	"python":     func() topology.Extractor { return treesitter.NewPython() },
	"typescript": func() topology.Extractor { return wasmts.NewTypeScript() },
	"tsx":        func() topology.Extractor { return wasmts.NewTSX() },
	"javascript": func() topology.Extractor { return treesitter.NewJavaScript() },
	"rust":       func() topology.Extractor { return treesitter.NewRust() },
	"zig":        func() topology.Extractor { return treesitter.NewZig() },
	"kotlin":     func() topology.Extractor { return treesitter.NewKotlin() },
	"swift":      func() topology.Extractor { return wasmts.NewSwift() },
	"java":       func() topology.Extractor { return treesitter.NewJava() },
	"bash":       func() topology.Extractor { return treesitter.NewBash() },
	"hcl":        func() topology.Extractor { return treesitter.NewHCL() },
	"sql":        func() topology.Extractor { return treesitter.NewSQL() },
	"dockerfile": func() topology.Extractor { return treesitter.NewDockerfile() },
	"toml":       func() topology.Extractor { return treesitter.NewTOML() },
	"yaml":       func() topology.Extractor { return treesitter.NewYAML() },
	"markdown":   func() topology.Extractor { return treesitter.NewMarkdown() },
	"html":       func() topology.Extractor { return treesitter.NewHTML() },
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
