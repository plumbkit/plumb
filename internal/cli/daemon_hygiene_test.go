package cli

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/daemons"
)

// Tests for socket and runtime-marker hygiene (PLAN-529).
//
// Every test here is host-safe: a short temp runtime directory (withShortTempRuntime
// exists because t.TempDir() exhausts the sun_path ceiling on macOS), real unix
// sockets and real files in it, and never a plumb daemon. Nothing here touches the
// account's own runtime directory: the account seam points at the temp tree, so a
// test binary cannot resolve — let alone take — the running daemon's authority.

// guardFor binds a socket guard over path and returns it with the identities it
// published, in order.
func guardFor(t *testing.T, path string, holdsAuthority bool) (*socketGuard, *[]fileIdentity) {
	t.Helper()
	published := &[]fileIdentity{}
	g, err := newSocketGuard(path,
		func() bool { return holdsAuthority },
		func(_ string, id fileIdentity) { *published = append(*published, id) })
	if err != nil {
		t.Fatalf("newSocketGuard(%s): %v", path, err)
	}
	t.Cleanup(g.close)
	return g, published
}

// waitForCond polls cond until it holds or the deadline passes.
func waitForCond(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return cond()
}

// TestBindOwnUnixSocket_CloseLeavesAReplacementAlone is the property
// SetUnlinkOnClose(false) exists for, at its lowest level: net's default is to
// unlink whatever is at the path when the listener closes, and a file somebody put
// there after the bind is not this daemon's to remove.
func TestBindOwnUnixSocket_CloseLeavesAReplacementAlone(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()

	ln, err := bindOwnUnixSocket(path)
	if err != nil {
		t.Fatalf("binding %s: %v", path, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the daemon socket: %v", err)
	}
	if err := os.WriteFile(path, []byte("somebody else's file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the file at %s did not survive the listener's close (%v): the listener is still unlinking its path", path, err)
	}
}

// TestBindOwnUnixSocket_RefusesToDisplaceALiveSocket is the startup half of the
// same rule: a stale entry (a daemon that died) is ordinary crash recovery and is
// cleared, while a socket something is listening on is never displaced.
func TestBindOwnUnixSocket_RefusesToDisplaceALiveSocket(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()

	live, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("binding the live socket: %v", err)
	}
	defer live.Close()

	if _, err := bindOwnUnixSocket(path); err == nil {
		t.Fatal("bindOwnUnixSocket displaced a LIVE socket; a running process's socket must never be removed")
	} else if !strings.Contains(err.Error(), "live socket") {
		t.Errorf("the refusal must say what it found: %v", err)
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err != nil {
		t.Fatalf("the live socket stopped serving after the refusal: %v", err)
	} else {
		_ = c.Close()
	}
}

// TestClearStaleSocket_StaleIsClearedAndLiveIsNot separates the two entries the
// refusal above turns on: a socket file with no listener behind it, and a
// non-socket entry, are both cleared; a live one is not.
func TestClearStaleSocket_StaleIsClearedAndLiveIsNot(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()

	// A socket file with nobody listening: bind, then close without unlinking.
	dead, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	dead.SetUnlinkOnClose(false)
	if err := dead.Close(); err != nil {
		t.Fatalf("closing the dead listener: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("precondition: the stale socket file is gone: %v", err)
	}
	if err := clearStaleSocket(path); err != nil {
		t.Fatalf("a stale socket must be cleared: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stale socket file survived clearStaleSocket (%v)", err)
	}

	// A non-socket entry is never live, so it is cleared too.
	if err := os.WriteFile(path, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clearStaleSocket(path); err != nil {
		t.Fatalf("a non-socket entry must be cleared: %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the non-socket entry survived clearStaleSocket (%v)", err)
	}

	// A live socket is refused, and left exactly where it is.
	live, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("binding the live socket: %v", err)
	}
	defer live.Close()
	if err := clearStaleSocket(path); err == nil {
		t.Error("clearStaleSocket removed a LIVE socket")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the live socket is gone: %v", err)
	}
}

// TestFileMarker_RemovesOnlyItsOwnFile is the inode-checked removal every runtime
// marker goes through: our own file is removed, a file that replaced it is not,
// and a marker whose identity could not be read authorises nothing.
func TestFileMarker_RemovesOnlyItsOwnFile(t *testing.T) {
	t.Run("our own file is removed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "plumb.pid")
		if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		markRuntimeFile(path).removeIfOurs("pid file")
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("this daemon's own file was not removed (%v)", err)
		}
	})

	t.Run("a replacement is left in place", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "plumb.pid")
		if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := markRuntimeFile(path)
		own, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("somebody else"), 0o600); err != nil {
			t.Fatal(err)
		}
		replacement, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		// This test is about a DIFFERENT file, so say what "different" rests on.
		// On a filesystem that hands the freed inode straight back — ext4 does,
		// 20 of 20 immediate recreations in a container measurement — the device
		// and inode are identical and the change time is the whole distinction;
		// the assertion below is what proves the test is not vacuous there.
		if identityOf(own).dev == identityOf(replacement).dev && identityOf(own).ino == identityOf(replacement).ino {
			t.Logf("the filesystem reused inode %d; the change time is what distinguishes the replacement (%+v vs %+v)",
				identityOf(own).ino, identityOf(own), identityOf(replacement))
		}
		if marker.namesInfo(replacement) {
			t.Fatalf("the marker claims a file this daemon did not create: own=%+v replacement=%+v", identityOf(own), identityOf(replacement))
		}
		marker.removeIfOurs("pid file")
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("a file that replaced this daemon's own was removed (own=%+v replacement=%+v): %v",
				identityOf(own), identityOf(replacement), err)
		}
	})

	t.Run("a file that inherited the inode is refused", func(t *testing.T) {
		// The ext4 case, made deterministic so it does not depend on the
		// allocator: the marker's device and inode are the live file's, and only
		// the change time differs. (A platform whose identityOf cannot read a
		// change time has 0 on both sides; the marker there is the weaker
		// (device, inode) claim, and this subtest's expectation is the ctime one.)
		path := filepath.Join(t.TempDir(), "plumb.pid")
		if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := markRuntimeFile(path)
		switch {
		case marker.id.ctime != 0:
			// The identity carries the change time: the case below is real.
		case runtime.GOOS == "linux" || runtime.GOOS == "darwin":
			// Both platforms plumb SHIPS on read the change time (their
			// daemon_identity_*.go files), so a zero here means the identity
			// silently lost the field it was strengthened with. Skipping would
			// turn that into a test that passes by not looking.
			t.Fatalf("identityOf read no change time on %s: %+v", runtime.GOOS, marker.id)
		default:
			t.Skipf("this platform (%s) has no change time in its identity", runtime.GOOS)
		}
		forged := marker
		forged.id.ctime++
		if !forged.id.known() {
			t.Fatal("precondition: the forged marker must still name a device and inode")
		}
		forged.removeIfOurs("pid file")
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("a marker whose change time does not match removed the file: %v", err)
		}
		// The genuine marker still removes it: the strengthening must not turn
		// every removal into a refusal.
		marker.removeIfOurs("pid file")
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("this daemon's own file survived its own marker (%v)", err)
		}
	})

	t.Run("a missing path is nothing to do", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "plumb.version")
		if err := os.WriteFile(path, []byte("0.24.1"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker := markRuntimeFile(path)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		marker.removeIfOurs("version file") // must not panic or recreate anything
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("removeIfOurs created or left something at a missing path (%v)", err)
		}
	})

	t.Run("an unreadable identity authorises no removal", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "plumb.pid")
		marker := markRuntimeFile(path) // the file does not exist yet: no identity
		if marker.id.known() {
			t.Fatal("precondition: a marker for a missing path must have no identity")
		}
		if err := os.WriteFile(path, []byte("1"), 0o600); err != nil {
			t.Fatal(err)
		}
		marker.removeIfOurs("pid file")
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("a marker with no identity removed a file it could not identify: %v", err)
		}
	})
}

