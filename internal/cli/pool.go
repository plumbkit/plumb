package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// The pool is split across files by concern: workspace detection + the CLI
// resolver live in pool_detect.go; per-language adapter construction and init
// params in pool_adapters.go; idle hibernation, LRU eviction, the janitor, live
// PID/RSS reporting, and jdtls-data pruning in pool_hibernate.go. This file holds
// the pool state and the acquire/release/teardown lifecycle.

// poolKey identifies a language-server entry by its workspace root AND the
// language it serves. Keying by (root, language) — rather than root alone — is
// what lets a single workspace bind more than one language server (e.g. Go +
// HTML in one web-app repo): each language gets its own supervisor, cache, and
// diagnostic stream under the same root.
type poolKey struct {
	root     string
	language string
}

// workspacePool keeps one language-server process alive per (root, language).
// Multiple MCP sessions targeting the same root share a single LS process per
// language, its cache, and its diagnostic stream.
//
// The pool supports multiple languages (Go via gopls, Python via pyright, …)
// and multiple languages within one root. Detect() resolves a path → (root,
// primary-language) tuple from configured root markers; acquireLang() starts
// the named adapter for a (root, language) pair.
//
// Concurrency: all methods are safe for concurrent use.
type workspacePool struct {
	mu       sync.Mutex
	entries  map[poolKey]*poolEntry // key: (root, language); one LS per pair
	langs    []langConfig           // enabled languages, deterministic order
	cacheTTL time.Duration

	// idleGrace is how long a pinned entry lingers after its last session
	// detaches before the language server is torn down. The delay absorbs a
	// client disconnect+reconnect (Claude Desktop) and a quick re-attach without
	// re-paying cold start. A field (not a const) so tests can shorten it.
	idleGrace time.Duration

	// baseCtx is the supervisor lifetime context — the daemon root context, not
	// any single connection or tool-call context. Language servers are shared
	// across all sessions, so tying one to a caller's context would let that
	// caller's disconnect tear it down for everyone. Never nil after newWorkspacePool.
	baseCtx context.Context
	xcode   poolXcodeState
}

type langConfig struct {
	name string
	cfg  config.LSPConfig
}

// poolLifecycle tracks whether a poolEntry's language-server process is running
// or has been hibernated — stopped to reclaim memory while the entry, its warm
// cache, and its map slot survive. See workspacePool.hibernateEntry / wakeLocked.
type poolLifecycle int

const (
	poolActive      poolLifecycle = iota // process running (or warming)
	poolHibernating                      // teardown in progress
	poolHibernated                       // process stopped; next acquire restarts it
)

func (s poolLifecycle) String() string {
	switch s {
	case poolActive:
		return "active"
	case poolHibernating:
		return "hibernating"
	case poolHibernated:
		return "hibernated"
	default:
		return "unknown"
	}
}

type poolEntry struct {
	root     string
	language string
	proxy    *clientProxy
	inv      *cache.Invalidator
	cache    *cache.Cache
	sup      *lsp.Supervisor
	watcher  *lspFSWatcher

	// state is the hibernation lifecycle (poolActive / poolHibernating /
	// poolHibernated). Guarded by workspacePool.mu. The activity timestamp that
	// drives hibernation lives on proxy.lastUsed (touched lock-free on the hot
	// path), not here.
	state poolLifecycle

	// startedAt is when this entry's language server began warming — set when the
	// entry is created and refreshed on each wake from hibernation. It backs the
	// warm-up elapsed time surfaced to tools and session_start while proxy.get()
	// is still nil (handshake incomplete). Guarded by workspacePool.mu.
	startedAt time.Time

	// diagMode is the resolved per-connection diagnostics mode — one of the four
	// vocabulary strings (push / pull / hybrid / pull-requested-but-unavailable),
	// or "" before Initialize resolves it. Set in poolOnStart after Initialize
	// (see resolveDiagMode) and flipped to "hybrid" when a push is observed while
	// in pull mode (see diagnosticsHybridFlip). Guarded by workspacePool.mu.
	diagMode string

	// refs counts the sessions that hold this root as their PINNED primary
	// workspace (attach / re-pin). On-demand routing acquires (routingProxy.route
	// for a non-primary URI) deliberately do NOT pin, so a route target is never
	// prematurely reclaimed mid-call and a never-pinned entry simply lives until
	// daemon shutdown (the pre-refcount behaviour). graceTimer fires teardown
	// after refs falls to zero; a new pin cancels it. closeOnce makes teardown
	// idempotent across the grace reaper and daemon shutdown. All three are
	// guarded by workspacePool.mu (closeOnce is self-synchronising).
	refs       int
	graceTimer *time.Timer
	closeOnce  sync.Once
}

