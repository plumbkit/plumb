package cli

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// These tests drive the hook's REAL probe (claudeIdentityDaemon ->
// probeDaemonIdentity -> the control socket) against fake daemons, because
// that glue is where the safety decision is made: sending plumb_agent to a
// daemon that does not lift it fails every plumb call as an unknown parameter.

// probeTestEnv points the control socket, the PID file and the probe cache at
// a short private directory (a unix socket path must stay under 103 bytes,
// which a macOS t.TempDir() does not). Every variable the runtime directory
// and the wake directory resolve through is pinned: paths.RuntimeDir takes
// XDG_RUNTIME_DIR, else os.UserCacheDir — XDG_CACHE_HOME on Linux, HOME on
// macOS and as Linux's fallback — and wakeDir takes PLUMB_WAKE_DIR, else HOME.
// One left unpinned and a developer's exported value would have these tests
// unlink the real daemon's control socket and overwrite its PID file.
func probeTestEnv(t *testing.T) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "plb") //nolint:usetesting // a unix socket path must stay under 103 bytes; macOS t.TempDir() is longer
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("PLUMB_WAKE_DIR", filepath.Join(home, "wake"))
}

// TestProbeTestEnv_IsolatesEveryDaemonPath: with the developer's own
// variables pointing elsewhere, every path these tests write — control
// socket, PID file, probe cache — must land inside the private directory.
func TestProbeTestEnv_IsolatesEveryDaemonPath(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(elsewhere, "cache"))
	t.Setenv("PLUMB_WAKE_DIR", filepath.Join(elsewhere, "wake"))
	probeTestEnv(t)
	home := os.Getenv("HOME")
	for name, p := range map[string]string{
		"control socket": daemonCtrlSocketPath(),
		"PID file":       daemonPIDPath(),
		"probe cache":    filepath.Join(wakeDir(), identityProbeCacheFile),
	} {
		if !strings.HasPrefix(p, home+string(filepath.Separator)) {
			t.Errorf("%s %s escapes the private directory %s", name, p, home)
		}
	}
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
// the desktop connector's stamps for the whole TTL.
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

// currentDaemon answers like a daemon that lifts plumb_agent.
func currentDaemon(version string) func(net.Conn, string) {
	return func(c net.Conn, line string) {
		switch line {
		case "version":
			_, _ = c.Write([]byte("ok " + version + "\n"))
		case ctrlIdentityKeysCommand:
			_, _ = c.Write([]byte(identityKeysReply()))
		}
	}
}

