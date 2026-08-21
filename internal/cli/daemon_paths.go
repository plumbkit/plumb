package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/plumbkit/plumb/internal/paths"
)

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

// plumbRuntimeDir returns the directory used for daemon runtime files (sockets,
// pid, version, locks). The resolution itself lives in internal/paths, which is
// also what the TUI and the two command sandboxes consult — four copies of this
// rule used to exist, and a sandbox whose copy disagreed would stop protecting
// the socket it was written to protect.
// It creates the directory as a side effect, which paths.RuntimeDir
// deliberately does not: the socket bind and the flocks both need it present,
// and every caller here is on that path.
func plumbRuntimeDir() string {
	dir := paths.RuntimeDir()
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

// maxUnixSocketPath is the longest socket path that is portable across the
// platforms plumb supports. sun_path holds 104 bytes on the BSDs and macOS and
// 108 on Linux, and the NUL terminator has to fit too — Go's
// syscall.SockaddrUnix rejects a non-abstract name at n >= len(raw.Path) — so
// the portable ceiling is 103 usable bytes, not 104. The bound is the
// conservative one on purpose: a path that fits on Linux but not macOS is
// still worth naming. The REMEDY differs per platform, though, which is what
// socketPathShortenLever exists to get right.
const maxUnixSocketPath = 103

// socketPathShortenLever names the environment variable that actually moves the
// runtime directory on this host. It delegates rather than deciding, because
// the answer depends on which base RuntimeDir resolved — $XDG_RUNTIME_DIR,
// $HOME on macOS, or XDG_CACHE_HOME — and naming the wrong one points the user
// at a setting that cannot move the socket.
func socketPathShortenLever() string { return paths.RuntimeDirLever() }

// daemonSocketCandidates lists every socket a running daemon might be on, most
// current first.
//
// There is more than one because RuntimeDir is environment-dependent, and the
// environment is not consistent across the ways plumb gets launched:
// $XDG_RUNTIME_DIR is set in a desktop session but absent under cron, a systemd
// system unit, `docker exec`, or ssh without pam_systemd. Resolving only the
// primary path would mean a plumb launched from one of those contexts fails to
// find the daemon a desktop session already started, and spawns a second one —
// two daemons, two sets of language servers, both writing the same stats.db,
// each holding a DIFFERENT plumb.daemon.lock so the flock singleton never
// notices. Dialling both is what keeps them converged on one process.
func daemonSocketCandidates() []string {
	sockets := []string{daemonSocketPath()}
	if legacy := legacyDaemonSocketPath(); legacy != "" {
		sockets = append(sockets, legacy)
	}
	return sockets
}

// legacyDaemonSocketPath is the socket under the pre-$XDG_RUNTIME_DIR runtime
// dir, or "" when that is the same directory in use now.
func legacyDaemonSocketPath() string {
	dir := paths.LegacyRuntimeDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "plumb.sock")
}

// socketPathLengthHint explains an over-long socket path, or returns "" when
// the path is not the problem. It rides every error the user can actually see
// when the daemon fails to come up.
//
// A Unix socket path lives in sun_path, a fixed-size array, so bind() answers
// an over-long path with EINVAL — "invalid argument", which says nothing about
// length. Every layer above then hides it: the daemon writes that error to
// daemon.log and exits, and `plumb serve` reports only "daemon did not start
// within 10 seconds". Reproduced on Linux with a long XDG_CACHE_HOME (the
// runtime dir follows os.UserCacheDir), where the daemon appeared to start and
// silently never came up.
func socketPathLengthHint(path string) string {
	if len(path) <= maxUnixSocketPath {
		return ""
	}
	return fmt.Sprintf(
		" (the path is %d bytes; a Unix socket path must fit in sun_path, at most %d — point %s at a shorter directory)",
		len(path), maxUnixSocketPath, socketPathShortenLever())
}

// daemonStartTimeoutError is what the caller sees when the socket never
// appears: `plumb serve` on a cold start, and `plumb restart` after a respawn.
//
// It is one function so the length hint cannot be attached to some of those
// paths and not others. That is exactly what happened first time round — the
// hint went on the daemon's own listen error, which goes to daemon.log before
// the daemon exits, so the user, who runs `plumb serve` through an MCP client,
// only ever saw the bare timeout.
func daemonStartTimeoutError(action, socketPath string) error {
	return fmt.Errorf("daemon did not %s within 10 seconds (socket: %s)%s", action, socketPath, socketPathLengthHint(socketPath))
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
	// Never let the daemon inherit this serve proxy's working directory. The daemon
	// is a singleton shared across every connection, so an inherited cwd belongs to
	// one arbitrary client's project — and any stray relative path would resolve
	// there. Root has no project to damage. runDaemon chdirs as well, covering a
	// daemon started by any other means.
	cmd.Dir = "/"
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

// daemonLogPath returns the path for daemon log output, under the OS log
// directory resolved by internal/paths:
//   - macOS  : ~/Library/Logs/plumb/daemon.log
//   - Linux  : $XDG_STATE_HOME/plumb/daemon.log  (fallback: ~/.local/state/plumb/daemon.log)
//   - Windows: %LocalAppData%\plumb\daemon.log
func daemonLogPath() string {
	return filepath.Join(paths.LogDir(), "daemon.log")
}