// newWorkspacePool builds the pool. baseCtx is the daemon-lifetime context that
// supervisors run on; pass the daemon root context. Detect-only call sites (the
// CLI workspace resolver) may pass context.Background() — it is only used when
// acquireLang starts a language server.
func newWorkspacePool(baseCtx context.Context, cfg config.Config) *workspacePool {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	var langs []langConfig
	for name, lspCfg := range cfg.LSP {
		// Effective enablement: the user's intent gated on the server actually
		// being installed (automatic mode). An enabled-but-uninstalled language
		// is excluded so its root markers never pollute workspace detection.
		if lspActive(lspCfg) {
			langs = append(langs, langConfig{name: name, cfg: lspCfg})
		}
	}
	// Deterministic order: "go" first for backward compatibility, then alphabetical.
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].name == "go" {
			return true
		}
		if langs[j].name == "go" {
			return false
		}
		return langs[i].name < langs[j].name
	})
	return &workspacePool{
		entries:   make(map[poolKey]*poolEntry),
		langs:     langs,
		cacheTTL:  cfg.Cache.TTL.Duration,
		idleGrace: poolIdleGrace,
		baseCtx:   baseCtx,
		xcode:     newPoolXcodeState(),
	}
}

// poolIdleGrace is the default delay before a pinned entry whose last session
// detached is torn down. See workspacePool.idleGrace.
const poolIdleGrace = 90 * time.Second

// firstStartGrace bounds the inline wait for a freshly started language server.
// A fast/warm server (small module) finishes Initialize+Initialized well inside
// this window, so the first tool call still gets full LSP results inline. A slow
// cold-start (large workspace) returns here within the grace as a not-yet-ready
// entry and keeps warming in the background, so the tool falls back to the
// tree-sitter index instead of blocking until the MCP client times out.
const firstStartGrace = 2 * time.Second

// poolCloseGrace bounds the LSP graceful-shutdown handshake per entry during
// pool.close(). jsonrpc Call/Notify honour their context, so a cold or hung
// language server unblocks at this deadline instead of stalling daemon exit;
// sup.Stop() then kills the process regardless. The daemon's shutdown watchdog
// (shutdownHardDeadline) is the outer backstop.
const poolCloseGrace = 2 * time.Second

// janitorInterval is how often the hibernation janitor scans for idle servers.
// Coarse relative to idle_timeout (minutes), so it adds negligible overhead.
const janitorInterval = 60 * time.Second

// cachePruneInterval is how often the janitor prunes stale jdtls-data dirs, and
// jdtlsCacheMaxAge is how old (by directory mtime) and unused a dir must be to
// be removed. Eclipse workspace storage runs ~50 MB/project, so reclaiming dirs
// for projects untouched for a month keeps the cache bounded.
const (
	cachePruneInterval = 24 * time.Hour
	jdtlsCacheMaxAge   = 30 * 24 * time.Hour
)

// acquireLang returns (or starts) the shared workspace state for root, never
// blocking on a slow cold-start. Pass "" for language to detect from root
// markers; otherwise the named adapter is used directly.
//
// The returned entry may not yet be ready: a cold language server keeps warming
// in the background (on the pool's base context) and proxy.get() stays nil until
// the handshake completes, which the routing proxy surfaces as "LSP server not
// yet ready". Callers that need the server immediately should treat a nil
// proxy.get() as a transient miss, not a failure.
//
// pin records a long-lived session reference on the entry (see poolEntry.refs):
// pass true from a workspace attach / re-pin, false from an on-demand routing
// acquire. A pinned entry is kept alive until its last session releases it (and
// then for idleGrace); an unpinned acquire never affects the refcount.
func (p *workspacePool) acquireLang(ctx context.Context, root, language string, pin bool) (*poolEntry, error) {
	e, readyCh, err := p.startOrReuse(root, language, pin)
	if err != nil {
		return nil, err
	}
	if readyCh == nil {
		return e, nil // reused an existing entry — no warm-up to wait on
	}
	return p.awaitReady(ctx, e, readyCh)
}

