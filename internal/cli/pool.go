package cli

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/fswatch"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
)

// The pool is split across files by concern: workspace detection + the CLI
// resolver live in pool_detect.go; per-language adapter construction and init
// params in pool_adapters.go; idle hibernation, LRU eviction, the janitor, live
// PID/RSS reporting, and jdtls-data pruning in pool_hibernate.go; entry teardown
// (idle-grace reaping, first-start failure eviction, and daemon-wide close) in
// pool_lifecycle.go. This file holds the pool state and the acquire/release/pin
// paths.

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
	mu      sync.Mutex
	entries map[poolKey]*poolEntry // key: (root, language); one LS per pair

	// langs is the global effective (enabled + installed) language set. Project
	// detection and routing derive their own cached effective slice from baseConfig
	// plus the governing workspace config. It is built once by newWorkspacePool and
	// thereafter mutated ONLY by enableLanguage
	// (live `enable-lsp`), which replaces it wholesale (copy-on-write) rather than
	// appending in place. Readers on the hot path (Detect, fileLanguage) range a
	// snapshot taken under langsMu.RLock and never mutate it, so a concurrent
	// enable never tears a reader's slice. langsMu guards only the slice header;
	// the backing array of any published slice is immutable.
	langs   []langConfig
	langsMu sync.RWMutex

	// langsGen counts the times enableLanguage has widened the effective set. It
	// is the signal a live connection watches to know its primary language may be
	// resolvable now when it was not at attach time: `enable-lsp` deliberately
	// leaves existing pool entries and pinned workspaces untouched, so without
	// this a session that attached as LanguageNone would report "none attached"
	// for its whole life while per-file routing quietly served it. Atomic rather
	// than langsMu-guarded because every tool call reads it (see
	// connSession.refreshPrimaryIfStale) and must not contend with the hot
	// detection path.
	langsGen atomic.Uint64

	// langResolves counts calls to effectiveLanguages. It exists because the cost
	// this fix has to keep bounded is invisible in the return value: a resolution
	// walks ancestors and stats the project config even on a cache hit, so a
	// caller that asks twice per request doubles that walk with nothing to show
	// for it. Routing shipped exactly that regression once (Detect and
	// fileLanguage each resolving), and a benchmark could not pin it — on a loaded
	// machine the variance is larger than the effect. A counter can.
	//
	// Where it is incremented is load-bearing, for the same reason the language
	// cache's lock scope is: it counts CALLS, from the first statement of
	// effectiveLanguages, so cache hits and both early returns are counted too.
	// Moving it past the cache-hit return would count misses — and misses are not
	// the cost, since the walk and the stats run on a hit as well. The guard would
	// then pass while the regression it exists to catch was back.
	langResolves atomic.Uint64

	baseConfig     config.Config // global base for per-workspace LSP overrides
	languageConfig languageConfigState
	cacheTTL       time.Duration
	cacheMaxSize   int

	// idleGrace is how long a pinned entry lingers after its last session
	// detaches before the language server is torn down. The delay absorbs a
	// client disconnect+reconnect (Claude Desktop) and a quick re-attach without
	// re-paying cold start. A field (not a const) so tests can shorten it.
	idleGrace time.Duration

	// startGrace bounds awaitReady's inline wait for a freshly started server
	// (see firstStartGrace). A field — not the const directly — so tests can
	// shorten it to exercise the grace path deterministically. A zero value
	// means "use firstStartGrace", so pool literals built without a constructor
	// (tests) keep the production timing unless they opt in.
	startGrace time.Duration

	// baseCtx is the supervisor lifetime context — the daemon root context, not
	// any single connection or tool-call context. Language servers are shared
	// across all sessions, so tying one to a caller's context would let that
	// caller's disconnect tear it down for everyone. Never nil after newWorkspacePool.
	baseCtx context.Context
	xcode   poolXcodeState

	// homePlumbWarn rate-limits the "ignoring residual ~/.plumb" warning to
	// once per pool: detect runs on hot paths (per-file routing), and the
	// remediation message would otherwise flood the log while the residue sits
	// there.
	homePlumbWarn sync.Once

	// watcherFactory builds an entry's file watcher; nil subscribes to watchHub
	// (newLSPFSWatcherVia).
	// A test seam: a slow or probing factory shows the watcher is built without
	// p.mu (PLAN-536).
	watcherFactory func(root string, proxy *clientProxy) (*lspFSWatcher, error)

	// closed is set by close(), under mu: a watcher whose build finishes after
	// it is stopped rather than attached (attachWatcher).
	closed bool

	// budget is the janitor's last sample of the advisory global target
	// (pool_budget.go, PLAN-534). Guarded by mu.
	budget poolBudget

	// watchHub shares one OS file watcher per root between this pool's language
	// servers and the topology index (PLAN-503); nil gives each its own. Set by
	// the daemon before first use.
	watchHub *fswatch.Hub

	// resyncs counts the language-server resyncs after lost watcher events and
	// lastResynced names the latest (pool_resync.go, PLAN-538). The three
	// durations override the resync and watcher-retry timings in tests (0 =
	// the defaults). Guarded by mu.
	resyncs           int
	lastResynced      poolKey
	resyncQuiet       time.Duration
	resyncMinInterval time.Duration
	watcherRetryDelay time.Duration

	// liveEnabled names the languages `plumb enable-lsp` turned on in this
	// daemon. A later global config reload keeps them enabled — the enable is a
	// deliberate pool-local act the config file does not record. Guarded by mu.
	liveEnabled map[string]bool
	// fileLSP is the [lsp.*] map the global config file last had, so a reload
	// can tell an edit of a language's `enabled` (which overrides a live
	// enable) from a reload that left it alone. Guarded by mu.
	fileLSP map[string]config.LSPConfig
}

