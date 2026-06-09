package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/monitor"
	"github.com/plumbkit/plumb/internal/tools"
)

// The daemon is split across files by concern: the connection registry + idle
// reaper live in daemon_registry.go; runtime-file paths and the detached-process
// spawn in daemon_paths.go; workspace-seed resolution from tool args / roots in
// daemon_workspace.go. This file holds the daemon entry point and accept loop.

func recoverWorkspaceTxlog(folder string, scan func(string)) {
	scan(folder)
}

// materialisePlumbDir creates <root>/.plumb/ if it does not already exist.
// Called on the AutoAttachPersist path so the next session resolves via the
// normal marker rather than going through synthetic attachment again.
func materialisePlumbDir(root string) error {
	return os.MkdirAll(filepath.Join(root, ".plumb"), 0o755)
}

func postWriteDiagWindow(edits config.EditsConfig) time.Duration {
	if edits.PostWriteDiagnosticsMs == 0 {
		return -1
	}
	return time.Duration(edits.PostWriteDiagnosticsMs) * time.Millisecond
}

func concurrentWriteSkew(edits config.EditsConfig) time.Duration {
	return time.Duration(edits.ConcurrentWriteSkewMs) * time.Millisecond
}

var daemonCmd = &cobra.Command{
	Use:         "daemon",
	Short:       "Run the background daemon (usually started automatically by serve)",
	Hidden:      true,
	RunE:        runDaemon,
	Annotations: map[string]string{annoSkipLogo: "true"}, // background process — no banner
}

// acceptDrainGrace bounds how long the accept loop waits for in-flight
// connections to finish once shutdown begins; the watchdog backstops the rest.
const acceptDrainGrace = 2 * time.Second

// shutdownHardDeadline bounds total graceful shutdown. Once a signal cancels
// the daemon context, armShutdownWatchdog forces process exit after this
// deadline if the orderly teardown has not already returned — so a wedged
// language server or a mid-resync can never leave the daemon hanging and
// holding the lifetime lock.
//
// It is derived from the inner graces so it stays a genuine last resort. The
// orderly path runs the connection drain and the pool.close handshake
// sequentially (acceptDrainGrace + poolCloseGrace) and then the still-unbounded
// topology/supervisor stops. A flat 5s left under 1s of headroom for those
// unbounded steps, so a slow-but-normal shutdown could trip the watchdog and
// truncate a topology resync (WAL-safe, but the resync is lost). The added
// headroom covers the normal unbounded teardown while still bounding a
// genuinely wedged step. Bounding the topology/supervisor stops outright (so
// the orderly path is provably under this deadline) is tracked in
// docs/internal/todo.md.
const shutdownHardDeadline = acceptDrainGrace + poolCloseGrace + 4*time.Second

// armShutdownWatchdog forces process exit shutdownHardDeadline after ctx is
// cancelled — a last-resort backstop behind the bounded graceful teardown. exit
// is injectable for tests; production passes os.Exit. On a clean shutdown
// runDaemon returns and the process exits before the timer fires, so exit is
// never actually called.
func armShutdownWatchdog(ctx context.Context, deadline time.Duration, exit func(int)) {
	go func() {
		<-ctx.Done()
		timer := time.NewTimer(deadline)
		defer timer.Stop()
		<-timer.C
		slog.Error("daemon: shutdown exceeded hard deadline; forcing exit", "deadline", deadline)
		exit(0)
	}()
}

