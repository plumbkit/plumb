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

// daemonLogPath returns the path for daemon log output, under the OS log
// directory resolved by internal/paths:
//   - macOS  : ~/Library/Logs/plumb/daemon.log
//   - Linux  : $XDG_STATE_HOME/plumb/daemon.log  (fallback: ~/.local/state/plumb/daemon.log)
//   - Windows: %LocalAppData%\plumb\daemon.log
func daemonLogPath() string {
	return filepath.Join(paths.LogDir(), "daemon.log")
}
