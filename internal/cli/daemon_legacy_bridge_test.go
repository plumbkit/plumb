package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The legacy bridge (PLAN-549): the account's daemon answers an old client at a
// legacy MCP socket path whose lifetime lock it holds — and only the MCP socket,
// never a live foreign one, and never mistaken for a legacy daemon by a reader.

// bridgeFixture holds a legacy location's lifetime lock (as the daemon does) and
// binds the bridge for it, answering every connection with "ok".
type bridgeFixture struct {
	loc         legacyLocation
	bridge      *legacyBridge
	wg          sync.WaitGroup
	republished atomic.Int64
}

func newBridgeFixture(t *testing.T) *bridgeFixture {
	t.Helper()
	withShortTempRuntime(t)
	withAccountAuthority(t)
	f := &bridgeFixture{loc: legacyLocationForTest(t), bridge: newLegacyBridge()}
	held, err := acquireLegacyLock(f.loc.Lock)
	if err != nil {
		t.Fatalf("acquireLegacyLock: %v", err)
	}
	t.Cleanup(func() { _ = held.File.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.bridge.bind(ctx, []heldLegacyLock{held}, daemonSocketPath(), func() { f.republished.Add(1) })
	f.bridge.serve(&f.wg, func(c net.Conn) {
		_, _ = c.Write([]byte("ok"))
		_ = c.Close()
	})
	t.Cleanup(func() { f.bridge.close(); f.wg.Wait() })
	return f
}

// dialBridge dials path and returns what the server wrote.
func dialBridge(t *testing.T, path string) string {
	t.Helper()
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("dialling %s: %v", path, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got, _ := io.ReadAll(conn)
	return string(got)
}

func TestLegacyBridge_AnswersAnOldClientAtTheHeldLegacySocket(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newBridgeFixture(t)
	if got := dialBridge(t, f.loc.Socket); got != "ok" {
		t.Fatalf("an old client at %s was not answered by the account's daemon: %q", f.loc.Socket, got)
	}
	_ = dialBridge(t, f.loc.Socket)

	recs := f.bridge.records()
	if len(recs) != 1 || recs[0].Path != f.loc.Socket || recs[0].Ino == 0 || recs[0].FirstClient.IsZero() {
		t.Fatalf("the bridge must publish its path, identity and first use: %+v", recs)
	}
	// Logged once per path, with the remedy, however many clients come.
	if n := strings.Count(logs.String(), "connected through a legacy socket path"); n != 1 {
		t.Errorf("the first client through the bridge was logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "restart that client") {
		t.Errorf("the log must name the remedy:\n%s", logs.String())
	}
}

// TestLegacyBridge_NeverServesTheLegacyControlSocket is the negative control the
// design names: serving the control plane at a legacy path would let an old CLI
// command the account's daemon.
func TestLegacyBridge_NeverServesTheLegacyControlSocket(t *testing.T) {
	f := newBridgeFixture(t)
	if _, err := os.Lstat(f.loc.Ctrl); !os.IsNotExist(err) {
		t.Fatalf("the bridge created the legacy control socket %s (stat err %v)", f.loc.Ctrl, err)
	}
	for _, r := range f.bridge.records() {
		if r.Path == f.loc.Ctrl {
			t.Fatalf("the bridge publishes the legacy control socket: %+v", r)
		}
	}
}

// TestLegacyBridge_NeverDisplacesALiveForeignSocket: PLAN-529's rule, unchanged —
// a path somebody serves is reported and left alone, never removed or adopted.
func TestLegacyBridge_NeverDisplacesALiveForeignSocket(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	foreign := countingListener(t, loc.Socket)
	before, err := os.Lstat(loc.Socket)
	if err != nil {
		t.Fatal(err)
	}
	held, err := acquireLegacyLock(loc.Lock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.File.Close() }()

	b := newLegacyBridge()
	b.bind(context.Background(), []heldLegacyLock{held}, daemonSocketPath(), nil)
	defer b.close()
	if len(b.records()) != 0 {
		t.Fatalf("the bridge bound over a live foreign socket: %+v", b.records())
	}
	after, err := os.Lstat(loc.Socket)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the foreign socket at %s was replaced or removed (err %v)", loc.Socket, err)
	}
	conn, err := net.DialTimeout("unix", loc.Socket, time.Second)
	if err != nil {
		t.Fatalf("the foreign listener stopped answering: %v", err)
	}
	_ = conn.Close()
	_ = foreign
}

// TestLegacyBridge_LeavesAReplacedPathAlone: shutdown removes only the file the
// bridge bound — identity, exactly like the canonical socket.
func TestLegacyBridge_LeavesAReplacedPathAlone(t *testing.T) {
	f := newBridgeFixture(t)
	if err := os.Remove(f.loc.Socket); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.loc.Socket, []byte("somebody else's"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.bridge.close()
	if data, err := os.ReadFile(f.loc.Socket); err != nil || string(data) != "somebody else's" {
		t.Fatalf("shutdown removed a file the bridge did not create (err %v)", err)
	}
}

// TestLegacyBridge_RemovesItsOwnSocketOnClose is the control for the test above.
func TestLegacyBridge_RemovesItsOwnSocketOnClose(t *testing.T) {
	f := newBridgeFixture(t)
	f.bridge.close()
	if _, err := os.Lstat(f.loc.Socket); !os.IsNotExist(err) {
		t.Fatalf("the bridge left its own socket behind (stat err %v)", err)
	}
}

// publishBridges writes the authority record a live daemon publishes, naming the
// bridge's sockets — so the readers can be asked whether a socket is ours.
func publishBridges(t *testing.T, pid int, recs []bridgeRecord) {
	t.Helper()
	f, err := acquireAuthority()
	if err != nil {
		t.Fatalf("acquireAuthority: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if err := publishAuthority(f, authorityMetadata{PID: pid, Socket: daemonSocketPath(), Bridges: recs}); err != nil {
		t.Fatalf("publishAuthority: %v", err)
	}
}

// TestLegacyProbe_DoesNotReadOurBridgeAsALegacyDaemon is the census-honesty half:
// the account's daemon answering an old client is ONE daemon, not a legacy one —
// otherwise a new client would be refused with the migration instruction while
// the account's daemon serves it.
func TestLegacyProbe_DoesNotReadOurBridgeAsALegacyDaemon(t *testing.T) {
	f := newBridgeFixture(t)
	if got := probeLegacyDaemons(); len(got) != 1 {
		t.Fatalf("control: with no record naming it, the bridge reads as a legacy daemon: %v", got)
	}
	publishBridges(t, os.Getpid(), f.bridge.records())
	if !ourBridgeAt(f.loc.Socket) {
		t.Fatal("ourBridgeAt does not recognise the live daemon's published bridge")
	}
	if got := probeLegacyDaemons(); len(got) != 0 {
		t.Fatalf("the legacy probe read the account's own bridge as a legacy daemon: %+v", got)
	}
	if conn, err := attachLegacyDaemon(); conn != nil || err != nil {
		t.Fatalf("a client was attached to or refused by the account's own bridge: conn=%v err=%v", conn, err)
	}
}

func TestOurBridgeAt_RequiresTheIdentityAndALivePublisher(t *testing.T) {
	f := newBridgeFixture(t)
	recs := f.bridge.records()

	wrong := []bridgeRecord{{Path: recs[0].Path, Dev: recs[0].Dev, Ino: recs[0].Ino + 1}}
	publishBridges(t, os.Getpid(), wrong)
	if ourBridgeAt(f.loc.Socket) {
		t.Error("a record naming a different inode claimed the socket at the path")
	}
}

func TestOurBridgeAt_RefusesADeadPublisher(t *testing.T) {
	f := newBridgeFixture(t)
	publishBridges(t, exitedPID(t), f.bridge.records())
	if ourBridgeAt(f.loc.Socket) {
		t.Error("a dead publisher's record claimed the socket at the path")
	}
}

func TestRenderLegacyBridges(t *testing.T) {
	unused := renderLegacyBridges([]bridgeRecord{{Path: "/old/plumb.sock"}})
	if !strings.Contains(unused, "/old/plumb.sock") || !strings.Contains(unused, "no client has used it") {
		t.Errorf("an unused bridge must be shown as unused: %q", unused)
	}
	used := renderLegacyBridges([]bridgeRecord{{Path: "/old/plumb.sock", FirstClient: time.Now()}})
	if !strings.Contains(used, "restart that client") {
		t.Errorf("a used bridge must name the remedy: %q", used)
	}
}

// TestCheckDaemon_DoesNotPointAtOurOwnBridge: doctor's "a daemon is running at
// <legacy path>" hint must not name the account's own bridge as a stray daemon.
func TestCheckDaemon_DoesNotPointAtOurOwnBridge(t *testing.T) {
	f := newBridgeFixture(t)
	if legacyDaemonSocketPath() != f.loc.Socket {
		t.Fatalf("doctor's legacy path %s is not this account's legacy location %s: the test would prove nothing", legacyDaemonSocketPath(), f.loc.Socket)
	}
	hint := func() string {
		var b strings.Builder
		for _, r := range checkDaemon() {
			b.WriteString(r.detail)
		}
		return b.String()
	}
	if !strings.Contains(hint(), "a daemon is running at") {
		t.Fatalf("control: without a record the bridge reads as a stray daemon to doctor: %q", hint())
	}
	publishBridges(t, os.Getpid(), f.bridge.records())
	if h := hint(); strings.Contains(h, "a daemon is running at") {
		t.Fatalf("doctor named the account's own bridge as a stray daemon: %q", h)
	}
}

// exitedPID returns the pid of a process that has exited and been reaped.
func exitedPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running the stand-in: %v", err)
	}
	return cmd.Process.Pid
}

// TestLegacyBridge_PublishesAtOnce: a bind, a re-bind and a first client each ask
// for the authority record to be rewritten now, so a reader never sees the
// account's own bridge as a legacy daemon for a tick (PLAN-549 review).
func TestLegacyBridge_PublishesAtOnce(t *testing.T) {
	f := newBridgeFixture(t)
	if f.republished.Load() < 1 {
		t.Fatal("binding the bridge did not republish the authority record")
	}
	after := f.republished.Load()
	_ = dialBridge(t, f.loc.Socket)
	if f.republished.Load() <= after {
		t.Fatal("the first client through the bridge did not republish the record")
	}
	// A deleted bridge path is re-bound by its guard, and the new identity is
	// published straight away.
	after = f.republished.Load()
	before := f.bridge.records()[0]
	if err := os.Remove(f.loc.Socket); err != nil {
		t.Fatal(err)
	}
	f.bridge.sockets[0].guard.check()
	deadline := time.Now().Add(5 * time.Second)
	for f.republished.Load() <= after {
		if time.Now().After(deadline) {
			t.Fatal("a re-bound bridge socket was not republished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := f.bridge.records()[0]; got.Ino == before.Ino && got.Ctime == before.Ctime {
		t.Fatalf("the record still names the deleted socket: %+v", got)
	}
	if got := dialBridge(t, f.loc.Socket); got != "ok" {
		t.Fatalf("the re-bound bridge socket does not answer: %q", got)
	}
}

// TestLegacyBridge_RefusesADirectoryOthersCanWrite: a legacy location comes from
// a client's environment, so the bridge listens only in a 0700 directory this
// user owns — the canonical runtime base's own rule — and its socket is 0600.
func TestLegacyBridge_RefusesADirectoryOthersCanWrite(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	held, err := acquireLegacyLock(loc.Lock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.File.Close() }()
	if err := os.Chmod(loc.Dir, 0o777); err != nil { //nolint:gosec // G302: the unsafe mode IS the case under test
		t.Fatal(err)
	}
	b := newLegacyBridge()
	b.bind(context.Background(), []heldLegacyLock{held}, daemonSocketPath(), nil)
	defer b.close()
	if len(b.records()) != 0 {
		t.Fatalf("the bridge listens in a world-writable directory: %+v", b.records())
	}
}

func TestLegacyBridge_SocketIsPrivate(t *testing.T) {
	f := newBridgeFixture(t)
	info, err := os.Stat(f.loc.Socket)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("the bridge socket is mode %04o, want 0600", perm)
	}
}

// TestServeConnections_DrainsAndClosesTheBridge pins the shared lifecycle: a
// connection through the bridge is handled by the SAME handler and joins the
// SAME drain as the account socket's, and shutdown closes the bridge.
func TestServeConnections_DrainsAndClosesTheBridge(t *testing.T) {
	withShortTempRuntime(t)
	withAccountAuthority(t)
	loc := legacyLocationForTest(t)
	held, err := acquireLegacyLock(loc.Lock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.File.Close() }()
	guard, err := newSocketGuard(daemonSocketPath(), func() bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bridge := newLegacyBridge()
	bridge.bind(ctx, []heldLegacyLock{held}, daemonSocketPath(), nil)

	inHandler := make(chan struct{}, 1)
	release := make(chan struct{})
	var finished atomic.Bool
	handle := func(c net.Conn) {
		inHandler <- struct{}{}
		<-release
		finished.Store(true)
		_ = c.Close()
	}
	done := make(chan struct{})
	go func() { defer close(done); serveConnections(ctx, guard, bridge, handle, func() {}) }()

	conn, err := net.DialTimeout("unix", loc.Socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	select {
	case <-inHandler:
	case <-time.After(5 * time.Second):
		t.Fatal("a bridge connection never reached the daemon's handler")
	}
	cancel()
	// Shutdown closes the bridge: nothing answers there any more.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := net.DialTimeout("unix", loc.Socket, 200*time.Millisecond)
		if err != nil {
			break
		}
		_ = c.Close()
		if time.Now().After(deadline) {
			t.Fatal("the bridge still accepts connections after shutdown began")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The drain waits for the bridge connection in flight.
	select {
	case <-done:
		t.Fatal("serveConnections returned while a bridge connection was still in its handler: it was not in the drain")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serveConnections did not return after the drain")
	}
	if !finished.Load() {
		t.Fatal("the handler had not finished when serveConnections returned")
	}
}

// TestLiveLegacyBridges_ReportsOnlyOurLiveBridge: `plumb debug daemons` lists a
// bridge only while the file at its path is the one the live daemon published.
func TestLiveLegacyBridges_ReportsOnlyOurLiveBridge(t *testing.T) {
	f := newBridgeFixture(t)
	publishBridges(t, os.Getpid(), f.bridge.records())
	if got := liveLegacyBridges(); len(got) != 1 || got[0].Path != f.loc.Socket {
		t.Fatalf("liveLegacyBridges = %+v, want the live bridge", got)
	}
	if !strings.Contains(renderLegacyBridges(liveLegacyBridges()), f.loc.Socket) {
		t.Fatal("the census output does not name the bridge")
	}
	if err := os.Remove(f.loc.Socket); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.loc.Socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := liveLegacyBridges(); len(got) != 0 {
		t.Fatalf("a replaced bridge path is still reported as ours: %+v", got)
	}
}

func TestOurBridgeAt_RefusesAStaleRecord(t *testing.T) {
	f := newBridgeFixture(t)
	fa, err := acquireAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fa.Close() }()
	m := authorityMetadata{
		PID: os.Getpid(), Socket: daemonSocketPath(), Bridges: f.bridge.records(),
		Published: time.Now().Add(-2 * daemonSocketPublicationFreshness),
	}
	if err := publishAuthority(fa, m); err != nil {
		t.Fatal(err)
	}
	if ourBridgeAt(f.loc.Socket) {
		t.Fatal("a record nobody refreshed within the freshness window claimed the socket")
	}
}

// TestRunDebugDaemons_ReportsTheBridge: `plumb debug daemons` itself prints the
// live bridge — the sunset evidence — not only the renderer in isolation.
func TestRunDebugDaemons_ReportsTheBridge(t *testing.T) {
	f := newBridgeFixture(t)
	publishBridges(t, os.Getpid(), f.bridge.records())
	var out bytes.Buffer
	_ = runDebugDaemons(&out)
	if !strings.Contains(out.String(), "legacy bridge "+f.loc.Socket) {
		t.Fatalf("plumb debug daemons does not report the live bridge:\n%s", out.String())
	}
}

// TestOurBridgeAt_RequiresTheChangeTime: the identity is (dev, ino, ctime) — a
// record whose change time is not the file's does not claim it, as the reader
// rule (fileIdentity.matches) demands.
func TestOurBridgeAt_RequiresTheChangeTime(t *testing.T) {
	f := newBridgeFixture(t)
	recs := f.bridge.records()
	recs[0].Ctime++
	publishBridges(t, os.Getpid(), recs)
	if ourBridgeAt(f.loc.Socket) {
		t.Fatal("a record with a different change time claimed the socket at the path")
	}
}

// TestCtrlSocketUnavailable_DoesNotPointAtOurOwnBridge: `plumb log-level`'s hint
// must not name the account's own bridge as a daemon running elsewhere.
func TestCtrlSocketUnavailable_DoesNotPointAtOurOwnBridge(t *testing.T) {
	f := newBridgeFixture(t)
	if legacyDaemonSocketPath() != f.loc.Socket {
		t.Fatalf("log-level's legacy path %s is not this account's legacy location %s", legacyDaemonSocketPath(), f.loc.Socket)
	}
	if !strings.Contains(ctrlSocketUnavailable(io.EOF).Error(), "IS running") {
		t.Fatal("control: without a record the bridge reads as a daemon running elsewhere")
	}
	publishBridges(t, os.Getpid(), f.bridge.records())
	if msg := ctrlSocketUnavailable(io.EOF).Error(); strings.Contains(msg, "IS running") {
		t.Fatalf("log-level named the account's own bridge as a daemon running elsewhere: %s", msg)
	}
}

// TestLegacyBridge_CloseWaitsForItsAcceptLoops pins that close does not return
// while a bridge accept loop is still running: the daemon closes the bridge and
// then starts its drain, and a loop still able to accept could add a connection
// to the group after the drain began. Each loop's exit is held until the test
// releases it, so a close that does not wait is caught deterministically.
func TestLegacyBridge_CloseWaitsForItsAcceptLoops(t *testing.T) {
	f := newBridgeFixture(t)
	release := make(chan struct{})
	old := bridgeLoopExited
	t.Cleanup(func() { bridgeLoopExited = old })
	bridgeLoopExited = func() { <-release }

	closed := make(chan struct{})
	go func() { f.bridge.close(); close(closed) }()
	select {
	case <-closed:
		close(release)
		t.Fatal("close returned while a bridge accept loop had not exited")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not return once the accept loops exited")
	}
}

// TestSocketGuard_RepublishSkipsAClosedGuard: once the account socket's guard is
// closed (shutdown), a late bridge change must not rewrite the authority record.
func TestSocketGuard_RepublishSkipsAClosedGuard(t *testing.T) {
	withShortTempRuntime(t)
	var published atomic.Int64
	guard, err := newSocketGuard(daemonSocketPath(), func() bool { return true }, func(string, fileIdentity) { published.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer guard.removeSocketIfOurs("test socket")
	guard.republish()
	open := published.Load()
	if open < 2 {
		t.Fatalf("control: republish on an open guard did not publish (count %d)", open)
	}
	guard.close()
	guard.republish()
	if got := published.Load(); got != open {
		t.Fatalf("a closed guard republished the record (count %d -> %d)", open, got)
	}
}