type langConfig struct {
	name string
	cfg  config.LSPConfig
}

// poolLifecycle tracks a poolEntry's language-server process: first starting,
// running, or hibernated — stopped to reclaim memory while the entry, its warm
// cache, and its map slot survive — and waking again. See
// workspacePool.hibernateEntry / wakeLocked.
//
// poolActive stays the zero value so an entry built directly (tests) is running.
type poolLifecycle int

const (
	poolActive      poolLifecycle = iota // process running and its handshake done
	poolHibernating                      // teardown in progress
	poolHibernated                       // process stopped; next acquire restarts it
	poolStarting                         // first start: process spawned, handshake not done
	poolWaking                           // restart after hibernation, handshake not done
)

func (s poolLifecycle) String() string {
	switch s {
	case poolActive:
		return "active"
	case poolHibernating:
		return "hibernating"
	case poolHibernated:
		return "hibernated"
	case poolStarting:
		return "starting"
	case poolWaking:
		return "waking"
	default:
		return "unknown"
	}
}

// running reports whether the entry's process is up or coming up: the states
// hibernation, budgets and reuse treat alike. Only the handshake separates
// starting and waking from active.
func (s poolLifecycle) running() bool {
	return s == poolActive || s == poolStarting || s == poolWaking
}

type poolEntry struct {
	root     string
	language string
	lspCfg   config.LSPConfig // resolved global + project config captured at creation
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

	// diagDowngraded records that a negotiated pull returned method-not-found
	// (-32601) and permanently downgraded this entry to push (downgradeDiagMode).
	// It makes the downgrade a SESSION fact: it survives a hibernation wake — where
	// poolOnStart re-runs resolveDiagMode on the same surviving entry, which would
	// otherwise resolve back to pull, re-pull, fail with -32601 again, and re-warn
	// once per wake — but not a daemon restart or an explicit server restart (both
	// build/clear a fresh entry), so a genuine restart still re-negotiates from
	// config. Guarded by workspacePool.mu.
	diagDowngraded bool

	// goWorkOff is the go.work this entry's Go language server was started with
	// GOWORK=off against (goLSPEnv), or "" when it runs with the environment it
	// inherited. Set once at creation, before the entry is published; a wake reuses
	// the same supervisor and so the same environment. Guarded by workspacePool.mu.
	goWorkOff string

	// refs counts the leases that keep this entry alive between calls: a session
	// holding the root as its PINNED primary (attach / re-pin), and a session
	// BORROWING it — a routed or fanned-out call from a session whose primary is
	// elsewhere, such as a nested root (plumb-ops routing into plumb). The pool
	// owns the entry; lease holders never close it themselves. Calls in progress
	// are counted separately (clientProxy.inflight). graceTimer fires teardown
	// once refs is zero and no call is running; a new lease or call cancels it.
	// closeOnce makes teardown idempotent across the grace reaper and daemon
	// shutdown. All three are guarded by workspacePool.mu (closeOnce is
	// self-synchronising); proxy.held mirrors refs > 0 for the lock-free path.
	refs       int
	graceTimer *time.Timer
	closeOnce  sync.Once

	// reconfiguring is set while a config change restarts this entry in place
	// (pool_reconfigure.go), so concurrent reloads do not start a second restart.
	// Guarded by workspacePool.mu.
	reconfiguring bool
	// reconfigureAgain marks a reconfigure that arrived while one was running;
	// the running restart re-resolves the config and goes again. Guarded by
	// workspacePool.mu.
	reconfigureAgain bool
	// hibernatedAt is when the entry last finished hibernating (zero while it
	// runs); trimmed marks a long-hibernated leased entry whose cache the
	// janitor already dropped (pool_budget.go). Guarded by workspacePool.mu.
	hibernatedAt time.Time
	trimmed      bool
	// resyncTimer is a pending resync after lost watcher events; lastResync is
	// when the server was last resynced; watcherRetrying marks a running
	// watcher-recreation loop and watcherFailures counts its attempts
	// (pool_resync.go, PLAN-538). Guarded by workspacePool.mu.
	resyncTimer     *time.Timer
	lastResync      time.Time
	watcherRetrying bool
	watcherFailures int
	// watcherRetryAgain records a failure that arrived during a recreation;
	// watcherHealthySince is when the last recreated watcher attached (a long
	// healthy run resets the backoff); resyncStreak counts back-to-back resyncs
	// (it doubles the minimum interval). Guarded by workspacePool.mu.
	watcherRetryAgain   bool
	watcherHealthySince time.Time
	resyncStreak        int
}

