package cli

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/monitor"
)

var logLevelCmd = &cobra.Command{
	Use:   "log-level <level>",
	Short: "Change the running daemon's log level (debug, info, warn, error, reset)",
	Long: `Temporarily change the log level of the running plumb daemon.

  plumb log-level debug   — enable verbose logging
  plumb log-level info    — standard logging (default)
  plumb log-level warn    — warnings and errors only
  plumb log-level error   — errors only
  plumb log-level reset   — restore the daemon's startup config level

The change is daemon-lifetime only. "reset" restores the level captured when the
daemon started, including any PLUMB_LOG_LEVEL override active at startup. To make
it permanent, set log_level in ~/.config/plumb/config.toml.`,
	Args: cobra.ExactArgs(1),
	RunE: runLogLevel,
}

func runLogLevel(_ *cobra.Command, args []string) error {
	level := args[0]
	if !validLogLevelCommand(level) {
		return fmt.Errorf("invalid log level %q; expected debug, info, warn, error, or reset", level)
	}

	resp, err := dialDaemonCtrl("set-level " + level)
	if err != nil {
		return err
	}
	if msg, ok := strings.CutPrefix(resp, "error:"); ok {
		return fmt.Errorf("%s", strings.TrimSpace(msg))
	}

	if level == "reset" {
		fmt.Println("log level reset to daemon startup config level")
	} else {
		fmt.Printf("log level set to %s\n", level)
	}
	return nil
}

// dialDaemonCtrl dials the daemon control socket, sends a single-line command,
// and returns the trimmed first response line. Shared by `plumb log-level` and
// `plumb config reload`.
func dialDaemonCtrl(command string) (string, error) {
	conn, err := net.Dial("unix", daemonCtrlSocketPath())
	if err != nil {
		return "", fmt.Errorf("daemon control socket unavailable — is plumb daemon running?\n  start it with: plumb serve\n  (%w)", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", command); err != nil {
		return "", fmt.Errorf("sending command: %w", err)
	}
	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}
	return strings.TrimRight(resp, "\n"), nil
}

func validLogLevelCommand(level string) bool {
	switch level {
	case "debug", "info", "warn", "error", "reset":
		return true
	default:
		return false
	}
}

// ctrlHandlers bundles the optional daemon-side callbacks the control socket
// dispatches to. Any field may be nil — tests pass a zero value and the handler
// treats a nil callback as "feature unavailable". Collapsing these into a struct
// (rather than positional params) keeps adding a new admin command cheap.
type ctrlHandlers struct {
	diags         func(string) string // diagnostics <workspace>
	reload        func() error        // reload-config
	reloadProject func(string)        // reload-project <workspace>
	lspStatus     func() string       // lsp-status
}

// serveControlSocket accepts admin connections on ln and handles each in its
// own goroutine. It returns when ln is closed (daemon shutdown). All callbacks
// in h are optional; pass a zero ctrlHandlers when the daemon has no workspace
// pool (e.g. in tests that don't need it).
func serveControlSocket(ln net.Listener, configLevel, logFormat string, h ctrlHandlers) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleCtrlConn(conn, configLevel, logFormat, h)
	}
}

func handleCtrlConn(conn net.Conn, configLevel, logFormat string, h ctrlHandlers) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	line := strings.TrimSpace(scanner.Text())

	if workspace, ok := strings.CutPrefix(line, "diagnostics "); ok {
		if h.diags != nil {
			fmt.Fprint(conn, h.diags(workspace))
		}
		return
	}

	if line == "reload-config" {
		handleReloadConfig(conn, h.reload)
		return
	}

	if line == "heap-profile" {
		handleHeapProfile(conn)
		return
	}

	if line == "mem-stats" {
		handleMemStats(conn)
		return
	}

	if line == "lsp-status" {
		if h.lspStatus != nil {
			fmt.Fprint(conn, h.lspStatus())
		}
		return
	}

	if line == "goroutine-stacks" {
		handleStacksProfile(conn)
		return
	}

	// reload-project <workspace>: re-apply the per-project config to the sessions
	// pinned to that workspace (and only those), so a workspace settings change
	// made in the TUI takes effect at once for that project.
	if ws, ok := strings.CutPrefix(line, "reload-project "); ok {
		ws = strings.TrimSpace(ws)
		if h.reloadProject != nil {
			h.reloadProject(ws)
		}
		slog.Info("daemon: project config reloaded via control socket", "workspace", ws)
		fmt.Fprint(conn, "ok\n")
		return
	}

	const prefix = "set-level "
	if !strings.HasPrefix(line, prefix) {
		fmt.Fprintf(conn, "error: unknown command %q\n", line)
		return
	}

	level := strings.TrimPrefix(line, prefix)
	if level == "reset" {
		level = configLevel
	}

	if err := setupLogging(level, logFormat); err != nil {
		fmt.Fprintf(conn, "error: %s\n", err.Error())
		return
	}

	slog.Info("daemon: log level changed via control socket", "level", level)
	fmt.Fprintf(conn, "ok\n")
}

