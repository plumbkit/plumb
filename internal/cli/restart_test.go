package cli

import (
	"net"
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
