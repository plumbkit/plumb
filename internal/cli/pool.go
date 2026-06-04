package cli

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/lsp"
	"github.com/golimpio/plumb/internal/lsp/adapters/gopls"
	htmlls "github.com/golimpio/plumb/internal/lsp/adapters/html"
	"github.com/golimpio/plumb/internal/lsp/adapters/jdtls"
	"github.com/golimpio/plumb/internal/lsp/adapters/kotlin"
	"github.com/golimpio/plumb/internal/lsp/adapters/pyright"
	"github.com/golimpio/plumb/internal/lsp/adapters/rust"
	"github.com/golimpio/plumb/internal/lsp/adapters/swift"
	tsls "github.com/golimpio/plumb/internal/lsp/adapters/typescript"
	"github.com/golimpio/plumb/internal/lsp/adapters/zig"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// workspacePool keeps one language-server process alive per workspace root.
// Multiple MCP sessions targeting the same root share a single LS process,
// its cache, and its diagnostic stream.
//
// The pool supports multiple languages (Go via gopls, Python via pyright).
// Detect() resolves a path → (root, language) tuple from configured root
// markers; acquireLang() starts the right adapter for that language.
//
// Concurrency: all methods are safe for concurrent use.
type workspacePool struct {
	mu       sync.Mutex
	entries  map[string]*poolEntry // key: root path; one LS per root
	langs    []langConfig          // enabled languages, deterministic order
	cacheTTL time.Duration

	// idleGrace is how long a pinned entry lingers after its last session
	// detaches before the language server is torn down. The delay absorbs a
	// client disconnect+reconnect (Claude Desktop) and a quick re-attach without
	// re-paying cold start. A field (not a const) so tests can shorten it.
	idleGrace time.Duration

	// baseCtx is the supervisor lifetime context — the daemon root context, not
	// any single connection or tool-call context. Language servers are shared
	// across all sessions, so tying one to a caller's context would let that
	// caller's disconnect tear it down for everyone. Never nil after
	// newWorkspacePool (guarded to context.Background()).
	baseCtx context.Context
}

type langConfig struct {
	name string
	cfg  config.LSPConfig
}

type poolEntry struct {
	root     string
	language string
	proxy    *clientProxy
	inv      *cache.Invalidator
	cache    *cache.Cache
	sup      *lsp.Supervisor
	watcher  *lspFSWatcher

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
		if lspCfg.Enabled {
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
		entries:   make(map[string]*poolEntry),
		langs:     langs,
		cacheTTL:  cfg.Cache.TTL.Duration,
		idleGrace: poolIdleGrace,
		baseCtx:   baseCtx,
	}
}

// poolIdleGrace is the default delay before a pinned entry whose last session
// detached is torn down. See workspacePool.idleGrace.
const poolIdleGrace = 90 * time.Second

// LanguageNone is the sentinel language returned by Detect for workspaces
// that are explicitly marked (via .plumb/) but have no enabled LSP language.
// Filesystem tools, stats attribution, and project config all still work for
// these workspaces; LSP tools fail with "LSP server not yet ready".
const LanguageNone = "none"