// TestSocketGuard_NeverRemovesAReplacementAndSaysSo is the card's foreign-inode
// case: somebody replaces the socket with their own, and the daemon must leave it
// alone, keep the listener it has, and report the replacement loudly.
func TestSocketGuard_NeverRemovesAReplacementAndSaysSo(t *testing.T) {
	withShortTempRuntime(t)
	logs := captureLogs(t)
	path := daemonSocketPath()
	g, _ := guardFor(t, path, true)
	myListener := g.listener()

	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the daemon's socket: %v", err)
	}
	foreign, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("binding the foreign socket: %v", err)
	}
	defer foreign.Close()
	foreignMarker := markRuntimeFile(path)

	g.check()

	now, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the daemon removed the foreign socket: %v", err)
	}
	if !foreignMarker.namesInfo(now) {
		t.Fatalf("the file at %s is no longer the foreign socket", path)
	}
	if g.listener() != myListener {
		t.Error("the guard replaced its listener for a path that is somebody else's")
	}
	if !g.foreign {
		t.Error("the guard did not record that the path is somebody else's")
	}
	if !strings.Contains(logs.String(), "no longer names this daemon's socket") {
		t.Errorf("the daemon never reported the replacement; log:\n%s", logs.String())
	}

	// And a second check does not change that, nor remove it.
	g.check()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("a second check removed the foreign socket: %v", err)
	}
}

