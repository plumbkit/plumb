package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/mcp"
	"github.com/golimpio/plumb/internal/monitor"
	"github.com/golimpio/plumb/internal/tools"
)

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
	Use:    "daemon",
	Short:  "Run the background daemon (usually started automatically by serve)",
	Hidden: true,
	RunE:   runDaemon,
}

// shutdownHardDeadline bounds total graceful shutdown. Once a signal cancels
// the daemon context, armShutdownWatchdog forces process exit after this
// deadline if the orderly teardown (bounded pool.close, connection drain,
// topology stop) has not already returned — so a wedged language server or a
// mid-resync can never leave the daemon hanging and holding the lifetime lock.
const shutdownHardDeadline = 5 * time.Second

// acceptDrainGrace bounds how long the accept loop waits for in-flight
// connections to finish once shutdown begins; the watchdog backstops the rest.
const acceptDrainGrace = 2 * time.Second

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

	pool := newWorkspacePool(cfg)
	defer pool.close()

	topoPool := newTopologyPool(cfg.Topology)
	defer topoPool.StopAll()

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
		diagsFn := func(workspace string) string {
			e := pool.lookup(workspace)
			if e == nil {
				return ""
			}
			return tools.FormatDiagnostics(e.inv.AllDiagnostics())
		}
		go serveControlSocket(ctrlLn, configLevel, cfg.LogFormat, diagsFn, store.Reload)
	}

	daemonStartedAt := time.Now()
	slog.Info("daemon: ready", "socket", socketPath, "pid", os.Getpid(), "log", daemonLogPath())

	tools.Version = Version

	// Start the background LRU sweep for per-path write locks. Runs for the
	// daemon's lifetime; ctx cancellation stops the sweep goroutine cleanly.
	tools.StartPathLockSweep(ctx)
	monitor.StartSnapshotWriter(ctx, monitor.SnapshotPath(), 2*time.Second, daemonStartedAt)

	// clientLimiters holds one RateLimiter per MCP client identity
	// (ClientName+"/"+ClientVersion). Connections from the same client share
	// this budget so opening multiple connections cannot multiply the allowed
	// write rate.
	var clientLimiters sync.Map // map[string]*tools.RateLimiter

	runDaemonAcceptLoop(ctx, ln, pool, topoPool, store, statsStore, daemonStartedAt, &clientLimiters)
	return nil
}

func runDaemonAcceptLoop(ctx context.Context, ln net.Listener, pool *workspacePool, topoPool *topologyPool, store *config.Store, statsStore *statsStore, daemonStartedAt time.Time, clientLimiters *sync.Map) {
	var wg sync.WaitGroup
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
			handleConn(ctx, conn, pool, topoPool, store, statsStore, daemonStartedAt, clientLimiters)
		})
	}
}

// handleConn runs a complete MCP session over conn. All per-connection state
// and behaviour live in connSession (see conn.go).
func handleConn(ctx context.Context, conn net.Conn, pool *workspacePool, topoPool *topologyPool, store *config.Store, statsStore *statsStore, daemonStartedAt time.Time, clientLimiters *sync.Map) {
	defer conn.Close()
	s := newConnSession(pool, topoPool, store, statsStore, clientLimiters)
	defer s.close()
	srv := mcp.New(mcp.ServerInfo{Name: "plumb", Version: Version})
	s.registerAllTools(srv, daemonStartedAt)
	s.registerHooks(srv)
	_ = srv.Serve(ctx, conn, conn)
}

// daemonSocketPath returns the Unix socket path for the plumb daemon.
func daemonSocketPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.sock")
}

// daemonCtrlSocketPath returns the Unix socket path for daemon admin commands
// (log level changes). Separate from the MCP socket so it never appears in the
// tool list and cannot be reached by MCP clients.
func daemonCtrlSocketPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.ctrl.sock")
}

// daemonPIDPath returns the path where the daemon writes its PID.
func daemonPIDPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.pid")
}

// daemonVersionPath returns the path where the daemon publishes its build
// version (read by `plumb serve` to detect a stale daemon).
func daemonVersionPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.version")
}

// plumbRuntimeDir returns the directory used for daemon runtime files
// (socket, PID). It uses os.UserCacheDir so the path is stable and consistent
// regardless of how the process was launched — critical on macOS where
// os.TempDir() follows $TMPDIR, which differs between GUI apps and terminals.
func plumbRuntimeDir() string {
	base, err := os.UserCacheDir()
	if err != nil {
		// os.UserCacheDir only fails if $HOME is unset; fall back to os.TempDir
		// which is the best we can do in that degenerate case.
		base = os.TempDir()
	}
	dir := filepath.Join(base, "plumb")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// startDaemonProcess launches a detached plumb daemon subprocess.
// Logs are written to daemonLogPath(); the process is detached with Setsid so
// it outlives the calling plumb serve process.
func startDaemonProcess() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	logPath := daemonLogPath()
	_ = os.MkdirAll(filepath.Dir(logPath), 0o755)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logFile, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}

	cmd := exec.Command(exe, "daemon")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	go reapAfterExit(cmd, logFile)
	return nil
}