func writePIDFile(t *testing.T, pid string) {
	t.Helper()
	if err := os.WriteFile(daemonPIDPath(), []byte(pid), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestIdentityProbe_DaemonSwappedInsideTheTTLIsReprobed is #532's
// reproduction through the real probe: the cache holds a current daemon's
// "plumb_agent is fine", the daemon is replaced inside the TTL by an older
// build that does not lift it, and the next call must ask the new daemon
// rather than stamp a key it rejects as an unknown parameter.
func TestIdentityProbe_DaemonSwappedInsideTheTTLIsReprobed(t *testing.T) {
	probeTestEnv(t)
	writePIDFile(t, "4101")
	fakeCtrlDaemon(t, currentDaemon("0.21.0"))
	if got := probedStampKey(); got != mcp.ArgLogicalAgentDeclaredKey {
		t.Fatalf("control: the current daemon got key %q", got)
	}
	if _, err := os.Stat(filepath.Join(wakeDir(), identityProbeCacheFile)); err != nil {
		t.Fatalf("control: the current daemon's answer was not cached: %v", err)
	}

	// The swap: a new process (new PID) binds a new control socket.
	writePIDFile(t, "4102")
	fakeCtrlDaemon(t, preKeysDaemon("0.20.3"))
	if got := probedStampKey(); got != mcp.ArgLogicalAgentKey {
		t.Fatalf("after a swap to a daemon that does not lift plumb_agent the hook stamped %q from the old daemon's cache", got)
	}
}

// TestDaemonInstanceMarker: the marker moves when any part of the instance
// changes, falls back to the socket alone without a usable PID file, and is
// "" — a miss — when the socket cannot be read.
func TestDaemonInstanceMarker(t *testing.T) {
	probeTestEnv(t)
	if got := daemonInstanceMarker(); got != "" {
		t.Fatalf("no PID file and no socket must be no marker, got %q", got)
	}
	writePIDFile(t, "4101")
	if got := daemonInstanceMarker(); got != "" {
		t.Fatalf("a PID file with no control socket must be no marker, got %q", got)
	}
	fakeCtrlDaemon(t, currentDaemon("0.21.0"))
	a := daemonInstanceMarker()
	if a == "" {
		t.Fatal("a PID file and a live control socket must give a marker")
	}
	if again := daemonInstanceMarker(); again != a {
		t.Fatalf("the marker is not stable for one instance: %q then %q", a, again)
	}

	writePIDFile(t, "4102")
	if b := daemonInstanceMarker(); b == a || b == "" {
		t.Fatalf("a new PID must move the marker: %q then %q", a, b)
	}

	// Same PID (a container restart), socket re-bound by the new process.
	writePIDFile(t, "4101")
	sock := daemonCtrlSocketPath()
	before, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	fakeCtrlDaemon(t, currentDaemon("0.21.0"))
	if c := daemonInstanceMarker(); c == a || c == "" {
		t.Fatalf("a re-bound control socket must move the marker even under the same PID: %q then %q", a, c)
	}

	// Each socket half on its own, since either can repeat: a re-bound socket
	// whose mtime lands on the old tick still differs by inode, and a socket
	// whose inode is handed out again still differs by mtime.
	if err := os.Chtimes(sock, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if c := daemonInstanceMarker(); c == a {
		t.Fatalf("a new socket inode must move the marker when the mtime repeats: %q", c)
	}
	sameInode := daemonInstanceMarker()
	later := before.ModTime().Add(time.Second)
	if err := os.Chtimes(sock, later, later); err != nil {
		t.Fatal(err)
	}
	if c := daemonInstanceMarker(); c == sameInode {
		t.Fatalf("a new socket mtime must move the marker under the same inode: %q", c)
	}

	// No usable PID file (the daemon only warns when it cannot write one):
	// the socket alone is the marker — stable, distinct from any marker that
	// had a PID, and still moved by a re-bind.
	withPID := daemonInstanceMarker()
	_ = os.Remove(daemonPIDPath())
	noPID := daemonInstanceMarker()
	if noPID == "" || noPID == withPID {
		t.Fatalf("a missing PID file must fall back to a socket-only marker distinct from %q, got %q", withPID, noPID)
	}
	for _, empty := range []string{"", "  \n"} {
		writePIDFile(t, empty)
		if got := daemonInstanceMarker(); got != noPID {
			t.Fatalf("an empty PID file (%q) must fall back like a missing one: %q, want %q", empty, got, noPID)
		}
	}
	_ = os.Remove(daemonPIDPath())
	fakeCtrlDaemon(t, currentDaemon("0.21.0"))
	if got := daemonInstanceMarker(); got == noPID || got == "" {
		t.Fatalf("a re-bound socket must move the socket-only marker: %q then %q", noPID, got)
	}
	_ = os.Remove(daemonCtrlSocketPath())
	if got := daemonInstanceMarker(); got != "" {
		t.Fatalf("no PID file and no socket must be no marker, got %q", got)
	}
}

// TestIdentityProbe_NoPIDFileStillCaches: a daemon that could not write its
// PID file must not cost two dials on every tool call, and a swap must still
// be caught through the socket.
func TestIdentityProbe_NoPIDFileStillCaches(t *testing.T) {
	probeTestEnv(t)
	var probes atomic.Int32
	fakeCtrlDaemon(t, func(c net.Conn, line string) {
		probes.Add(1)
		currentDaemon("0.21.0")(c, line)
	})
	if got := probedStampKey(); got != mcp.ArgLogicalAgentDeclaredKey {
		t.Fatalf("control: the current daemon got key %q", got)
	}
	first := probes.Load()
	if got := probedStampKey(); got != mcp.ArgLogicalAgentDeclaredKey || probes.Load() != first {
		t.Fatalf("with no PID file the second call re-probed (%d dials, then %d) — the cache never hits", first, probes.Load())
	}
	fakeCtrlDaemon(t, preKeysDaemon("0.20.3"))
	if got := probedStampKey(); got != mcp.ArgLogicalAgentKey {
		t.Fatalf("a swap with no PID file was served from the cache: key %q", got)
	}
}

// TestDaemonIdentity_CacheIsKeyedOnTheInstance covers the cache rule without
// a daemon: a record answers only for the instance it was written for, and
// an unreadable marker neither hits nor writes.
func TestDaemonIdentity_CacheIsKeyedOnTheInstance(t *testing.T) {
	cache := filepath.Join(t.TempDir(), identityProbeCacheFile)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	probes := 0
	older := func() (identityProbeRecord, error) {
		probes++
		return identityProbeRecord{DaemonVersion: "0.20.3"}, nil
	}
	writeIdentityProbe(cache, identityProbeRecord{DaemonVersion: "0.21.0", DeclaredKey: true, DaemonInstance: "A", CheckedAt: now})

	if rec := daemonIdentity(older, "A", cache, now.Add(10*time.Second)); !rec.DeclaredKey || probes != 0 {
		t.Fatalf("control: the same instance inside the TTL must hit: %+v, probes=%d", rec, probes)
	}
	if rec := daemonIdentity(older, "B", cache, now.Add(20*time.Second)); rec.DeclaredKey || probes != 1 {
		t.Fatalf("another instance must re-probe: %+v, probes=%d", rec, probes)
	}
	if rec, _ := readIdentityProbe(cache); rec.DaemonInstance != "B" || rec.DeclaredKey {
		t.Fatalf("the re-probe must be cached for the new instance: %+v", rec)
	}

	// An unreadable marker is a miss even against a record that also lacks
	// one (a cache written by an older hook binary).
	writeIdentityProbe(cache, identityProbeRecord{DaemonVersion: "0.21.0", DeclaredKey: true, CheckedAt: now})
	probes = 0
	if rec := daemonIdentity(older, "", cache, now.Add(10*time.Second)); rec.DeclaredKey || probes != 1 {
		t.Fatalf("no marker must re-probe, never trust the cache: %+v, probes=%d", rec, probes)
	}
	fresh := filepath.Join(t.TempDir(), identityProbeCacheFile)
	daemonIdentity(older, "", fresh, now)
	if _, err := os.Stat(fresh); err == nil {
		t.Fatal("an answer with no instance marker was cached; no later call could ever match it")
	}
}