// newWorkspacePool builds the pool. baseCtx is the daemon-lifetime context that
// supervisors run on; pass the daemon root context. Detect-only call sites (the
// CLI workspace resolver) may pass context.Background() — it is only used when
// acquireLang starts a language server.
func newWorkspacePool(baseCtx context.Context, cfg config.Config) *workspacePool {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	return &workspacePool{
		entries:        make(map[poolKey]*poolEntry),
		langs:          activeLanguages(cfg),
		baseConfig:     cfg,
		fileLSP:        maps.Clone(cfg.LSP),
		languageConfig: languageConfigState{cache: make(map[string]cachedWorkspaceLanguages)},
		cacheTTL:       cfg.Cache.TTL.Duration,
		cacheMaxSize:   cfg.Cache.MaxSize,
		idleGrace:      poolIdleGrace,
		startGrace:     firstStartGrace,
		baseCtx:        baseCtx,
		xcode:          newPoolXcodeState(),
	}
}

// sortLangs orders the effective-language slice deterministically: "go" first
// (backward compatibility — it is the historical default primary), then the rest
// alphabetically. Shared by newWorkspacePool and enableLanguage so a
// live-enabled language lands in the same stable order it would have at startup.
func sortLangs(langs []langConfig) {
	sort.Slice(langs, func(i, j int) bool {
		if langs[i].name == "go" {
			return true
		}
		if langs[j].name == "go" {
			return false
		}
		return langs[i].name < langs[j].name
	})
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

// supStopGrace is the extra budget, beyond the poolCloseGrace handshake, that
// pool.close() allows the per-entry teardown goroutines (whose blocking step is
// the otherwise-unbounded sup.Stop() — it waits on the supervisor loop goroutine
// to observe context cancellation and reap the killed process) before it is
// abandoned via waitWithTimeout. sup.Stop() completes in microseconds for a
// healthy supervisor; this only ever expires on a wedge. The pool.close() outer
// wait is therefore poolCloseGrace + supStopGrace, kept well under the shutdown
// watchdog — see shutdownHardDeadline.
const supStopGrace = 1 * time.Second

// janitorInterval is how often the hibernation janitor scans for idle servers.
// Coarse relative to idle_timeout (minutes), so it adds negligible overhead.
const janitorInterval = 60 * time.Second

// cachePruneInterval is how often the janitor prunes stale per-root server state
// dirs (serverStateDirs), and serverStateMaxAge is how old (by directory mtime)
// and unused a dir must be to be removed. Eclipse workspace storage runs ~50 MB
// per project and an IntelliJ system path is larger still, so reclaiming dirs
// for projects untouched for a month keeps the cache bounded.
const (
	cachePruneInterval = 24 * time.Hour
	serverStateMaxAge  = 30 * 24 * time.Hour
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
// pass true from a workspace attach / re-pin. A pinned entry is kept alive until
// its last session releases it (and then for idleGrace). pin=false is an acquire
// with no lease of its own: the entry is reaped idleGrace after it returns
// unless something leases it meanwhile. Routed calls use acquireCall instead,
// which keeps the entry for the whole call.
func (p *workspacePool) acquireLang(ctx context.Context, root, language string, pin bool) (*poolEntry, error) {
	if !pin {
		e, done, err := p.acquireCall(ctx, root, language)
		if done != nil {
			done()
		}
		return e, err
	}
	e, readyCh, err := p.startOrReuse(root, language, true, false)
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
// is held only to look up, publish and lease the entry: the language and config
// resolution, the Go environment and the file watcher's initial walk run
// without it, and the server's warm-up runs on its own goroutine. The entry is
// published into the map before the lock is released, so concurrent callers for
// the same root reuse it and a language server is never spawned twice.
//
// pin takes a session lease (refs) and call takes a call lease (inflight), both
// under the lock and only once the entry is known good; see leaseLocked.
func (p *workspacePool) startOrReuse(root, language string, pin, call bool) (*poolEntry, <-chan error, error) {
	// Everything slow runs OUTSIDE p.mu (PLAN-536): language detection and the
	// per-workspace config walk the tree and may parse project TOML, the Go
	// environment reads go.work / go.mod / go.env, and a file watcher's initial
	// walk can take up to a minute on Linux. Holding the pool-wide mutex across
	// any of them stalled every other root's acquire behind one slow root.
	//
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

	// Eligibility is checked BEFORE the reuse lookup, not after it. The set of
	// languages a workspace may run is a property of that workspace's config, and
	// the config can move under a live daemon — a project that turns [lsp.<lang>]
	// off must stop being handed that server, not keep the one it already has
	// because an entry happens to be in the map. Checking only on the miss path
	// made the refusal depend on whether the daemon had started the server before
	// the config changed, which is not a distinction the user asked for.
	//
	// It refuses; it does not tear down. An entry left behind is unreferenced by
	// this acquire and reaped by the idle path, so revoking a language never kills
	// a request already in flight against it.
	lspCfg, ok := p.cfgForWorkspace(root, language)
	if !ok {
		return nil, nil, fmt.Errorf("language %q not configured or not enabled for %s", language, root)
	}
	key := poolKey{root, language}

	p.mu.Lock()
	if e, ok := p.entries[key]; ok {
		return p.reuseUnlock(e, pin, call)
	}
	p.mu.Unlock()

	// A miss: compute the start environment unlocked, then re-check — another
	// acquire may have started the server meanwhile, and a server is never
	// spawned twice for one key.
	env, goWorkOff := goLSPEnv(language, root, lspCfg, envFor(lspCfg))
	p.mu.Lock()
	if e, ok := p.entries[key]; ok {
		return p.reuseUnlock(e, pin, call)
	}
	e, readyCh, err := p.startLocked(key, lspCfg, env, goWorkOff)
	if err != nil {
		p.mu.Unlock()
		return nil, nil, err
	}
	p.leaseLocked(e, pin, call)
	slog.Info("pool: new workspace (warming)", "root", root, "language", language, "refs", e.refs)
	p.mu.Unlock()
	p.attachWatcher(e)
	return e, readyCh, nil
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
		if _, err := ad.Initialize(startCtx, initParamsFor(ad, e.language, rootURI, requested, lspCfg)); err != nil {
			return fmt.Errorf("initialize: %w", err)
		}
		p.resolveDiagMode(e, ad, requested)
		if err := ad.Initialized(startCtx); err != nil {
			return fmt.Errorf("initialized: %w", err)
		}
		e.proxy.set(ad)
		p.markActive(e)
		slog.Info("pool: LS ready", "root", e.root, "language", e.language, "diag", p.diagModeFor(e.root, e.language))
		return nil
	}
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