// existingDir returns dir when it names an existing directory, else "". Used to
// decide whether a child process can safely be given it as a working directory.
func existingDir(dir string) string {
	if dir == "" {
		return ""
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// startOrReuse returns the existing entry for root, or builds a new one and
// begins warming its language server in the background. For a reused entry the
// returned channel is nil (nothing to wait on); for a freshly started one it
// delivers the first-start outcome (see Supervisor.StartAsync). The pool mutex
// is never held across the warm-up: the entry is published into the map before
// the lock is released, so concurrent callers for the same root reuse it and a
// language server is never spawned twice.
func (p *workspacePool) startOrReuse(root, language string, pin bool) (*poolEntry, <-chan error, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Resolve the language BEFORE building the reuse key: an acquire with an
	// empty language (detect-from-markers) and one with the explicit primary
	// language must collapse to the SAME (root, language) entry, never two
	// servers for one logical workspace language.
	if language == "" {
		language = p.lspLanguageForRoot(root)
		if language == "" {
			return nil, nil, fmt.Errorf("no enabled language matches %s", root)
		}
	}

	if e, ok := p.entries[poolKey{root, language}]; ok {
		// Pin only AFTER any fallible step (wakeLocked): pinning before a wake that
		// errors would leak a reference (refs incremented, no matching release) and
		// cancel the grace timer on an entry that stays hibernated.
		switch e.state {
		case poolHibernated:
			readyCh, err := p.wakeLocked(e)
			if err != nil {
				return nil, nil, err
			}
			if pin {
				p.pinLocked(e)
			}
			slog.Info("pool: waking hibernated LS", "root", root, "language", e.language, "refs", e.refs)
			return e, readyCh, nil
		case poolHibernating:
			// Teardown in flight; the server is not restartable until it settles.
			// Return the not-yet-ready entry so the caller retries (route surfaces
			// "LSP server not yet ready"); the next acquire finds it hibernated and
			// wakes it.
			if pin {
				p.pinLocked(e)
			}
			return e, nil, nil
		default:
			if pin {
				p.pinLocked(e)
			}
			slog.Info("pool: reusing LS", "root", root, "language", e.language, "refs", e.refs)
			return e, nil, nil
		}
	}

	lspCfg, ok := p.cfgFor(language)
	if !ok {
		return nil, nil, fmt.Errorf("language %q not configured or not enabled", language)
	}

	// LRU eviction: before starting a new server, if this language is at its
	// max_workspaces budget, hibernate its least-recently-used running entry.
	// hibernateEntry re-takes p.mu and does blocking teardown, so it runs in a
	// goroutine that parks until startOrReuse releases the lock; the new server
	// starts immediately and the victim's JVM is reclaimed concurrently.
	if victim := p.overBudgetVictimLocked(language, lspCfg.MaxWorkspaces); victim != nil {
		slog.Info("pool: max_workspaces reached — evicting LRU", "language", language, "victim_root", victim.root, "max", lspCfg.MaxWorkspaces)
		go p.hibernateEntry(victim)
	}

	rootURI := protocol.FileURI(root)
	c := cache.New(p.cacheTTL)
	inv := cache.NewInvalidator(c)
	proxy := &clientProxy{}
	e := &poolEntry{root: root, language: language, proxy: proxy, inv: inv, cache: c, state: poolActive, startedAt: time.Now()}
	proxy.touch()

	sup := lsp.NewSupervisor(lspCfg.Command, argsFor(language, root, lspCfg), envFor(lspCfg), lsp.SupervisorOptions{
		OnStart: p.poolOnStart(e, rootURI, lspCfg),
		// Run the server from the workspace it serves, not from the daemon's cwd
		// (which is "/"). Skipped when root is not an existing directory, so a root
		// deleted under a live session degrades to the daemon's cwd rather than
		// failing the spawn outright with ENOENT from chdir.
		Dir: existingDir(root),
	})
	// Warm up on the pool's base (daemon-lifetime) context, not a request ctx.
	readyCh, err := sup.StartAsync(p.baseCtx)
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("starting %s for %s: %w", language, root, err)
	}
	e.sup = sup
	if watcher, err := newLSPFSWatcher(root, proxy); err != nil {
		slog.Warn("lsp: file watcher unavailable", "root", root, "language", language, "err", err)
	} else {
		e.watcher = watcher
		watcher.Start()
	}
	if pin {
		p.pinLocked(e)
	}
	p.entries[poolKey{root, language}] = e
	slog.Info("pool: new workspace (warming)", "root", root, "language", language, "refs", e.refs)
	return e, readyCh, nil
}

// pinLocked increments an entry's session refcount and cancels any pending
// idle-teardown timer (a re-pin during the grace window keeps the server
// alive). Caller must hold p.mu.
func (p *workspacePool) pinLocked(e *poolEntry) {
	e.refs++
	if e.graceTimer != nil {
		e.graceTimer.Stop()
		e.graceTimer = nil
	}
}

// release drops one pinned session reference on root (paired with an
// acquireLang(pin=true)). When the last reference goes, the entry is scheduled
// for teardown after idleGrace rather than torn down immediately, so a client
// reconnect or quick re-attach reuses the warm server. A no-op when root has no
// entry or no outstanding pins (defensive: a session that attached without LSP,
// LanguageNone, holds no pin and must not decrement a sibling's count).
func (p *workspacePool) release(root, language string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[poolKey{root, language}]
	if !ok || e.refs <= 0 {
		return
	}
	e.refs--
	if e.refs > 0 {
		return
	}
	if e.graceTimer != nil {
		e.graceTimer.Stop()
	}
	e.graceTimer = time.AfterFunc(p.idleGrace, func() { p.reapEntry(e) })
	slog.Info("pool: last session detached — scheduling idle teardown", "root", root, "language", language, "grace", p.idleGrace)
}

// reapEntry tears down an entry whose grace window elapsed, but only if it is
// still the mapped entry for root and still has no pins (a pin during the grace
// window cancels the timer, but the callback may already be running). Teardown
// happens outside p.mu — closeEntry performs a bounded LSP shutdown handshake we
// must not hold the pool lock across.
func (p *workspacePool) reapEntry(e *poolEntry) {
	key := poolKey{e.root, e.language}
	p.mu.Lock()
	cur, ok := p.entries[key]
	if !ok || cur != e || e.refs > 0 {
		p.mu.Unlock()
		return
	}
	delete(p.entries, key)
	p.mu.Unlock()
	slog.Info("pool: tearing down idle LS", "root", e.root, "language", e.language)
	ctx, cancel := context.WithTimeout(context.Background(), poolCloseGrace)
	defer cancel()
	e.closeOnce.Do(func() { closeEntry(ctx, e) })
}

// awaitReady waits up to firstStartGrace for a freshly started entry to become
// ready. A first-start failure (e.g. a missing binary, which the supervisor
// will not retry) removes the entry so a later call re-spawns, and surfaces the
// error so attachWorkspace degrades to LanguageNone. On grace or request-context
// expiry the not-yet-ready entry is returned and the supervisor keeps warming.
func (p *workspacePool) awaitReady(ctx context.Context, e *poolEntry, readyCh <-chan error) (*poolEntry, error) {
	select {
	case startErr := <-readyCh:
		if startErr != nil {
			p.removeFailed(e)
			return nil, fmt.Errorf("starting %s for %s: %w", e.language, e.root, startErr)
		}
		return e, nil
	case <-time.After(firstStartGrace):
		return e, nil
	case <-ctx.Done():
		return e, nil
	}
}

// warmupFor reports whether the language server for (root, language) is still
// warming — its handshake incomplete, so proxy.get() is nil — and how long it
// has been warming. Resolution-only: it never starts or wakes a server, so a
// caller (a tool, session_start) can fail fast with an elapsed-time advisory
// instead of blocking on a cold handshake. Returns (false, 0) when no entry
// exists or the server is already ready.
func (p *workspacePool) warmupFor(root, language string) (warming bool, elapsed time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[poolKey{root, language}]
	if !ok {
		return false, 0
	}
	if e.proxy.get() != nil {
		return false, 0
	}
	if e.startedAt.IsZero() {
		return true, 0
	}
	return true, time.Since(e.startedAt)
}

// poolOnStart builds the supervisor OnStart hook: construct the adapter,
// subscribe the invalidator AND the hybrid-flip watcher BEFORE initialized (so
// the first publishDiagnostics burst — sent within ms of initialized — is
// neither lost nor missed by the flip), negotiate the diagnostics mode, run the
// handshake, and publish the ready client into proxy. Re-run on every wake from
// hibernation, so the mode is re-resolved against the freshly-negotiated caps.
//
// Ordering matters twice over: the invalidator subscription must precede
// Initialized (the burst is lost otherwise), and diagMode is resolved BEFORE
// Initialized so the flip watcher sees the correct "pull" mode when the very
// first push arrives (a server only pushes after it receives initialized).
func (p *workspacePool) poolOnStart(e *poolEntry, rootURI string, lspCfg config.LSPConfig) func(context.Context, *jsonrpc.Conn) error {
	return func(startCtx context.Context, conn *jsonrpc.Conn) error {
		ad, err := newAdapter(e.language, conn)
		if err != nil {
			return err
		}
		// One refresh wiring point for every adapter — see wrapServerRequest.
		conn.SetRequestHandler(p.wrapServerRequest(e, conn.RequestHandler()))
		clearEntryPullState(e) // fresh/woken server: drop stale pull result IDs (see helper)
		requested := resolveRequestedDiagnosticsMode(lspCfg.Diagnostics, e.language)
		ad.Subscribe(e.inv.Handle)
		ad.Subscribe(p.diagnosticsHybridFlip(e))
		if _, err := ad.Initialize(startCtx, initParamsFor(ad, e.language, rootURI, requested)); err != nil {
			return fmt.Errorf("initialize: %w", err)
		}
		p.resolveDiagMode(e, ad, requested)
		if err := ad.Initialized(startCtx); err != nil {
			return fmt.Errorf("initialized: %w", err)
		}
		e.proxy.set(ad)
		slog.Info("pool: LS ready", "root", e.root, "language", e.language, "diag", p.diagModeFor(e.root, e.language))
		return nil
	}
}

// removeFailed deletes a dead entry — one whose first start failed and which the
// supervisor will not retry — from the pool so a later acquire re-spawns, then
// tears down its supervisor and cache. The identity check guards against
// deleting a different entry a concurrent caller may have inserted for the same
// root.
func (p *workspacePool) removeFailed(e *poolEntry) {
	key := poolKey{e.root, e.language}
	p.mu.Lock()
	if cur, ok := p.entries[key]; ok && cur == e {
		delete(p.entries, key)
	}
	p.mu.Unlock()
	// closeOnce guards against the rare race where a parallel reapEntry for the
	// same entry already started teardown (a pin-then-immediate-fail sequence).
	e.closeOnce.Do(func() {
		if e.sup != nil {
			e.sup.Stop()
		}
		if e.watcher != nil {
			e.watcher.Stop()
		}
		if e.cache != nil {
			e.cache.Close()
		}
	})
}

func (p *workspacePool) cfgFor(language string) (config.LSPConfig, bool) {
	for _, l := range p.langs {
		if l.name == language {
			return l.cfg, true
		}
	}
	return config.LSPConfig{}, false
}

// lookup returns the entry for (root, language) if it has already been
// acquired, or nil if no entry exists. Unlike acquire, lookup never starts a
// new LS.
func (p *workspacePool) lookup(root, language string) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[poolKey{root, language}]
}

