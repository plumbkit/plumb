package cli

import (
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/langsupport"
	"github.com/plumbkit/plumb/internal/paths"
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
	// flight lets a root's store be opened or re-opened without p.mu: opening
	// starts SQLite, a file watcher and the indexer (PLAN-536). closed marks the
	// pool stopped, so an open that finishes afterwards closes its store.
	flight keyedFlight
	closed bool
	// sweeps counts the disabling reloads that closed every store; a leader
	// whose open spans one does not publish its store (it would outlive the
	// sweep on a pool the reload just disabled).
	sweeps uint64
	// openStore opens a store; nil means topology.Open with every extractor. A
	// test seam for a slow or probing open.
	openStore func(root string, cfg config.TopologyConfig) (*topology.Store, error)
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
//
// The close and the open run WITHOUT p.mu (PLAN-536); a concurrent Acquire for
// the same root waits for them, one for another root does not.
func (p *topologyPool) Acquire(root string, cfg config.TopologyConfig) *topology.Store {
	if root == "" {
		return nil
	}
	for {
		p.mu.Lock()
		if s, ok := p.stores[root]; ok && reflect.DeepEqual(p.cfgs[root], cfg) {
			p.mu.Unlock()
			return s
		}
		wait, leader := p.flight.begin(root)
		if !leader {
			p.mu.Unlock()
			<-wait
			continue
		}
		// Absent, or opened with a different config — a first attach carrying
		// project tuning, or a post-reload re-acquire: (re-)open so the new
		// settings take effect.
		old := p.stores[root]
		delete(p.stores, root)
		delete(p.cfgs, root)
		sweeps := p.sweeps
		p.mu.Unlock()
		return p.reopen(root, old, cfg, sweeps)
	}
}

// reopen closes old (when any) and opens root with cfg, both without p.mu, then
// publishes the result and ends root's flight. Caller holds root's flight.
func (p *topologyPool) reopen(root string, old *topology.Store, cfg config.TopologyConfig, sweeps uint64) *topology.Store {
	ended := false
	defer p.endFlightUnlessEnded(root, &ended) // first: old.Close can panic too
	if old != nil {
		if err := old.Close(); err != nil {
			slog.Warn("topology: close on reconfigure", "root", root, "err", err)
		}
	}
	open := p.openStore
	if open == nil {
		open = func(root string, cfg config.TopologyConfig) (*topology.Store, error) {
			return topology.Open(root, cfg, buildExtractors())
		}
	}
	s, err := open(root, cfg)
	p.mu.Lock()
	p.flight.end(root)
	ended = true
	// A reload that disabled topology while this open ran has already swept the
	// pool; publishing now would leave a store the sweep meant to close.
	closed := p.closed || p.sweeps != sweeps
	if err == nil && !closed {
		p.stores[root] = s
		p.cfgs[root] = cfg
	}
	p.mu.Unlock()
	if err != nil {
		slog.Error("topology: failed to open store", "root", root, "err", err)
		return nil
	}
	if closed {
		_ = s.Close()
		return nil
	}
	slog.Info("topology: store opened", "root", root)
	return s
}

// endFlightUnlessEnded ends root's flight when the leader's open did not reach
// its own end — a panicking open — so the panic cannot wedge every later Acquire
// for root behind a flight nobody will end. Deferred by the leader.
func (p *topologyPool) endFlightUnlessEnded(root string, ended *bool) {
	if *ended {
		return
	}
	p.mu.Lock()
	p.flight.end(root)
	p.mu.Unlock()
}

// get returns the pool's store for root when one is already open, and nil
// otherwise. It never opens a store — a read-only caller (the web dashboard's
// health poll) must not materialise an index, with its database file and
// background indexer, for a workspace that has none. Same never-creates
// discipline as collabPool.getGlobal.
func (p *topologyPool) get(root string) *topology.Store {
	if root == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stores[root]
}

// forRoot returns the pool's already-open store for a canonical root, or nil when
// none is open. It is the root-keyed accessor the daemon hands to readers that
// answer FOR a root other than the one a connection holds (context_for_task's
// agents, the lifecycle hook hinter): the answer depends on root and on nothing
// else, and, like get, it never opens a store, so a read cannot materialise an
// index, with its database file and background indexer, for a workspace that has
// none. Keys are the spellings sessions attached with, so a canonical root also
// matches a key that canonicalises to it. Canonicalising touches the file system, so
// it runs on a copy of the keys taken under the lock and never while holding it, and
// goes in key order so two keys that canonicalise alike always answer the same.
func (p *topologyPool) forRoot(root string) *topology.Store {
	if root == "" {
		return nil
	}
	p.mu.Lock()
	if s, ok := p.stores[root]; ok {
		p.mu.Unlock()
		return s
	}
	open := maps.Clone(p.stores)
	p.mu.Unlock()
	for _, key := range slices.Sorted(maps.Keys(open)) {
		if paths.Canonical(key) == root {
			return open[key]
		}
	}
	return nil
}

// healthFor reports the LIVE indexer's health for root, and false when the pool
// holds no store for it — so a caller can tell "this indexer is healthy" from
// "there is no indexer here to ask", which is the web layer's failing flag being
// known rather than merely false. Reads through get, so it never opens a store.
func (p *topologyPool) healthFor(root string) (topology.Health, bool) {
	s := p.get(root)
	if s == nil {
		return topology.Health{}, false
	}
	return s.Health(), true
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
//
// The map is snapshotted under p.mu; every close and re-open runs without it
// (PLAN-536), one root at a time through that root's flight.
func (p *topologyPool) Reconcile(cfg config.TopologyConfig) {
	p.mu.Lock()
	if reflect.DeepEqual(cfg, p.cfg) {
		p.mu.Unlock()
		return
	}
	prevEnabled := p.cfg.Enabled
	p.cfg = cfg

	if !cfg.Enabled {
		p.sweeps++
		closing := make(map[string]*topology.Store, len(p.stores))
		for root, s := range p.stores {
			closing[root] = s
			delete(p.stores, root)
			delete(p.cfgs, root)
		}
		p.mu.Unlock()
		for root, s := range closing {
			if err := s.Close(); err != nil {
				slog.Warn("topology: close on disable", "root", root, "err", err)
			}
		}
		slog.Info("topology: disabled via config reload; stores closed")
		return
	}
	roots := slices.Collect(maps.Keys(p.stores))
	p.mu.Unlock()

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
	for _, root := range roots {
		p.Acquire(root, cfg)
	}
	slog.Info("topology: reconfigured via config reload", "roots", len(roots))
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
	p.closed = true
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
	"typescript": func() topology.Extractor { return treesitter.NewTypeScript() },
	"tsx":        func() topology.Extractor { return treesitter.NewTSX() },
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
	"ruby":       func() topology.Extractor { return treesitter.NewRuby() },
	"c":          func() topology.Extractor { return treesitter.NewC() },
	"json":       func() topology.Extractor { return treesitter.NewJSON() },
	"css":        func() topology.Extractor { return treesitter.NewCSS() },
	"scala":      func() topology.Extractor { return treesitter.NewScala() },
	"elixir":     func() topology.Extractor { return treesitter.NewElixir() },
	"csharp":     func() topology.Extractor { return treesitter.NewCSharp() },
	"php":        func() topology.Extractor { return treesitter.NewPHP() },
	"scss":       func() topology.Extractor { return treesitter.NewSCSS() },
	"xml":        func() topology.Extractor { return treesitter.NewXML() },
	"lua":        func() topology.Extractor { return treesitter.NewLua() },
	"cpp":        func() topology.Extractor { return treesitter.NewCpp() },
	"objc":       func() topology.Extractor { return treesitter.NewObjC() },
	"dart":       func() topology.Extractor { return treesitter.NewDart() },
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
