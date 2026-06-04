package cli

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/spf13/cobra"
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

// serveControlSocket accepts admin connections on ln and handles each in its
// own goroutine. It returns when ln is closed (daemon shutdown).
// diagsFn returns live formatted diagnostics for the given workspace path;
// pass nil if the daemon has no workspace pool (e.g. in tests that don't need it).
func serveControlSocket(ln net.Listener, configLevel, logFormat string, diagsFn func(string) string, reloadFn func() error, reloadProjectFn func(string)) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleCtrlConn(conn, configLevel, logFormat, diagsFn, reloadFn, reloadProjectFn)
	}
}

func handleCtrlConn(conn net.Conn, configLevel, logFormat string, diagsFn func(string) string, reloadFn func() error, reloadProjectFn func(string)) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		return
	}
	line := strings.TrimSpace(scanner.Text())

	if workspace, ok := strings.CutPrefix(line, "diagnostics "); ok {
		if diagsFn != nil {
			fmt.Fprint(conn, diagsFn(workspace))
		}
		return
	}

	if line == "reload-config" {
		handleReloadConfig(conn, reloadFn)
		return
	}

	// reload-project <workspace>: re-apply the per-project config to the sessions
	// pinned to that workspace (and only those), so a workspace settings change
	// made in the TUI takes effect at once for that project.
	if ws, ok := strings.CutPrefix(line, "reload-project "); ok {
		ws = strings.TrimSpace(ws)
		if reloadProjectFn != nil {
			reloadProjectFn(ws)
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
