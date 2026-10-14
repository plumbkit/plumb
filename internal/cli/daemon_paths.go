package cli

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/paths"
)

// The daemon's runtime paths are resolved as a SET — the MCP socket, the control
// socket, the pid file, the version file and the lifetime lock all name the one
// runtime directory this process uses (currentLocation, legacy.go). Each accessor
// below is a field of that set rather than its own join, which is what makes it
// impossible for two of them to end up in different directories: that is the
// half-migration serve.go records, where a process connected to one directory's
// socket while resolving the rest in another.

// daemonSocketPath returns the Unix socket path for the plumb daemon.
func daemonSocketPath() string {
	return currentLocation().Socket
}

// daemonCtrlSocketPath returns the Unix socket path for daemon admin commands
// (log level changes). Separate from the MCP socket so it never appears in the
// tool list and cannot be reached by MCP clients.
func daemonCtrlSocketPath() string {
	return currentLocation().Ctrl
}

// daemonPIDPath returns the path where the daemon writes its PID.
func daemonPIDPath() string {
	return currentLocation().PID
}

// publishDaemonPID writes this process's PID file, first clearing a stale control
// socket. The identity hook names a daemon instance by the PID file plus the
// control socket's inode and mtime. After a kill -9 both survive, and a
// successor that reuses the PID would show the dead daemon's marker until it
// re-binds the socket, so the hook would serve the dead daemon's cached answer.
// With the socket gone first, that window reads as an empty marker, which never
// matches a cache.
//
// Only a STALE socket is cleared (PLAN-529): one something is still listening on
// is somebody else's, and unlinking it is exactly the file-a-live-process-is-
// using defect this phase removes. The bind below refuses a live occupant too, so
// the two agree about what may be displaced. writeFile is os.WriteFile; it is a
// parameter so a test can see the socket's state at the instant the PID file
// appears.
func publishDaemonPID(writeFile func(name string, data []byte, perm os.FileMode) error) error {
	ctrlPath := daemonCtrlSocketPath()
	if err := clearStaleSocket(ctrlPath); err != nil {
		slog.Warn("daemon: leaving the control socket path alone", "path", ctrlPath, "err", err)
	}
	return writeFile(daemonPIDPath(), fmt.Appendf(nil, "%d", os.Getpid()), 0o600)
}

// daemonVersionPath returns the path where the daemon publishes its build
// version (read by `plumb serve` to detect a stale daemon).
func daemonVersionPath() string {
	return currentLocation().Version
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

// socketPathShortenRemedy says what would actually move the runtime directory on
// this process.
//
// Legacy mode names the environment variable, because there the answer depends
// on which base RuntimeDir resolved — $XDG_RUNTIME_DIR, $HOME on macOS, or
// XDG_CACHE_HOME — and naming the wrong one points the user at a setting that
// cannot move the socket.
//
// With the account authority enabled, NO environment variable can move it: the
// runtime directory comes from the service profile (or the account database), so
// telling the user to point $HOME somewhere shorter would send them to a setting
// that provably changes nothing. The remedy is the profile.
func socketPathShortenRemedy() string {
	if paths.AuthorityEnabled() {
		return "adopt a shorter base with `plumb service set-xdg --runtime-dir <dir>`; the service profile records " +
			"the runtime directory, so no environment variable can move the socket"
	}
	return fmt.Sprintf("point %s at a shorter directory", socketPathShortenLever())
}

// legacyDaemonSocketPath is the socket under the cache-dir runtime location,
// or "" when that is the same directory in use now.
//
// It exists for DIAGNOSIS only — telling the user a daemon is running in the
// other directory — and never for connecting. RuntimeDir determines the
// socket, the control socket, the pid and the version file as a set, so a
// process that connected to one directory's socket while resolving the rest in
// the other would be half-migrated: `plumb web` and `plumb log-level` dial a
// control socket that is not there, doctor reads a version file one directory
// over and calls it missing, and `plumb restart` spawns a duplicate. One
// directory per process.
//
// It names the CACHE-directory location, which is the one an unupgraded install
// falls back to. The complete set of locations an older binary could be serving
// from — the environment-derived one included, which is a different directory
// again when the account authority is on — is legacyLocations (legacy.go), and
// that is what the migration probe and the launch fence use.
func legacyDaemonSocketPath() string {
	dir := paths.LegacyRuntimeDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "plumb.sock")
}

// socketAlive reports whether something is listening on a unix socket path.
func socketAlive(path string) bool {
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// socketPathLengthHint explains an over-long socket path, or returns "" when
// the path is not the problem. It rides every error the user can actually see
// when the daemon fails to come up.
//
// A Unix socket path lives in sun_path, a fixed-size array, so bind() answers
// an over-long path with EINVAL — "invalid argument", which says nothing about
// length. Every layer above then hides it: the daemon writes that error to
// daemon.log and exits, and `plumb serve` reported only that the daemon "did
// not start" in time. Reproduced on Linux with a long XDG_CACHE_HOME (the
// runtime dir follows os.UserCacheDir), where the daemon appeared to start and
// silently never came up.
func socketPathLengthHint(path string) string {
	if len(path) <= maxUnixSocketPath {
		return ""
	}
	return fmt.Sprintf(
		" (the path is %d bytes; a Unix socket path must fit in sun_path, at most %d — %s)",
		len(path), maxUnixSocketPath, socketPathShortenRemedy())
}

// daemonStartTimeoutError is what the caller sees when the socket never
// appears: `plumb serve` on a cold start, and `plumb restart` after a respawn.
//
// It is one function so the length hint cannot be attached to some of those
// paths and not others. That is exactly what happened first time round — the
// hint went on the daemon's own listen error, which goes to daemon.log before
// the daemon exits, so the user, who runs `plumb serve` through an MCP client,
// only ever saw the bare timeout.
func daemonStartTimeoutError(action, socketPath string, wait time.Duration) error {
	return fmt.Errorf("daemon did not %s within %s (socket: %s)%s", action, wait, socketPath, socketPathLengthHint(socketPath))
}

// startDaemonProcess launches a detached plumb daemon subprocess that inherits
// the two held launch locks — the account authority on fd 3 and the runtime
// lifetime lock on fd 4 (see daemon_launch.go): the caller must hold both, and
// closes its own copies afterwards. Logs are written to daemonLogPath(); the
// process is detached with Setsid so it outlives the calling plumb serve
// process.
//
// WHAT it starts and the environment it starts it in come from daemonLaunchSpec
// (service_launch.go): in canonical mode that is the service profile's
// executable with a clean environment, and a missing profile refuses here rather
// than guessing. In legacy mode it is this binary with this environment, exactly
// as before.
func startDaemonProcess(locks launchLocks) error {
	spec, err := resolveLaunchSpec()
	if err != nil {
		return err
	}
	cmd, logFile, err := daemonCommand(spec, locks)
	if err != nil {
		return err
	}
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