// handleHeapProfile writes a heap pprof snapshot to the cache dir and replies
// with its absolute path, in response to the control-socket "heap-profile"
// command (sent by `plumb debug heap`). A forced GC runs first so the profile
// reflects live, post-collection memory rather than uncollected garbage. Open
// the result with `go tool pprof <path>`.
func handleHeapProfile(conn net.Conn) {
	runtime.GC()
	dir := config.CacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(conn, "error: creating cache dir: %s\n", err.Error())
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("plumb.heap.%d.pprof", time.Now().UnixNano()))
	f, err := os.Create(path) //nolint:gosec // G304: path is cache dir + a fixed-format name, no user input
	if err != nil {
		fmt.Fprintf(conn, "error: creating heap profile: %s\n", err.Error())
		return
	}
	defer f.Close()
	if err := pprof.WriteHeapProfile(f); err != nil {
		fmt.Fprintf(conn, "error: writing heap profile: %s\n", err.Error())
		return
	}
	slog.Info("daemon: heap profile written via control socket", "path", path)
	fmt.Fprintf(conn, "%s\n", path)
}

// handleStacksProfile writes a full goroutine stack dump (the pprof "goroutine"
// profile at debug=2 — human-readable stacks for every goroutine, the
// non-destructive equivalent of SIGQUIT) to the cache dir and replies with its
// path, in response to the control-socket "goroutine-stacks" command (sent by
// `plumb debug stacks`). Capturing it *during* a hang shows what each goroutine
// is blocked on — a held mutex, a stalled socket write, a lock wait.
func handleStacksProfile(conn net.Conn) {
	dir := config.CacheDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(conn, "error: creating cache dir: %s\n", err.Error())
		return
	}
	path := filepath.Join(dir, fmt.Sprintf("plumb.stacks.%d.txt", time.Now().UnixNano()))
	f, err := os.Create(path) //nolint:gosec // G304: path is cache dir + a fixed-format name, no user input
	if err != nil {
		fmt.Fprintf(conn, "error: creating stacks dump: %s\n", err.Error())
		return
	}
	defer f.Close()
	if err := pprof.Lookup("goroutine").WriteTo(f, 2); err != nil {
		fmt.Fprintf(conn, "error: writing stacks dump: %s\n", err.Error())
		return
	}
	slog.Info("daemon: goroutine stacks written via control socket", "path", path, "goroutines", runtime.NumGoroutine())
	fmt.Fprintf(conn, "%s\n", path)
}

// handleMemStats replies with a formatted runtime memory snapshot, in response
// to the control-socket "mem-stats" command (sent by `plumb debug mem`).
func handleMemStats(conn net.Conn) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	// Tab-separated label/value pairs; the CLI (`plumb debug mem`) renders the
	// aligned dotted-leader rows so presentation stays out of the daemon.
	fmt.Fprintf(conn,
		"HeapAlloc\t%s\nHeapInuse\t%s\nHeapSys\t%s\nHeapIdle\t%s\nHeapReleased\t%s\nNextGC\t%s\nNumGC\t%d\nGoroutines\t%d\n",
		monitor.FormatBytes(m.HeapAlloc),
		monitor.FormatBytes(m.HeapInuse),
		monitor.FormatBytes(m.HeapSys),
		monitor.FormatBytes(m.HeapIdle),
		monitor.FormatBytes(m.HeapReleased),
		monitor.FormatBytes(m.NextGC),
		m.NumGC,
		runtime.NumGoroutine(),
	)
}

// handleReloadConfig re-reads the global config in response to a control-socket
// "reload-config" command — sent by the TUI after it saves a setting, or by
// `plumb config reload`. A reload error is reported back to the caller; a nil
// reloadFn is treated as a no-op success (used by tests with no store).
func handleReloadConfig(conn net.Conn, reloadFn func() error) {
	if reloadFn != nil {
		if err := reloadFn(); err != nil {
			fmt.Fprintf(conn, "error: %s\n", err.Error())
			return
		}
	}
	slog.Info("daemon: config reloaded via control socket")
	fmt.Fprint(conn, "ok\n")
}
