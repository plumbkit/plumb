package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/plumbkit/plumb/internal/paths"
)

var (
	serveFlagNoReconnect bool
	serveFlagAllowDirs   []string
	serveFlagWorkspace   string
)

var serveCmd = &cobra.Command{
	Use:         "serve",
	Short:       "Start the MCP server over stdio",
	RunE:        runServe,
	Annotations: map[string]string{annoSkipLogo: "true"}, // stdio MCP wire — no banner
}

func init() {
	serveCmd.Flags().BoolVar(&serveFlagNoReconnect, "no-reconnect", false,
		"disable transparent daemon reconnect; exit on daemon failure (legacy byte-pump proxy)")
	serveCmd.Flags().StringArrayVar(&serveFlagAllowDirs, "allow-dir", nil,
		"grant an extra read-write root to this connection (repeatable; also PLUMB_ALLOWED_DIRS, os-list-separated). Additive to the pinned workspace and config extra_roots; inert until a workspace is pinned — never a workspace source on an unattached serve.")
	serveCmd.Flags().StringVar(&serveFlagWorkspace, "workspace", "",
		"pre-pin this connection's workspace to a path (also PLUMB_WORKSPACE) for clients that report no roots. Without it serve starts unattached and session_start pins the workspace; never overrides an explicit session_start pin.")
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

	allowDirs := resolveAllowDirs(serveFlagAllowDirs, os.Getenv("PLUMB_ALLOWED_DIRS"))

	// The explicit workspace pre-pin transported in the initialize frame's
	// _meta: --workspace wins over PLUMB_WORKSPACE, and there is deliberately NO
	// serve-cwd fallback — cwd is not intent (an MCP client spawns serve from its
	// own launcher's directory, which says nothing about the project), so a serve
	// started with neither starts UNATTACHED and session_start({workspace}) is
	// the sole workspace-pin authority. The cwd is still captured for
	// diagnostics in the unattached log below, never for an attach.
	cwd, _ := os.Getwd()
	workspace := resolveWorkspaceHint(serveFlagWorkspace, os.Getenv("PLUMB_WORKSPACE"))

	if serveFlagNoReconnect || !proxyReconnectEnabled() {
		defer conn.Close()
		if len(allowDirs) > 0 {
			slog.Warn("serve: --allow-dir is ignored with the legacy byte-pump proxy; it requires the resilient proxy (the default)")
		}
		if workspace != "" {
			slog.Warn("serve: --workspace is ignored with the legacy byte-pump proxy; it requires the resilient proxy (the default)")
		}
		return proxyStdio(ctx, conn)
	}

	// Name the unattached state at startup so a later "no workspace yet" reads
	// as intent, not a bug. Only the resilient proxy ever transported the hint,
	// so only here can the absence of one be new information.
	if workspace == "" {
		slog.Info("serve: no --workspace/PLUMB_WORKSPACE — starting unattached; the caller pins the workspace with session_start", "cwd", cwd)
	}

	p := newReconnectingProxy(proxyDeps{
		in:                os.Stdin,
		out:               os.Stdout,
		initial:           conn,
		dial:              func(ctx context.Context) (net.Conn, error) { return connectOrStartDaemon(ctx, socketPath) },
		killDaemon:        killHungDaemon,
		heartbeatInterval: proxyHeartbeatInterval(),
		allowDirs:         allowDirs,
		proxySessionID:    newProxySessionID(),
		workspace:         workspace,
	})
	return p.run(ctx)
}

// resolveAllowDirs normalises the client-granted extra read-write roots from the
// --allow-dir flags and the PLUMB_ALLOWED_DIRS env var (os-list-separated, e.g.
// ':' on Unix). Each entry is $VAR-expanded in the serve process's environment
// and made absolute, so the daemon — a separate, possibly differently-rooted
// process — receives canonical-ready paths. Blank entries are dropped and order
// is preserved (flags first, then env). An empty result transports nothing, so
// behaviour is identical to today.
func resolveAllowDirs(flags []string, env string) []string {
	raw := append([]string(nil), flags...)
	if env != "" {
		raw = append(raw, filepath.SplitList(env)...)
	}
	out := make([]string, 0, len(raw))
	for _, d := range raw {
		d = strings.TrimSpace(os.ExpandEnv(d))
		if d == "" {
			continue
		}
		if abs, err := filepath.Abs(d); err == nil {
			d = abs
		}
		out = append(out, d)
	}
	return out
}

