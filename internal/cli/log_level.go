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

	conn, err := net.Dial("unix", daemonCtrlSocketPath())
	if err != nil {
		return fmt.Errorf("daemon control socket unavailable — is plumb daemon running?\n  start it with: plumb serve\n  (%w)", err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintf(conn, "set-level %s\n", level); err != nil {
		return fmt.Errorf("sending command: %w", err)
	}

	resp, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	resp = strings.TrimRight(resp, "\n")
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
func serveControlSocket(ln net.Listener, configLevel, logFormat string, diagsFn func(string) string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go handleCtrlConn(conn, configLevel, logFormat, diagsFn)
	}
}

func handleCtrlConn(conn net.Conn, configLevel, logFormat string, diagsFn func(string) string) {
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
