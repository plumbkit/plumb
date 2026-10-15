package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var (
	serveFlagNoReconnect bool
	serveFlagAllowDirs   []string
	serveFlagWorkspace   string
	serveFlagNamespace   string
	serveFlagAttachOnly  bool
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
	serveCmd.Flags().BoolVar(&serveFlagAttachOnly, "attach-only", false,
		"dial the running daemon and never start one; fail if none is running (for scripts and harnesses)")
	serveCmd.Flags().StringVar(&serveFlagNamespace, "namespace", "",
		"join the namespace this token names (also PLUMB_NAMESPACE): an isolated slice of the daemon for an end-to-end run; see `plumb namespace create`. A refused token ends the serve — it never falls back to the default namespace.")
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
	logClientEnvDivergence()
	conn, err := dialOrAttach(ctx, socketPath, serveFlagAttachOnly)
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

	namespace := serveFlagNamespace
	if namespace == "" {
		namespace = os.Getenv("PLUMB_NAMESPACE")
	}

	if serveFlagNoReconnect || !proxyReconnectEnabled() {
		defer conn.Close()
		if namespace != "" {
			// The legacy byte pump cannot carry the token, and joining the default
			// namespace instead would be exactly the fallback a namespace forbids.
			return errors.New("plumb serve: --namespace needs the resilient proxy; drop --no-reconnect")
		}
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

	deps := serveProxyDeps(socketPath, serveFlagAttachOnly, allowDirs, workspace, conn)
	deps.namespaceToken = namespace
	p := newReconnectingProxy(deps)
	return p.run(ctx)
}

// serveProxyDeps builds the resilient proxy's dependencies for this serve.
//
// It is a named constructor because of ONE field: the redial closure. A proxy
// that reconnects is the normal case (a replaced daemon, a dropped socket), so
// `--attach-only` has to hold on the redial exactly as it does on the first dial
// — otherwise a script that forbade spawning gets a daemon started for it the
// moment the first connection drops. That is a wiring fact, invisible to a
// reader of the struct literal and untestable through runServe's stdio, so it
// lives here and is pinned by a test that drives this very closure.
func serveProxyDeps(socketPath string, attachOnly bool, allowDirs []string, workspace string, initial net.Conn) proxyDeps {
	return proxyDeps{
		in:                os.Stdin,
		out:               os.Stdout,
		initial:           initial,
		dial:              func(ctx context.Context) (net.Conn, error) { return dialOrAttach(ctx, socketPath, attachOnly) },
		killDaemon:        killHungDaemon,
		heartbeatInterval: proxyHeartbeatInterval(),
		allowDirs:         allowDirs,
		proxySessionID:    newProxySessionID(),
		workspace:         workspace,
		resumeStore:       openDefaultResumeStore(),
		resumeProofKey:    loadResumeProofKey,
	}
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

// dialOrAttach dials the daemon for a serve client: connectOrStartDaemon
// normally, or attachOnly under --attach-only, which never starts one. It is one
// function so the RESILIENT PROXY's redial takes the same path as the first
// dial: a proxy that reconnects after --attach-only must not quietly start a
// daemon the flag forbade.
func dialOrAttach(ctx context.Context, socketPath string, attachOnlyOnly bool) (net.Conn, error) {
	if attachOnlyOnly {
		return attachOnly(ctx, socketPath)
	}
	return connectOrStartDaemon(ctx, socketPath)
}

// connectOrStartDaemon dials the daemon socket. If no daemon accepts, it starts
// one, but only while holding the launch locks (daemon_launch.go): while another
// daemon holds one, running or still starting, it only keeps dialling. It waits
// up to daemonStartWait (30 s by default) in all.
//
// A daemon an OLDER binary left running at a legacy runtime location is answered
// FIRST, before any launch is considered (PLAN-530): one of the same build is
// reused, and one of a different build refuses the launch with an instruction
// rather than being migrated silently or spawned beside. Only when there is no
// legacy daemon does this start one — and then under a fence, so an old launcher
// cannot start a successor in the meantime.
//
// Concurrent serves are also serialised through plumb.spawn.lock, so only one
// of them at a time makes the dial-or-launch decision.
func connectOrStartDaemon(ctx context.Context, socketPath string) (net.Conn, error) {
	if conn, err := net.DialTimeout("unix", socketPath, time.Second); err == nil {
		slog.Info("serve: connected to existing daemon")
		warnIfDaemonStale(daemonVersionPath())
		return conn, nil
	}

	// No daemon of this process's own runtime directory. An older binary's may
	// still be serving somewhere else, so ask before deciding to launch: one that
	// cannot be reached is invisible to the dial above, and starting a second one
	// beside it is the defect this whole plan exists to remove.
	if conn, err := attachLegacyDaemon(); conn != nil || err != nil {
		return conn, err
	}

	spawn, err := acquireSpawnLock(ctx)
	if err != nil {
		return nil, fmt.Errorf("waiting to spawn daemon: %w", err)
	}
	// Released as soon as the launch decision is made: the daemon lock, not
	// this one, keeps a second daemon out, so others need not queue behind
	// this serve's wait.
	release := sync.OnceFunc(func() { _ = spawn.Close() })
	defer release()

	l := newDaemonLauncher(socketPath)
	start := l.start
	l.start = func(locks launchLocks) error {
		slog.Info("serve: daemon not running — starting", "socket", socketPath)
		return start(locks)
	}
	l.decided = release
	l.onHeld = func() {
		slog.Info("serve: a plumb daemon holds the launch lock but is not accepting yet; waiting", "wait", l.wait)
	}
	conn, launched, err := l.dialOrLaunch(ctx)
	if err != nil {
		return nil, launchError("start", socketPath, l.wait, err)
	}
	if launched {
		slog.Info("serve: connected to daemon")
	} else {
		slog.Info("serve: connected to a daemon another process started")
		warnIfDaemonStale(daemonVersionPath())
	}
	return conn, nil
}

// warnIfDaemonStale compares a running daemon's published version against our
// binary's build version. Mismatch usually means the user rebuilt without
// restarting the daemon, so new tools/features won't be visible. We warn rather
// than fail — the old daemon is still functional for the tools it has.
//
// versionPath is the daemon's OWN version file, resolved from the runtime
// location the connection was made to — never this process's. Reading the
// version file of a directory one over is exactly the half-migration this and
// legacy.go exist to prevent: it reports a mismatch that is not there, or misses
// one that is.
func warnIfDaemonStale(versionPath string) {
	data, err := os.ReadFile(versionPath) //nolint:gosec // G304: a runtime directory plumb resolved, or the one a legacy daemon published in
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