// TestSocketGuard_TreatsAReusedInodeAsSomebodyElses makes the ext4 case
// deterministic: the marker's device and inode are the live socket's and only the
// change time differs, which is exactly the state a filesystem that hands the
// freed inode straight back creates. The guard must see a stranger's socket —
// not rebind over it, not remove it, and say so.
func TestSocketGuard_TreatsAReusedInodeAsSomebodyElses(t *testing.T) {
	withShortTempRuntime(t)
	logs := captureLogs(t)
	path := daemonSocketPath()
	g, _ := guardFor(t, path, true)
	if g.marker.id.ctime == 0 {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			t.Fatalf("identityOf read no change time on %s: %+v", runtime.GOOS, g.marker.id)
		}
		t.Skipf("this platform (%s) has no change time in its identity", runtime.GOOS)
	}
	own, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	g.marker.id.ctime++ // same device and inode, a different change time

	if g.check() {
		t.Error("the guard re-bound a path that holds a file it did not create")
	}
	if !g.foreign {
		t.Error("the guard accepted a socket whose change time is not the one it recorded")
	}
	now, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the daemon removed a file whose change time does not match: %v", err)
	}
	if identityOf(now) != identityOf(own) {
		t.Errorf("the file at the path changed: %+v -> %+v", identityOf(own), identityOf(now))
	}
	if !strings.Contains(logs.String(), "no longer names this daemon's socket") {
		t.Errorf("the daemon never reported the mismatched change time; log:\n%s", logs.String())
	}
}

// TestSocketGuard_ShutdownLeavesAReplacementInPlace is the card's clean-shutdown
// case, run in the order runDaemon defers it: the listener closes first, then the
// removal examines the path — and the replacement survives both.
func TestSocketGuard_ShutdownLeavesAReplacementInPlace(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()
	g, _ := guardFor(t, path, true)

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not the daemon's socket\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement := markRuntimeFile(path)

	g.close()
	g.removeSocketIfOurs("daemon socket")

	now, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the shutdown removed a file this daemon did not create: %v", err)
	}
	if !replacement.namesInfo(now) {
		t.Fatal("the file at the path is not the replacement that was there before shutdown")
	}
}