// entriesUnderRoot returns every acquired entry whose workspace root is root or
// a directory beneath it, across all languages. One root may host several
// language servers directly (Go + HTML co-located), and a secondary whose own
// root marker sits in a subdirectory (e.g. index.html under site/) is carved
// into a sub-root — so aggregating a workspace's diagnostics must reach into the
// subtree, not just the exact root. The returned slice is a snapshot; never
// starts a new LS.
func (p *workspacePool) entriesUnderRoot(root string) []*poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*poolEntry
	for k, e := range p.entries {
		if k.root == root || strings.HasPrefix(k.root, root+"/") {
			out = append(out, e)
		}
	}
	return out
}

// close shuts down all LS processes. Safe to call from multiple goroutines
// but intended to be called once at daemon shutdown. Entries are torn down
// concurrently under a bounded deadline so one slow language server cannot
// stall the others or daemon exit. Pending grace timers are cancelled so idle
// entries are shut down immediately rather than after their grace window.
func (p *workspacePool) close() {
	p.mu.Lock()
	// Snapshot entries and cancel any pending grace timers while holding the lock.
	entries := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		if e.graceTimer != nil {
			e.graceTimer.Stop()
			e.graceTimer = nil
		}
		entries = append(entries, e)
	}
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), poolCloseGrace)
	defer cancel()

	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// closeOnce ensures a reapEntry goroutine already running for this
			// entry (race between grace timer firing and daemon shutdown) does
			// not double-close the supervisor and cache.
			e.closeOnce.Do(func() { closeEntry(ctx, e) })
		}()
	}
	wg.Wait()
}

// closeEntry shuts down a single pool entry: a best-effort bounded LSP
// Shutdown/Exit handshake, then sup.Stop() to kill the process regardless of
// whether the handshake completed. The handshake is politeness; sup.Stop()
// (which cancels the supervisor's exec.CommandContext, killing the process) is
// the real guarantee the language server dies.
func closeEntry(ctx context.Context, e *poolEntry) {
	if c := e.proxy.get(); c != nil {
		_ = c.Shutdown(ctx)
		_ = c.Exit(ctx)
	}
	if e.sup != nil {
		e.sup.Stop()
	}
	if e.watcher != nil {
		e.watcher.Stop()
	}
	if e.cache != nil {
		e.cache.Close()
	}
}