// resolveWorkspaceHint picks the explicit workspace pre-pin the proxy
// transports in the initialize frame's _meta: the --workspace flag wins over
// PLUMB_WORKSPACE. There is deliberately NO serve-cwd fallback — the proxy's
// working directory is whichever directory the MCP client's launcher happened
// to use, which is not a declaration of intent — so with neither flag nor env
// the serve starts unattached and session_start({workspace}) pins the
// workspace. Like resolveAllowDirs, an explicit value is $VAR-expanded in the
// serve process's environment and made absolute, so the daemon — a separate,
// possibly differently-rooted process — receives a canonical-ready path; blank
// values are treated as unset. Symlink canonicalisation is deliberately left to
// the daemon's pool.Detect, which already resolves the hint exactly as it
// resolves any other candidate root — canonicalising here as well could diverge
// from what Detect actually attaches.
func resolveWorkspaceHint(flag, env string) string {
	v := strings.TrimSpace(os.ExpandEnv(flag))
	if v == "" {
		v = strings.TrimSpace(os.ExpandEnv(env))
	}
	if v == "" {
		return ""
	}
	if abs, err := filepath.Abs(v); err == nil {
		v = abs
	}
	return v
}

// newProxySessionID returns a fresh, process-stable proxy session ID (a random
// UUIDv4). Generated once per `plumb serve` and injected into the captured
// initialize frame's _meta, identical across every handshake replay so the
// daemon can correlate a reconnected connection after a restart. A crypto/rand
// failure (vanishingly rare) yields "" — the daemon then treats the connection
// as fresh, which is the safe fallback.
func newProxySessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
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

	warnIfDaemonAtLegacyPath()

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
	return nil, daemonStartTimeoutError("start", socketPath)
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

// warnIfDaemonAtLegacyPath reports a daemon listening at the cache-dir runtime
// location when this process resolves $XDG_RUNTIME_DIR, right before we start a
// second one beside it.
//
// It states the fact and stops there. An earlier version called it "a daemon
// from an older version", which it cannot know — it never reads the version
// file, and a CURRENT build lands at the cache path whenever $XDG_RUNTIME_DIR
// is absent from the launching environment (cron, a systemd system unit,
// docker exec, ssh without pam_systemd). Version skew is warnIfDaemonStale's
// job, which reads the version file to decide.
//
// The warning is the whole remedy on purpose. Attaching to that daemon instead
// was tried and reverted: the runtime directory determines the socket, the
// control socket, the pid and the version file together, so a process that
// connected to one directory's socket while resolving every other path in the
// other got a half-migrated state — `plumb web` and `plumb log-level` dialling
// a control socket that was not there, doctor reporting a version file as
// missing when it existed one directory over, and `plumb restart` spawning the
// duplicate this was supposed to prevent. One directory per process, and a
// warning when the user has two.
func warnIfDaemonAtLegacyPath() {
	legacy := legacyDaemonSocketPath()
	if legacy == "" {
		return // the runtime dir does not move on this platform
	}
	conn, err := net.DialTimeout("unix", legacy, time.Second)
	if err != nil {
		return
	}
	_ = conn.Close()
	fmt.Fprintf(os.Stderr,
		"plumb: warning: a daemon is already running at %s, but this plumb uses %s.\n"+
			"plumb: that happens after an upgrade, or when plumb is launched somewhere\n"+
			"plumb: $XDG_RUNTIME_DIR is not set (cron, a systemd unit, docker exec, ssh).\n"+
			"plumb: starting a second daemon; run `plumb stop` to consolidate on one.\n",
		legacy, paths.RuntimeDir())
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
