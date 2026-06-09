package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var serveFlagNoReconnect bool

var serveCmd = &cobra.Command{
	Use:         "serve",
	Short:       "Start the MCP server over stdio",
	RunE:        runServe,
	Annotations: map[string]string{annoSkipLogo: "true"}, // stdio MCP wire — no banner
}

func init() {
	serveCmd.Flags().BoolVar(&serveFlagNoReconnect, "no-reconnect", false,
		"disable transparent daemon reconnect; exit on daemon failure (legacy byte-pump proxy)")
}

func runServe(cmd *cobra.Command, _ []string) error {
	parent := cmd.Context()
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	socketPath := daemonSocketPath()
	conn, err := connectOrStartDaemon(ctx, socketPath)
	if err != nil {
		return fmt.Errorf("plumb serve: %w", err)
	}

	if serveFlagNoReconnect || !proxyReconnectEnabled() {
		defer conn.Close()
		return proxyStdio(ctx, conn)
	}

	p := newReconnectingProxy(proxyDeps{
		in:                os.Stdin,
		out:               os.Stdout,
		initial:           conn,
		dial:              func(ctx context.Context) (net.Conn, error) { return connectOrStartDaemon(ctx, socketPath) },
		killDaemon:        killHungDaemon,
		heartbeatInterval: proxyHeartbeatInterval(),
	})
	return p.run(ctx)
}

// proxyReconnectEnabled reports whether the resilient reconnecting proxy is
// active. On by default; PLUMB_PROXY_RECONNECT=0/false/off reverts to the
// legacy byte-pump proxy.
func proxyReconnectEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PLUMB_PROXY_RECONNECT"))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

// proxyHeartbeatInterval is the idle-probe interval for hang detection.
// PLUMB_PROXY_HEARTBEAT accepts a Go duration; "0" disables hang detection
// (crash recovery stays on). An unset or unparseable value uses the default.
func proxyHeartbeatInterval() time.Duration {
	v := strings.TrimSpace(os.Getenv("PLUMB_PROXY_HEARTBEAT"))
	if v == "" {
		return defaultHeartbeatInterval
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return defaultHeartbeatInterval
	}
	return d
}

// connectOrStartDaemon dials the daemon socket. If it is not yet running,
// the daemon subprocess is started and we wait up to 10 seconds for its socket
// to appear before retrying the dial.
//
// Concurrent serves are serialised through plumb.spawn.lock so that only one
// of them ever calls startDaemonProcess. Without that lock, two serves racing
// from a cold start each observe "no daemon" and each spawn one.
func connectOrStartDaemon(ctx context.Context, socketPath string) (net.Conn, error) {
	if conn, err := net.DialTimeout("unix", socketPath, time.Second); err == nil {
		slog.Info("serve: connected to existing daemon")
		warnIfDaemonStale()
		return conn, nil
	}

	spawn, err := acquireSpawnLock(ctx)
	if err != nil {
		return nil, fmt.Errorf("waiting to spawn daemon: %w", err)
	}
	defer spawn.Close()

	// Re-check now that we hold the lock — another serve may have spawned
	// the daemon while we were waiting.
	if conn, err := net.DialTimeout("unix", socketPath, time.Second); err == nil {
		slog.Info("serve: daemon was started by another serve while we waited for the spawn lock")
		warnIfDaemonStale()
		return conn, nil
	}

	slog.Info("serve: daemon not running — starting", "socket", socketPath)
	if err := startDaemonProcess(); err != nil {
		return nil, fmt.Errorf("starting daemon: %w", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if conn, err := net.DialTimeout("unix", socketPath, time.Second); err == nil {
			slog.Info("serve: connected to daemon")
			return conn, nil
		}
	}
	return nil, fmt.Errorf("daemon did not start within 10 seconds (socket: %s)", socketPath)
}

// warnIfDaemonStale compares the running daemon's published version against
// our binary's build version. Mismatch usually means the user rebuilt without
// restarting the daemon, so new tools/features won't be visible. We warn
// rather than fail — the old daemon is still functional for the tools it has.
func warnIfDaemonStale() {
	data, err := os.ReadFile(daemonVersionPath())
	if err != nil {
		return // older daemon predates the version file; can't tell.
	}
	running := string(bytes.TrimSpace(data))
	if running == "" || running == Version {
		return
	}
	fmt.Fprintf(os.Stderr,
		"plumb: warning: connected daemon is %s but this binary is %s — run `plumb stop` to refresh.\n",
		running, Version)
}

// proxyStdio copies stdin → conn and conn → stdout until ctx is cancelled or
// either side closes. This is the only responsibility of the serve proxy.
func proxyStdio(ctx context.Context, conn net.Conn) error {
	errCh := make(chan error, 2)
	go func() { _, err := io.Copy(conn, os.Stdin); errCh <- err }()
	go func() { _, err := io.Copy(os.Stdout, conn); errCh <- err }()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		return err
	}
}