// Detect walks up from start looking for a workspace root, with three
// markers tried in priority order at each directory (nearest directory wins,
// since the walk returns on the first match):
//
//  1. A `.plumb/` marker. If an LSP language is also detectable from this
//     directory or any ancestor, return (root, language). Otherwise return
//     (root, "none") — the user marked this directory as a workspace, so we
//     respect that even without LSP support.
//  2. A configured language's root marker (`go.mod`, `pyproject.toml`, ...).
//     Returns (root, language).
//  3. A `.git/` directory. A git repository is an unambiguous project
//     boundary, so a repo with no language marker (a scripts / multi-language
//     repo) still resolves — returned as (root, "none"). This is what lets
//     such workspaces attach in the default config; without it the session
//     never resolves and the TUI shows "resolving…" forever. The user's $HOME
//     is excluded: a dotfiles repo at $HOME must not turn all of $HOME into a
//     workspace.
//
// If no marker is found, walk up to the parent. If we walk past the filesystem
// root, return an error.
func (p *workspacePool) Detect(start string) (root, language string, err error) {
	// Stat $HOME once so the .git guard below can compare by filesystem
	// identity (os.SameFile) rather than by string — a raw compare is defeated
	// by a trailing slash or a symlink/firmlink alias of $HOME.
	var homeInfo os.FileInfo
	if home, herr := os.UserHomeDir(); herr == nil && home != "" {
		homeInfo, _ = os.Stat(home)
	}
	d := filepath.Clean(start)
	for {
		// Highest priority: explicit .plumb marker. Honour it even when no
		// LSP language matches — the user has declared this directory a
		// plumb workspace, and stats / project config should follow that
		// declaration regardless of whether gopls or pyright can attach.
		if _, err := os.Stat(filepath.Join(d, ".plumb")); err == nil {
			if lang := p.detectLanguageAt(d); lang != "" {
				return d, lang, nil
			}
			return d, LanguageNone, nil
		}
		// Next: first language whose root marker exists.
		for _, l := range p.langs {
			for _, marker := range l.cfg.RootMarkers {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return d, l.name, nil
				}
			}
		}
		// Lowest priority: a .git directory marks a project boundary even
		// without a language. Skip $HOME (by filesystem identity, so a
		// non-canonical spelling cannot defeat the guard) so a dotfiles repo
		// there does not capture the whole home directory, and skip the
		// filesystem root.
		if d != filepath.Dir(d) && !sameDirAs(d, homeInfo) {
			if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
				return d, LanguageNone, nil
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", "", fmt.Errorf("no project root found in or above %s", start)
		}
		d = parent
	}
}

// sameDirAs reports whether dir refers to the same directory as info (typically
// the user's $HOME), comparing by filesystem identity via os.SameFile. This is
// robust to trailing slashes, "."/".." segments, and symlink / macOS-firmlink
// aliasing, where a raw string compare against $HOME would be defeated by any
// non-canonical spelling. Returns false when info is nil (home undeterminable)
// or dir cannot be stat'd, leaving the .git guard inert rather than refusing a
// legitimate repo in those cases.
func sameDirAs(dir string, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	di, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return os.SameFile(di, info)
}

// SynthesiseRoot returns a synthetic workspace root for seedDir, used as a
// last resort when Detect has already failed. It walks up from seedDir
// looking for a .git directory (the conventional project-root signal for
// unrecognised languages). If found, that directory is returned. If the
// filesystem root is reached without finding .git, seedDir itself is
// returned as the safest approximation.
//
// SynthesiseRoot must only be called on the Detect error path in
// OnBeforeTool — never inside route() or LSP-routing paths.
func (p *workspacePool) SynthesiseRoot(seedDir string) string {
	d := seedDir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return seedDir // reached filesystem root — use the seed itself
		}
		d = parent
	}
}