// TestSocketGuard_RebindsWhenTheRuntimeDirectoryIsDeleted is the self-heal path:
// the whole runtime directory goes, and a daemon that holds the authority binds
// the path again and serves on the new listener.
func TestSocketGuard_RebindsWhenTheRuntimeDirectoryIsDeleted(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()
	g, published := guardFor(t, path, true)
	before := (*published)[len(*published)-1]

	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("precondition: the socket path still exists (%v)", err)
	}

	if !g.check() {
		t.Fatal("check did not re-bind the socket after its path disappeared")
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("the socket was not re-bound: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket after the re-bind", path)
	}
	after := (*published)[len(*published)-1]
	if after == before {
		t.Error("the re-bind published the same identity as before, so the record still names the deleted file")
	}
	if got := markRuntimeFile(path); got.id != after {
		t.Errorf("the published identity %+v is not the file that was bound %+v", after, got.id)
	}

	// A client reaches the daemon again, through the re-bound socket and the
	// listener the guard now accepts on.
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dialling the re-bound socket: %v", err)
	}
	defer c.Close()
	conn, err := g.Accept()
	if err != nil {
		t.Fatalf("accepting on the re-bound socket: %v", err)
	}
	_ = conn.Close()
}

// TestSocketGuard_DoesNotRebindWithoutTheAuthority: the authority is what makes a
// re-bind safe (nobody else may be serving this account). Without it the path is
// left alone, because it may be somebody else's to bind.
func TestSocketGuard_DoesNotRebindWithoutTheAuthority(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()
	g, _ := guardFor(t, path, false)

	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if g.check() {
		t.Error("check re-bound the socket without the account authority")
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a daemon without the authority created a socket path (%v)", err)
	}
}

// TestSocketGuard_RetriesOnANewListenerAfterAnAcceptError covers the second place
// the path is examined: an accept error that is not "the listener was closed" must
// lead to a check and, if the path was re-bound, to accepting on the new listener
// rather than returning the error. The failure is injected because an fd-level
// accept failure cannot be provoked on demand.
func TestSocketGuard_RetriesOnANewListenerAfterAnAcceptError(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()
	g, _ := guardFor(t, path, true)

	var calls atomic.Int32
	realAccept := g.accept
	g.accept = func(ln *net.UnixListener) (net.Conn, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("accept failed for a reason that is not a closed listener")
		}
		return realAccept(ln)
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}

	type accepted struct {
		conn net.Conn
		err  error
	}
	done := make(chan accepted, 1)
	go func() {
		conn, err := g.Accept()
		done <- accepted{conn, err}
	}()

	// The failed accept is what re-binds the path; dial once it is back.
	if !waitForCond(t, 5*time.Second, func() bool { _, err := os.Lstat(path); return err == nil }) {
		t.Fatal("Accept did not re-bind the socket path after the accept error")
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dialling the re-bound socket: %v", err)
	}
	defer c.Close()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Accept returned the accept error instead of retrying on the new listener: %v", got.err)
		}
		_ = got.conn.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("Accept never returned a connection from the re-bound socket")
	}
	if calls.Load() < 2 {
		t.Errorf("accept was called %d time(s); the retry on the new listener did not happen", calls.Load())
	}
}

// TestSocketGuard_AClosedListenerIsNotAPathFault: shutdown closes the listener,
// and the check that runs on every accept error must not re-bind the path then —
// that would plant a socket nothing would ever accept on.
func TestSocketGuard_AClosedListenerIsNotAPathFault(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()
	g, _ := guardFor(t, path, true)

	// Close the listener behind the guard's back, so it still believes it is
	// open: this is the state inside Accept when the shutdown goroutine closes it.
	if err := g.listener().Close(); err != nil {
		t.Fatalf("closing the listener: %v", err)
	}
	// The path is missing too, so a check WOULD re-bind it.
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	assertClosedAccept(t, g, "a closed listener's accept error")
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a closed listener's accept error re-bound the socket path (%v)", err)
	}

	// A guard that was closed properly answers the same way, and never re-binds a
	// path that disappears afterwards.
	g.close()
	assertClosedAccept(t, g, "a closed guard")
	if g.check() {
		t.Error("a closed guard re-bound the socket path after shutdown")
	}
}

