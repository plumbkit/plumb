package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRestartActionPromptWording(t *testing.T) {
	t.Parallel()
	got := ansiStripForCLITest(renderStopConfirmationPrompt(restartActionPrompt.consequence, 2, 1))
	if !strings.Contains(got, "Restarting the daemon") || !strings.Contains(got, "reconnect automatically") {
		t.Fatalf("restart prompt should explain the resilient-reconnect behaviour:\n%s", got)
	}
	if strings.Contains(got, "will terminate all active sessions") {
		t.Fatalf("restart prompt should not use the stop wording:\n%s", got)
	}
	if !strings.Contains(got, "You have 2 active sessions.") {
		t.Fatalf("restart prompt missing session count:\n%s", got)
	}
}

func TestDialDaemonOnce(t *testing.T) {
	t.Parallel()
	socketPath := filepath.Join(t.TempDir(), "s.sock")

	if dialDaemonOnce(socketPath) {
		t.Fatal("dialDaemonOnce should be false when nothing is listening")
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Skipf("cannot bind unix socket (path too long?): %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if !dialDaemonOnce(socketPath) {
		t.Fatal("dialDaemonOnce should be true when a listener accepts")
	}
}

// withShortTempRuntime is withTempRuntime for the tests that BIND the daemon
// socket rather than merely name it.
//
// t.TempDir() cannot be used for those: on macOS it lands under
// /var/folders/<2>/<28>/T/<TestName><10>/001, which on its own eats most of the
// 103-byte sun_path ceiling — the bind then fails with EINVAL ("invalid
// argument", saying nothing about length) and the test SKIPS. A skipped test
// guards nothing, so the socket tests get an explicitly short base instead. CI
// makes this worse, not better: it points GOTMPDIR inside the checkout, so
// t.TempDir() is longer there than on a developer's box.
func withShortTempRuntime(t *testing.T) {
	t.Helper()
	base := "/tmp"
	if info, err := os.Stat(base); err != nil || !info.IsDir() {
		base = os.TempDir()
	}
	//nolint:usetesting // t.TempDir() is exactly what cannot be used here: its macOS path
	// exhausts the 103-byte sun_path ceiling and the socket bind fails with EINVAL. That is
	// the whole reason this helper exists; see the doc comment above.
	dir, err := os.MkdirTemp(base, "plumb-rt-")
	if err != nil {
		t.Fatalf("creating short runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Same three variables, and the same reasons, as withTempRuntime.
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
}

// listenOnDaemonSocket stands a listener up on daemonSocketPath(), the way a
// resilient `plumb serve` proxy's respawned daemon would. Call
// withShortTempRuntime first so this is a temp socket, never the developer's
// live daemon.
func listenOnDaemonSocket(t *testing.T) {
	t.Helper()
	path := daemonSocketPath()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("binding unix socket %s (%d bytes): %v", path, len(path), err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
}

func writeDaemonPIDFile(t *testing.T, pid int) {
	t.Helper()
	if err := os.WriteFile(daemonPIDPath(), fmt.Appendf(nil, "%d\n", pid), 0o600); err != nil {
		t.Fatalf("writing pid file: %v", err)
	}
}

// TestRespawnDaemon_AnnouncesStartingWhenAnotherProcessWonTheRace is the
// reported regression. `plumb restart` printed "Starting..." only on the branch
// where restart itself called startDaemonProcess — but with sessions attached, a
// resilient `plumb serve` proxy usually respawns the daemon the instant the old
// one dies and wins that race, so respawnDaemon's first dial succeeded and the
// line the operator was promised never appeared. The announcement belongs to the
// command, not to whichever process happens to do the spawning.
func TestRespawnDaemon_AnnouncesStartingWhenAnotherProcessWonTheRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-socket bind semantics differ on Windows")
	}
	withShortTempRuntime(t)
	listenOnDaemonSocket(t) // the proxy got there first
	writeDaemonPIDFile(t, os.Getpid())

	out := captureStdout(t, func() {
		if err := respawnDaemon([]int{os.Getpid() + 1}); err != nil {
			t.Fatalf("respawnDaemon: %v", err)
		}
	})

	if !strings.Contains(out, "Starting...") {
		t.Fatalf("restart must announce the starting phase even when another process respawned the daemon; got:\n%s", out)
	}
	want := fmt.Sprintf("Daemon restarted (PID %d).", os.Getpid())
	if !strings.Contains(out, want) {
		t.Fatalf("want %q in output; got:\n%s", want, out)
	}
	if strings.Index(out, "Starting...") > strings.Index(out, "Daemon restarted") {
		t.Fatalf("the starting line must precede the restarted line; got:\n%s", out)
	}
}

// TestWaitForDaemonPID_NeverReportsAStoppedPID guards the second half of the
// same line. The outgoing daemon does not erase its PID file, so a naive read
// right after the kill returns the corpse's number and the command cheerfully
// reports the process it just killed as the fresh one.
func TestWaitForDaemonPID_NeverReportsAStoppedPID(t *testing.T) {
	withTempRuntime(t)
	origGrace := daemonPIDGrace
	t.Cleanup(func() { daemonPIDGrace = origGrace })
	daemonPIDGrace = 50 * time.Millisecond

	stopped := 424242
	writeDaemonPIDFile(t, stopped) // stale: the daemon we just SIGTERM'd wrote it

	if got := waitForDaemonPID([]int{stopped}); got == stopped {
		t.Fatal("waitForDaemonPID returned the PID the restart had just stopped")
	}
	if got := waitForDaemonPID(nil); got != stopped {
		t.Fatalf("with nothing excluded the file's PID should be returned: want %d, got %d", stopped, got)
	}
}

// TestWaitForDaemonPID_PicksUpTheFreshPIDFile covers the race the grace window
// exists for: the daemon publishes its PID just after binding the socket, so the
// file can still be stale (or missing) when the dial first succeeds.
func TestWaitForDaemonPID_PicksUpTheFreshPIDFile(t *testing.T) {
	withTempRuntime(t)

	stopped := 424242
	writeDaemonPIDFile(t, stopped)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(daemonPIDPath(), fmt.Appendf(nil, "%d\n", 515151), 0o600)
	}()

	if got := waitForDaemonPID([]int{stopped}); got != 515151 {
		t.Fatalf("want the freshly published PID 515151, got %d", got)
	}
}

// TestForceKillIfAlive is the F1 regression: restart must SIGKILL a daemon that
// ignores SIGTERM, so a hung daemon is actually replaced rather than re-dialled.
func TestForceKillIfAlive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL semantics differ on Windows")
	}
	forceKillIfAlive(-1) // already-gone / invalid pid: no-op, must not panic

	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	pid := cmd.Process.Pid
	// Reap in the background so the kill-0 liveness check reflects real death
	// rather than a lingering (unreaped) zombie.
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()

	forceKillIfAlive(pid)

	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("forceKillIfAlive did not kill the process")
	}
}
