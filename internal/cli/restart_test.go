package cli

import (
	"net"
	"path/filepath"
	"strings"
	"testing"
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