// assertClosedAccept requires Accept to return net.ErrClosed promptly. It is
// bounded, so a mutant that treats a closed listener as a path fault and retries
// fails this test instead of hanging it.
func assertClosedAccept(t *testing.T, g *socketGuard, what string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := g.Accept()
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("%s: Accept = %v, want net.ErrClosed", what, err)
		}
	case <-time.After(2 * time.Second):
		t.Errorf("%s: Accept did not return — a closed listener is being retried as if it were a path fault", what)
	}
}

// TestSocketGuard_RefreshesThePublicationOnEveryCheck: the authority record is
// rewritten while the socket is where the daemon put it, which is what lets a
// reader tell a live holder from a stale record whose pid was recycled.
func TestSocketGuard_RefreshesThePublicationOnEveryCheck(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()
	g, published := guardFor(t, path, true)
	before := len(*published)

	g.check()

	if len(*published) != before+1 {
		t.Errorf("check wrote the authority record %d time(s), want 1 — the record's freshness is what a reader trusts", len(*published)-before)
	}
}

// TestSocketGuard_TheTickerHealsWithoutAClient: the ticker is what notices a path
// deleted while the daemon sits idle in Accept — deleting the socket file does not
// disturb a listening socket, so no accept error ever comes. The interval is
// shortened here; the loop is the production one.
func TestSocketGuard_TheTickerHealsWithoutAClient(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonSocketPath()
	g, _ := guardFor(t, path, true)
	g.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.run(ctx)

	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	if !waitForCond(t, 5*time.Second, func() bool { _, err := os.Lstat(path); return err == nil }) {
		t.Fatal("the hygiene ticker never re-bound the deleted socket path")
	}
}

// TestPublishDaemonPID_LeavesALiveControlSocketAlone: the PID file must appear —
// the identity hook needs it — but a control socket something is listening on is
// not this daemon's to remove. The stale case is the ordinary crash recovery the
// same call still has to perform.
func TestPublishDaemonPID_LeavesALiveControlSocketAlone(t *testing.T) {
	withShortTempRuntime(t)
	path := daemonCtrlSocketPath()
	write := func(name string, data []byte, perm os.FileMode) error { return os.WriteFile(name, data, perm) }

	live, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("binding the live control socket: %v", err)
	}
	defer live.Close()
	if err := publishDaemonPID(write); err != nil {
		t.Fatalf("publishDaemonPID: %v", err)
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("the live control socket was removed: %v", err)
	}
	if _, err := os.Stat(daemonPIDPath()); err != nil {
		t.Errorf("the PID file was not written: %v", err)
	}

	// A socket nobody is listening on is stale, and clearing it is what keeps a
	// PID-reusing successor from showing the dead daemon's marker.
	live.SetUnlinkOnClose(false)
	if err := live.Close(); err != nil {
		t.Fatalf("closing the live listener: %v", err)
	}
	if err := publishDaemonPID(write); err != nil {
		t.Fatalf("publishDaemonPID (stale): %v", err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a stale control socket must still be cleared (%v)", err)
	}
}

