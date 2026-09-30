package cli

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// These tests drive the hook's REAL probe (claudeIdentityDaemon ->
// probeDaemonIdentity -> the control socket) against fake daemons, because
// that glue is where the safety decision is made: sending plumb_agent to a
// daemon that does not lift it fails every plumb call as an unknown parameter.

// probeTestEnv points the control socket and the probe cache at a short
// private directory (a unix socket path must stay under 103 bytes, which a
// macOS t.TempDir() does not).
func probeTestEnv(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "plb") //nolint:usetesting // a unix socket path must stay under 103 bytes; macOS t.TempDir() is longer
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("PLUMB_WAKE_DIR", filepath.Join(home, "wake"))
}

// fakeCtrlDaemon listens on the control socket and answers each one-line
// command with reply(line). It is stopped at test cleanup.
func fakeCtrlDaemon(t *testing.T, reply func(net.Conn, string)) {
	t.Helper()
	sock := daemonCtrlSocketPath()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				reply(c, strings.TrimSpace(line))
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
}

// preKeysDaemon answers `version` and refuses everything else, like every
// daemon from 0.19.1 up to the one that added identity-keys.
func preKeysDaemon(version string) func(net.Conn, string) {
	return func(c net.Conn, line string) {
		if line == "version" {
			_, _ = c.Write([]byte("ok " + version + "\n"))
			return
		}
		_, _ = c.Write([]byte(`error: unknown command "` + line + `"` + "\n"))
	}
}

func probedStampKey() string { return identityStampKey(claudeIdentityDaemon) }

func TestIdentityProbe_DaemonWithoutIdentityKeysGetsTheOldKey(t *testing.T) {
	for _, v := range []string{"0.20.3", "dev"} {
		t.Run(v, func(t *testing.T) {
			probeTestEnv(t)
			fakeCtrlDaemon(t, preKeysDaemon(v))
			if got := probedStampKey(); got != mcp.ArgLogicalAgentKey {
				t.Fatalf("a daemon that never listed plumb_agent got key %q; it would reject it", got)
			}
		})
	}
}

// The positive case, answered by the real handleCtrlConn under a 0.20.3
// version label — the dev-build case the version threshold got wrong.
func TestIdentityProbe_CurrentDaemonGetsTheDeclaredKey(t *testing.T) {
	probeTestEnv(t)
	old := Version
	Version = "0.20.3"
	t.Cleanup(func() { Version = old })
	fakeCtrlDaemon(t, func(c net.Conn, line string) {
		p1, p2 := net.Pipe()
		go handleCtrlConn(p2, "info", "text", ctrlHandlers{})
		_, _ = p1.Write([]byte(line + "\n"))
		r, _ := bufio.NewReader(p1).ReadString('\n')
		_ = p1.Close()
		_, _ = c.Write([]byte(r))
	})
	if got := probedStampKey(); got != mcp.ArgLogicalAgentDeclaredKey {
		t.Fatalf("a daemon listing plumb_agent got key %q", got)
	}
}

func TestIdentityProbe_NoDaemonStampsNothingAndCachesNothing(t *testing.T) {
	probeTestEnv(t)
	if got := probedStampKey(); got != "" {
		t.Fatalf("no daemon, key %q", got)
	}
	if _, err := os.Stat(filepath.Join(wakeDir(), identityProbeCacheFile)); err == nil {
		t.Fatal("a failed probe was cached")
	}
}

// An identity-keys probe that fails on I/O is "no" for this call (the safe
// key) but is not cached, so the next call asks again rather than stripping
// the desktop connector's stamps for a minute.
func TestIdentityProbe_TransientKeysFailureIsNotCached(t *testing.T) {
	probeTestEnv(t)
	fakeCtrlDaemon(t, func(c net.Conn, line string) {
		if line == "version" {
			_, _ = c.Write([]byte("ok 0.21.0\n"))
		}
		// identity-keys: close without replying (a short read)
	})
	if got := probedStampKey(); got != mcp.ArgLogicalAgentKey {
		t.Fatalf("an unanswered identity-keys must fall back to the safe key, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(wakeDir(), identityProbeCacheFile)); err == nil {
		t.Fatal("an unanswered identity-keys probe was cached as a definitive no")
	}
}
