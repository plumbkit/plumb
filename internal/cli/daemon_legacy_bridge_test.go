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
	"testing"
	"time"
)

// The legacy bridge (PLAN-549): the account's daemon answers an old client at a
// legacy MCP socket path whose lifetime lock it holds — and only the MCP socket,
// never a live foreign one, and never mistaken for a legacy daemon by a reader.

// bridgeFixture holds a legacy location's lifetime lock (as the daemon does) and
// binds the bridge for it, answering every connection with "ok".
type bridgeFixture struct {
	loc    legacyLocation
	bridge *legacyBridge
	wg     sync.WaitGroup
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
	f.bridge.bind(ctx, []heldLegacyLock{held}, daemonSocketPath())
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
	b.bind(context.Background(), []heldLegacyLock{held}, daemonSocketPath())
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
		t.Skipf("doctor's legacy path %s is not this account's legacy location %s here", legacyDaemonSocketPath(), f.loc.Socket)
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