// drainConnections waits up to d for the in-flight connection WaitGroup to
// drain, returning false on timeout. Leaked goroutines are torn down by process
// exit, which the shutdown watchdog guarantees.
func drainConnections(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// workspaceDiagnostics merges diagnostics across every language server bound to
// workspace and formats them for the control-socket dump. A root may host
// several servers (e.g. Go + HTML); each contributes the diagnostics for the
// files it owns.
func workspaceDiagnostics(pool *workspacePool, workspace string) string {
	entries := pool.entriesUnderRoot(workspace)
	if len(entries) == 0 {
		return ""
	}
	merged := entries[0].inv.AllDiagnostics()
	for _, e := range entries[1:] {
		for uri, diags := range e.inv.AllDiagnostics() {
			merged[uri] = diags
		}
	}
	return tools.FormatDiagnostics(merged)
}

func runDaemon(_ *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Acquire the daemon-lifetime lock before doing anything else. If another
	// daemon already holds it, exit immediately rather than stealing the socket
	// path from it (which is what `os.Remove(socketPath); net.Listen(...)`
	// below would otherwise do, leaving two daemons running on the same path).
	lock, err := acquireDaemonLock()
	if err != nil {
		if errors.Is(err, errDaemonAlreadyRunning) {
			slog.Info("daemon: another plumb daemon is already running — exiting")
			return nil
		}
		return err
	}
	defer lock.Close()

	// Last-resort backstop: guarantee the daemon exits within a bounded time of a
	// shutdown signal even if some teardown step wedges, so it releases the
	// lifetime lock and a restart's fresh daemon can bind the socket.
	armShutdownWatchdog(ctx, shutdownHardDeadline, os.Exit)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	configLevel := cfg.LogLevel // saved for "plumb log-level reset"
	if err := setupLogging(configLevel, cfg.LogFormat); err != nil {
		slog.Warn("daemon: invalid log config; keeping defaults", "err", err)
	}

	// Soft heap ceiling: bound a memory spike so it can't exhaust the machine,
	// and surface the active limit in daemon.log.
	applyMemoryLimit(os.Getenv("PLUMB_MEMORY_LIMIT"))

	// Bound a single tree-sitter parse so a GLR-heavy structural file (large
	// Markdown above all) cannot balloon the heap high-water. Must precede the
	// first parse — gotreesitter memoises the env value once.
	applyParseMemoryBudget()

	hasEnabled := false
	for _, lspCfg := range cfg.LSP {
		if lspCfg.Enabled {
			hasEnabled = true
			break
		}
	}
	if !hasEnabled {
		return fmt.Errorf("no language servers enabled; edit ~/.config/plumb/config.toml")
	}

	// store is the daemon-singleton source of truth for the global base config.
	// Subsystems read from it (lock-free) and subscribe for change notifications;
	// the control socket and the global-config file watcher drive store.Reload.
	store := config.NewStore(cfg)

	// Watch the global config file so external edits (vim, etc.) reload live; the
	// TUI and `plumb config reload` push via the control socket instead.
	go func() {
		if err := newGlobalConfigWatcher(store).Run(ctx); err != nil {
			slog.Warn("daemon: global config watcher unavailable", "err", err)
		}
	}()

	socketPath := daemonSocketPath()
	_ = os.Remove(socketPath)
	_ = os.MkdirAll(filepath.Dir(socketPath), 0o700)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("daemon: listening on %s: %w", socketPath, err)
	}
	defer os.Remove(socketPath)
	defer ln.Close()

	pidPath := daemonPIDPath()
	if err := os.WriteFile(pidPath, fmt.Appendf(nil, "%d", os.Getpid()), 0o600); err != nil {
		slog.Warn("daemon: could not write PID file", "path", pidPath, "err", err)
	}
	defer os.Remove(pidPath)

	// Publish our build version next to the PID so `plumb serve` can detect a
	// version mismatch (running daemon older than the binary that's launching).
	versionPath := daemonVersionPath()
	if err := os.WriteFile(versionPath, []byte(Version), 0o600); err != nil {
		slog.Warn("daemon: could not write version file", "path", versionPath, "err", err)
	}
	defer os.Remove(versionPath)

	statsStore := newStatsStore()
	defer statsStore.Close()

	pool := newWorkspacePool(ctx, cfg)
	defer pool.close()

	topoPool := newTopologyPool(cfg.Topology)
	defer topoPool.StopAll()

	memPool := newMemoryIndexPool()
	defer memPool.CloseAll()

	// Daemon-level reconciliation: on every global config change, reconfigure the
	// shared pools that can reload live (topology), and log when a change needs a
	// restart to take effect (LSP servers, cache, log format). Per-connection
	// edits/git/walk reload via each session's own store subscription (conn.go).
	// The subscription lives for the daemon's lifetime, so its unsubscribe is
	// intentionally discarded.
	store.Subscribe(func(c config.Config) {
		topoPool.Reconcile(c.Topology)
		if store.RestartNeeded() {
			slog.Warn("daemon: config change requires a restart to take effect (LSP servers, cache, or log format)")
		}
	})

	// The connection registry is created here (not in the accept loop) so the
	// control socket can reach it for the reload-project command, which targets
	// the per-workspace config reload at the sessions on that workspace.
	registry := newConnRegistry()

	ctrlPath := daemonCtrlSocketPath()
	_ = os.Remove(ctrlPath)
	ctrlLn, ctrlErr := net.Listen("unix", ctrlPath)
	if ctrlErr != nil {
		slog.Warn("daemon: could not start control socket", "path", ctrlPath, "err", ctrlErr)
	} else {
		defer os.Remove(ctrlPath)
		defer ctrlLn.Close()
		go func() {
			<-ctx.Done()
			ctrlLn.Close()
		}()
		diagsFn := func(workspace string) string { return workspaceDiagnostics(pool, workspace) }
		go serveControlSocket(ctrlLn, configLevel, cfg.LogFormat, diagsFn, store.Reload, registry.reloadProject)
	}

	daemonStartedAt := time.Now()
	slog.Info("daemon: ready", "socket", socketPath, "pid", os.Getpid(), "log", daemonLogPath())

	tools.Version = Version

	// Start the background LRU sweep for per-path write locks. Runs for the
	// daemon's lifetime; ctx cancellation stops the sweep goroutine cleanly.
	tools.StartPathLockSweep(ctx)
	monitor.StartSnapshotWriter(ctx, monitor.SnapshotPath(), 2*time.Second, daemonStartedAt)

	// budgets holds one shared, reference-counted RateLimiter per (MCP client
	// identity, workspace) pair (key: ClientName+"/"+ClientVersion+NUL+root).
	// Connections from the same client working the same workspace share this
	// budget so opening multiple connections cannot multiply the allowed write
	// rate; connections on different workspaces never share, so a burst in one
	// project cannot throttle a session in another. Entries are reclaimed once
	// their last session disconnects. See bindWriteLimiterParent and sharedBudgets.
	budgets := newSharedBudgets()

	runDaemonAcceptLoop(ctx, ln, pool, topoPool, memPool, store, statsStore, daemonStartedAt, budgets, registry)
	return nil
}