// TestDaemonSocketPublication_TrustsOnlyALiveFreshRecord: the socket record is
// evidence about a running daemon only while it is both fresh and owned by a live
// pid. A stale record (a dead holder whose pid was recycled) must not make a
// deleted path read as a live daemon's fault.
func TestDaemonSocketPublication_TrustsOnlyALiveFreshRecord(t *testing.T) {
	withTempRuntime(t)
	path := authorityLockPathForTest(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("creating the authority directory: %v", err)
	}
	socket := filepath.Join(t.TempDir(), "plumb.sock")
	write := func(m authorityMetadata) {
		t.Helper()
		if err := os.WriteFile(path, authorityJSON(t, m), 0o600); err != nil {
			t.Fatalf("writing the metadata: %v", err)
		}
	}

	write(authorityMetadata{PID: os.Getpid(), Socket: socket, SocketDev: 7, SocketIno: 4242, SocketCtime: 99, Published: time.Now()})
	pub, ok := daemonSocketPublication()
	if !ok || pub.pid != os.Getpid() || pub.path != socket || pub.id.ino != 4242 || pub.id.ctime != 99 {
		t.Fatalf("a fresh record of a live holder must be trusted, change time included: %+v (ok=%v)", pub, ok)
	}

	// A record written before the change time was published carries none. It is
	// still a record of a live holder, with the weaker (device, inode) identity:
	// the reader must not treat "no change time" as a mismatch, or every healthy
	// daemon an older binary started would be reported as a replaced socket.
	write(authorityMetadata{PID: os.Getpid(), Socket: socket, SocketDev: 7, SocketIno: 4242, Published: time.Now()})
	if older, ok := daemonSocketPublication(); !ok || older.id.ctime != 0 {
		t.Fatalf("a record without a change time must still be trusted, with ctime 0: %+v (ok=%v)", older, ok)
	}

	stale := authorityMetadata{PID: os.Getpid(), Socket: socket, Published: time.Now().Add(-2 * daemonSocketPublicationFreshness)}
	write(stale)
	if _, ok := daemonSocketPublication(); ok {
		t.Error("a record nobody has refreshed was trusted as a live holder's")
	}

	write(authorityMetadata{PID: 1 << 30, Socket: socket, Published: time.Now()})
	if _, ok := daemonSocketPublication(); ok {
		t.Error("a dead holder's record was trusted")
	}

	write(authorityMetadata{PID: os.Getpid(), Published: time.Now()})
	if _, ok := daemonSocketPublication(); ok {
		t.Error("a record naming no socket was trusted")
	}
}

// TestDaemonHoldsAuthority pins the single predicate the re-bind turns on, so a
// daemon cannot re-bind a path by mistake (or refuse to heal one) because the
// authority moved to another lock.
func TestDaemonHoldsAuthority(t *testing.T) {
	if daemonHoldsAuthority(daemonLocks{}) {
		t.Error("a daemon holding neither lock must not report the authority")
	}
	if daemonHoldsAuthority(daemonLocks{Lifetime: &os.File{}}) {
		t.Error("the runtime lifetime lock alone is not the account authority")
	}
	if !daemonHoldsAuthority(daemonLocks{Authority: &os.File{}}) {
		t.Error("a held account authority must report yes")
	}
}