// detectLanguageAt returns the language for dir based on which root marker
// is present at dir or any ancestor. Used after a .plumb/ marker is found
// to determine which adapter to start.
func (p *workspacePool) detectLanguageAt(dir string) string {
	d := dir
	for {
		for _, l := range p.langs {
			for _, marker := range l.cfg.RootMarkers {
				if _, err := os.Stat(filepath.Join(d, marker)); err == nil {
					return l.name
				}
			}
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// resolveCLIWorkspace resolves start to the same workspace root the daemon
// would use, without acquiring or starting a language server. If no project
// marker exists, it returns start unchanged so explicit non-project inspection
// paths keep their current behaviour.
func resolveCLIWorkspace(start string, cfg config.Config) (string, error) {
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getwd: %w", err)
		}
		start = cwd
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("resolving workspace path %s: %w", start, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat workspace path %s: %w", abs, err)
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}
	root, _, err := newWorkspacePool(context.Background(), cfg).Detect(abs)
	if err != nil {
		return abs, nil
	}
	return root, nil
}

// firstStartGrace bounds the inline wait for a freshly started language server.
// A fast/warm server (small module) finishes Initialize+Initialized well inside
// this window, so the first tool call still gets full LSP results inline. A slow
// cold-start (large workspace) returns here within the grace as a not-yet-ready
// entry and keeps warming in the background, so the tool falls back to the
// tree-sitter index instead of blocking until the MCP client times out.
const firstStartGrace = 2 * time.Second

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
	return p.awaitReady(ctx, root, e, readyCh)
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

	if e, ok := p.entries[root]; ok {
		if pin {
			p.pinLocked(e)
		}
		slog.Info("pool: reusing LS", "root", root, "language", e.language, "refs", e.refs)
		return e, nil, nil
	}

	if language == "" {
		language = p.detectLanguageAt(root)
		if language == "" {
			return nil, nil, fmt.Errorf("no enabled language matches %s", root)
		}
	}
	lspCfg, ok := p.cfgFor(language)
	if !ok {
		return nil, nil, fmt.Errorf("language %q not configured or not enabled", language)
	}

	rootURI := protocol.FileURI(root)
	c := cache.New(p.cacheTTL)
	inv := cache.NewInvalidator(c)
	proxy := &clientProxy{}
	e := &poolEntry{root: root, language: language, proxy: proxy, inv: inv, cache: c}

	sup := lsp.NewSupervisor(lspCfg.Command, argsFor(language, root, lspCfg), envFor(lspCfg), lsp.SupervisorOptions{
		OnStart: poolOnStart(language, root, rootURI, inv, proxy),
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
	p.entries[root] = e
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
func (p *workspacePool) release(root string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[root]
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
	e.graceTimer = time.AfterFunc(p.idleGrace, func() { p.reapEntry(root, e) })
	slog.Info("pool: last session detached — scheduling idle teardown", "root", root, "grace", p.idleGrace)
}

// reapEntry tears down an entry whose grace window elapsed, but only if it is
// still the mapped entry for root and still has no pins (a pin during the grace
// window cancels the timer, but the callback may already be running). Teardown
// happens outside p.mu — closeEntry performs a bounded LSP shutdown handshake we
// must not hold the pool lock across.
func (p *workspacePool) reapEntry(root string, e *poolEntry) {
	p.mu.Lock()
	cur, ok := p.entries[root]
	if !ok || cur != e || e.refs > 0 {
		p.mu.Unlock()
		return
	}
	delete(p.entries, root)
	p.mu.Unlock()
	slog.Info("pool: tearing down idle LS", "root", root, "language", e.language)
	ctx, cancel := context.WithTimeout(context.Background(), poolCloseGrace)
	defer cancel()
	e.closeOnce.Do(func() { closeEntry(ctx, e) })
}

// awaitReady waits up to firstStartGrace for a freshly started entry to become
// ready. A first-start failure (e.g. a missing binary, which the supervisor
// will not retry) removes the entry so a later call re-spawns, and surfaces the
// error so attachWorkspace degrades to LanguageNone. On grace or request-context
// expiry the not-yet-ready entry is returned and the supervisor keeps warming.
func (p *workspacePool) awaitReady(ctx context.Context, root string, e *poolEntry, readyCh <-chan error) (*poolEntry, error) {
	select {
	case startErr := <-readyCh:
		if startErr != nil {
			p.removeFailed(root, e)
			return nil, fmt.Errorf("starting %s for %s: %w", e.language, root, startErr)
		}
		return e, nil
	case <-time.After(firstStartGrace):
		return e, nil
	case <-ctx.Done():
		return e, nil
	}
}

// poolOnStart builds the supervisor OnStart hook: construct the adapter,
// subscribe the invalidator BEFORE initialized (so the first publishDiagnostics
// burst — sent within ms of initialized — is not lost), run the handshake, and
// publish the ready client into proxy.
func poolOnStart(language, root, rootURI string, inv *cache.Invalidator, proxy *clientProxy) func(context.Context, *jsonrpc.Conn) error {
	return func(startCtx context.Context, conn *jsonrpc.Conn) error {
		ad, err := newAdapter(language, conn)
		if err != nil {
			return err
		}
		ad.Subscribe(inv.Handle)
		if _, err := ad.Initialize(startCtx, initParamsFor(language, rootURI)); err != nil {
			return fmt.Errorf("initialize: %w", err)
		}
		if err := ad.Initialized(startCtx); err != nil {
			return fmt.Errorf("initialized: %w", err)
		}
		proxy.set(ad)
		slog.Info("pool: LS ready", "root", root, "language", language)
		return nil
	}
}

// removeFailed deletes a dead entry — one whose first start failed and which the
// supervisor will not retry — from the pool so a later acquire re-spawns, then
// tears down its supervisor and cache. The identity check guards against
// deleting a different entry a concurrent caller may have inserted for the same
// root.
func (p *workspacePool) removeFailed(root string, e *poolEntry) {
	p.mu.Lock()
	if cur, ok := p.entries[root]; ok && cur == e {
		delete(p.entries, root)
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

// newAdapter constructs the right adapter for a language.
func newAdapter(language string, conn *jsonrpc.Conn) (lsp.Client, error) {
	switch language {
	case "go":
		return gopls.New(conn), nil
	case "java":
		return jdtls.New(conn), nil
	case "python":
		return pyright.New(conn), nil
	case "rust":
		return rust.New(conn), nil
	case "swift":
		return swift.New(conn), nil
	case "zig":
		return zig.New(conn), nil
	case "typescript":
		return tsls.New(conn), nil
	case "kotlin":
		return kotlin.New(conn), nil
	case "html":
		return htmlls.New(conn), nil
	default:
		return nil, fmt.Errorf("no adapter registered for language %q", language)
	}
}

// initParamsFor builds the Initialize params for a language.
func initParamsFor(language, rootURI string) protocol.InitializeParams {
	switch language {
	case "java":
		return jdtls.DefaultInitParams(rootURI)
	case "python":
		return pyright.DefaultInitParams(rootURI)
	case "rust":
		return rust.DefaultInitParams(rootURI)
	case "swift":
		return swift.DefaultInitParams(rootURI)
	case "zig":
		return zig.DefaultInitParams(rootURI)
	case "typescript":
		return tsls.DefaultInitParams(rootURI)
	case "kotlin":
		return kotlin.DefaultInitParams(rootURI)
	case "html":
		return htmlls.DefaultInitParams(rootURI)
	default:
		return gopls.DefaultInitParams(rootURI)
	}
}

// argsFor returns the supervisor args for the given language and workspace root.
// For most languages this is lspCfg.Args verbatim. Java is special: jdtls
// requires a -data <dir> argument pointing to an Eclipse workspace storage
// directory. Using a per-root directory prevents classpath conflicts when
// multiple Java projects are open simultaneously.
func argsFor(language, root string, lspCfg config.LSPConfig) []string {
	if language != "java" {
		return lspCfg.Args
	}
	dataDir := jdtlsDataDir(root)
	_ = os.MkdirAll(dataDir, 0o700)
	out := make([]string, len(lspCfg.Args), len(lspCfg.Args)+2)
	copy(out, lspCfg.Args)
	return append(out, "-data", dataDir)
}

// jdtlsDataDir returns a per-workspace Eclipse workspace data directory for
// jdtls. The directory name is derived from a hash of the workspace root so
// each project gets isolated Eclipse state.
func jdtlsDataDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(config.CacheDir(), "jdtls-data", fmt.Sprintf("%x", sum[:8]))
}

// lookup returns the entry for root if it has already been acquired, or nil
// if no entry exists. Unlike acquire, lookup never starts a new LS.
func (p *workspacePool) lookup(root string) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entries[root]
}

// poolCloseGrace bounds the LSP graceful-shutdown handshake per entry during
// pool.close(). jsonrpc Call/Notify honour their context, so a cold or hung
// language server unblocks at this deadline instead of stalling daemon exit;
// sup.Stop() then kills the process regardless. The daemon's shutdown watchdog
// (shutdownHardDeadline) is the outer backstop.
const poolCloseGrace = 2 * time.Second

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