// reapAfterExit drops the spawner's copy of the log handle (the child dup'd its
// own fd at exec) and waits on cmd so the daemon is reaped on exit instead of
// lingering as a zombie. Setsid detaches the controlling terminal but does NOT
// reparent the child — it stays a child of the long-lived `plumb serve` that
// spawned it, so without this Wait a `plumb restart` SIGTERM leaves a <defunct>
// process (and `stopByPID`'s kill-0 liveness check then misreports it as still
// running). Runs in its own goroutine in production; the short-lived restart/stop
// callers exit before the daemon dies, so the child reparents to init and is
// reaped there.
func reapAfterExit(cmd *exec.Cmd, logFile *os.File) {
	_ = logFile.Close()
	_ = cmd.Wait()
}

// daemonLogPath returns the OS-appropriate path for daemon log output.
//   - macOS : ~/Library/Logs/plumb/daemon.log
//   - Linux : $XDG_STATE_HOME/plumb/daemon.log  (fallback: ~/.local/state/plumb/daemon.log)
//   - other : $TMPDIR/plumb/daemon.log
func daemonLogPath() string {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			break
		}
		return filepath.Join(home, "Library", "Logs", "plumb", "daemon.log")
	case "linux":
		if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
			return filepath.Join(xdg, "plumb", "daemon.log")
		}
		home, err := os.UserHomeDir()
		if err != nil {
			break
		}
		return filepath.Join(home, ".local", "state", "plumb", "daemon.log")
	}
	return filepath.Join(os.TempDir(), "plumb", "daemon.log")
}

// rootFromRoots calls roots/list on the MCP client and returns the first root
// URI, or "" if the client does not support roots/list or returns no roots.
func rootFromRoots(ctx context.Context, request mcp.RequestFn) string {
	raw, err := request(ctx, "roots/list", nil)
	if err != nil {
		slog.Info("roots/list not supported by client — deferring to OnBeforeTool", "err", err)
		return ""
	}

	var resp struct {
		Roots []struct {
			URI string `json:"uri"`
		} `json:"roots"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		slog.Warn("parsing roots/list response", "err", err)
		return ""
	}
	if len(resp.Roots) == 0 {
		slog.Info("roots/list returned no roots — deferring to OnBeforeTool")
		return ""
	}

	root := resp.Roots[0].URI
	slog.Info("workspace root from MCP client", "rootURI", root)
	return root
}

// workspaceFromArgs returns the resolved workspace root for a tool call's raw
// JSON arguments. Returns "" if no path-bearing field is present or the path
// doesn't sit under a discoverable project root.
func workspaceFromArgs(pool *workspacePool, args json.RawMessage) string {
	seed := seedPathFromArgs(args)
	if seed == "" {
		return ""
	}
	// If seed is already a directory, use it directly — filepath.Dir would
	// strip the last component and miss the project root marker.
	startDir := seed
	if info, err := os.Stat(seed); err != nil || !info.IsDir() {
		startDir = filepath.Dir(seed)
	}
	root, _, err := pool.Detect(startDir)
	if err != nil {
		return ""
	}
	return root
}

// seedPathFromArgs extracts a single filesystem path from a tool call's raw
// JSON arguments. Probes the argument shapes plumb's tools use:
//
//	{"uri": "file:///..."}                      — LSP tools
//	{"file_path": "/..."}                       — file-content tools (read/write/edit/delete)
//	{"path": "/..."}                            — search/dir tools (list_directory, find_files, …)
//	{"root": "/..."}                            — list_files
//	{"workspace": "/..."}                       — session_start
//	{"paths": ["/...", ...]}                    — read_multiple_files
//	{"operations": [{"path": "/..."}, ...]}     — transaction_apply
//
// Returns "" if no shape matches. Any leading file:// is stripped so the
// caller gets a plain filesystem path.
func seedPathFromArgs(args json.RawMessage) string {
	var a struct {
		URI        string   `json:"uri"`
		FilePath   string   `json:"file_path"`
		Path       string   `json:"path"`
		Root       string   `json:"root"`
		Workspace  string   `json:"workspace"`
		Paths      []string `json:"paths"`
		Operations []struct {
			FilePath string `json:"file_path"`
			Path     string `json:"path"`
		} `json:"operations"`
	}
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	switch {
	case a.URI != "":
		return strings.TrimPrefix(a.URI, "file://")
	case a.FilePath != "":
		return a.FilePath
	case a.Path != "":
		return a.Path
	case a.Root != "":
		return a.Root
	case a.Workspace != "":
		return a.Workspace
	case len(a.Paths) > 0:
		return a.Paths[0]
	case len(a.Operations) > 0:
		if a.Operations[0].FilePath != "" {
			return a.Operations[0].FilePath
		}
		return a.Operations[0].Path
	}
	return ""
}