// TestSocketPathStateFor_ComparesThePathWithThePublication is the mapping doctor
// and `plumb debug daemons` render from: a path that still holds the published
// file is healthy, a replaced one and a missing one are faults.
func TestSocketPathStateFor_ComparesThePathWithThePublication(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plumb.sock")
	if err := os.WriteFile(path, []byte("the socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	pub := socketPublication{pid: 4242, path: path, id: identityOf(info)}

	healthy := socketPathStateFor(path, true, []int{4242}, pub, true)
	if !healthy.Exists || !healthy.IdentityMatches {
		t.Fatalf("the published file is at the path, so the state must match: %+v", healthy)
	}
	if healthy.fault() != "" || healthy.note() != "" {
		t.Errorf("a healthy path must say nothing: fault=%q note=%q", healthy.fault(), healthy.note())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("somebody else's file"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if identityOf(replacement).ino == pub.id.ino {
		t.Logf("the filesystem reused inode %d; the change time is what distinguishes the replacement", pub.id.ino)
	}
	replaced := socketPathStateFor(path, true, nil, pub, true)
	if !replaced.IdentityKnown || replaced.IdentityMatches {
		t.Fatalf("the path no longer holds the published file: published=%+v replacement=%+v state=%+v", pub.id, identityOf(replacement), replaced)
	}
	// The same inode with a different change time is a replacement too — the ext4
	// case, made deterministic here rather than left to the allocator.
	forged := pub
	forged.id.ctime++
	if st := socketPathStateFor(path, true, nil, forged, true); st.IdentityMatches {
		t.Errorf("a file whose change time differs from the published one was accepted: published=%+v state=%+v", forged.id, st)
	}
	if !strings.Contains(replaced.fault(), "replaced") {
		t.Errorf("a replaced socket must be a fault, got %q", replaced.fault())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	gone := socketPathStateFor(path, true, nil, pub, true)
	if !strings.Contains(gone.fault(), "gone while plumb daemon 4242") {
		t.Errorf("a missing socket of a live daemon must be a fault, got %q", gone.fault())
	}

	// With no publication there is nothing to compare, so nothing is claimed —
	// even for a path that is present and owned by nobody.
	unpublished := socketPathStateFor(path, true, nil, socketPublication{}, false)
	if unpublished.fault() != "" {
		t.Errorf("a path no live daemon published must not be a fault, got %q", unpublished.fault())
	}

	// A publication names ONE path: another path must not inherit its publisher,
	// or a healthy account would be accused of a socket it never had.
	other := filepath.Join(dir, "plumb.ctrl.sock")
	unrelated := socketPathStateFor(other, true, nil, pub, true)
	if unrelated.Publisher != 0 || unrelated.fault() != "" {
		t.Errorf("the publication for %s made %s read as a live daemon's socket: %+v", path, other, unrelated)
	}
}

// TestDebugDaemonsExit_SocketFaultIsNotSuccess: exactly one daemon holding the
// authority, whose socket path has been replaced under it, is one daemon no client
// can reach — a script reading the exit status must not read that as success.
func TestDebugDaemonsExit_SocketFaultIsNotSuccess(t *testing.T) {
	healthy := []socketPathState{{Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 7, IdentityKnown: true, IdentityMatches: true}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, healthy); err != nil {
		t.Errorf("a clean account with an intact socket must be success, got %v", err)
	}
	// A note alone (a socket file nothing serves) is information, not a fault.
	noted := []socketPathState{{Path: "/run/plumb/plumb.sock", Exists: true, Answered: true}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, noted); err != nil {
		t.Errorf("a path with no live publisher must not fail the command, got %v", err)
	}
	faulted := []socketPathState{{Path: "/run/plumb/plumb.sock", Publisher: 7, IdentityKnown: true}}
	if err := debugDaemonsExit(daemons.Verdict{State: daemons.StateOK}, faulted); err == nil {
		t.Error("a live daemon whose socket path is gone must exit non-zero")
	}
}

// TestDaemonSocketResults is doctor's half: the same state becomes a failed row
// with a remedy, a warning, or nothing at all.
func TestDaemonSocketResults(t *testing.T) {
	faulted := daemonSocketResults([]socketPathState{{Path: "/run/plumb/plumb.sock", Publisher: 7, IdentityKnown: true}})
	if len(faulted) != 1 || faulted[0].ok || faulted[0].fix == "" {
		t.Fatalf("a socket fault must be one failed row with a fix, got %+v", faulted)
	}
	if !strings.Contains(faulted[0].detail, "gone while plumb daemon 7") {
		t.Errorf("the row must say what is wrong, got %q", faulted[0].detail)
	}

	noted := daemonSocketResults([]socketPathState{{Path: "/run/plumb/plumb.sock", Exists: true, Answered: true}})
	if len(noted) != 1 || !noted[0].ok || !noted[0].warn {
		t.Fatalf("a socket file nothing serves must be one warning row, got %+v", noted)
	}

	healthy := daemonSocketResults([]socketPathState{{Path: "/run/plumb/plumb.sock", Exists: true, Publisher: 7, IdentityKnown: true, IdentityMatches: true}})
	if len(healthy) != 0 {
		t.Errorf("a healthy socket must add no rows, got %+v", healthy)
	}
}