func runDaemonAcceptLoop(ctx context.Context, ln net.Listener, pool *workspacePool, topoPool *topologyPool, memPool *memoryIndexPool, store *config.Store, statsStore *statsStore, daemonStartedAt time.Time, budgets *sharedBudgets, registry *connRegistry) {
	var wg sync.WaitGroup

	// Idle-session reaper: cancel connections that have not called any tool
	// for longer than the configured eviction TTL.
	go func() {
		ticker := time.NewTicker(reaperInterval)
		defer ticker.Stop()
		runIdleReaper(ctx, store, registry, ticker.C)
	}()

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				if !drainConnections(&wg, acceptDrainGrace) {
					slog.Warn("daemon: in-flight requests did not drain before the shutdown deadline; proceeding")
				}
				return
			default:
				slog.Error("daemon: accept", "err", err)
				continue
			}
		}
		wg.Go(func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("daemon: connection goroutine panic — daemon kept alive",
						"err", r,
						"stack", string(debug.Stack()))
				}
			}()
			handleConn(ctx, conn, pool, topoPool, memPool, store, statsStore, daemonStartedAt, budgets, registry)
		})
	}
}

// serverWriteTimeout is the per-connection response-write deadline. A blocked
// socket write would otherwise hold the connection's write mutex forever and
// wedge every later reply (see docs/internal/todo.md). PLUMB_WRITE_TIMEOUT
// accepts a Go duration; "0"/"off"/"disable" disables the deadline. An unset or
// unparseable value uses mcp's built-in default.
func serverWriteTimeout() time.Duration {
	v := strings.TrimSpace(os.Getenv("PLUMB_WRITE_TIMEOUT"))
	if v == "" {
		return mcp.DefaultWriteTimeout
	}
	switch strings.ToLower(v) {
	case "0", "off", "disable", "disabled", "none":
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return mcp.DefaultWriteTimeout
	}
	return d
}

// handleConn runs a complete MCP session over conn. All per-connection state
// and behaviour live in connSession (see conn.go).
func handleConn(ctx context.Context, conn net.Conn, pool *workspacePool, topoPool *topologyPool, memPool *memoryIndexPool, store *config.Store, statsStore *statsStore, daemonStartedAt time.Time, budgets *sharedBudgets, registry *connRegistry) {
	defer conn.Close()
	s := newConnSession(ctx, pool, topoPool, store, statsStore, budgets)
	s.memoryPool = memPool
	registry.add(s.sessID, connHandle{
		cancel:        s.cancel,
		workspace:     s.workspace,
		reloadProject: func() { s.applyProjectConfig(s.workspace()) },
		summarise:     s.generateEpisodicSummary,
	})
	defer registry.remove(s.sessID)
	defer s.close()
	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: Version})
	srv.WriteTimeout = serverWriteTimeout()
	s.registerAllTools(srv, daemonStartedAt)
	s.registerHooks(srv)
	// Serve on the session context (a child of the daemon ctx) — NOT the bare
	// daemon ctx — so the idle reaper's cancel() makes Serve return and the
	// deferred registry.remove + s.close (which Unregisters) run. A daemon-wide
	// shutdown still cancels it via the parent.
	_ = srv.Serve(s.ctx, conn, conn)
}
