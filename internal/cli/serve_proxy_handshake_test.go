package cli

import (
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// serve_proxy_handshake_test.go covers replayHandshake — the reconnect path that
// re-establishes an MCP session on a fresh daemon connection. Split from
// serve_proxy_test.go, which had reached its size cap.
// replayHandshakeForTest drives a reconnect handshake against a fake daemon that
// answers initialize, and returns the frames the proxy wrote to the client.
// alreadyAnswered distinguishes a RECONNECT (the client already has its
// initialize result) from a first handshake that the daemon died in the middle
// of.
func replayHandshakeForTest(t *testing.T, alreadyAnswered bool) ([]string, bool) {
	t.Helper()
	outR, outW := io.Pipe()
	clientOut := newFrameReader(outR)

	daemonSide, proxySide := net.Pipe()
	t.Cleanup(func() { _ = daemonSide.Close(); _ = proxySide.Close(); _ = outW.Close() })
	p := newReconnectingProxy(proxyDeps{out: outW, handshakeWait: 5 * time.Second})
	p.initializeFrame = []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	p.initializeID = idKey([]byte(`1`))
	p.initializeAnswered = alreadyAnswered

	// The fake daemon reads the replayed initialize and answers it.
	go func() {
		fr := newFrameReader(daemonSide)
		_, _ = fr.read()
		_ = writeFrame(daemonSide, []byte(
			`{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"plumb","version":"0.16.4"}}}`))
	}()

	frames := make(chan string, 4)
	go func() {
		for {
			b, err := clientOut.read()
			if err != nil {
				return
			}
			frames <- string(b)
		}
	}()

	if _, err := p.replayHandshake(proxySide); err != nil {
		t.Fatalf("replayHandshake: %v", err)
	}

	var got []string
	for {
		select {
		case f := <-frames:
			got = append(got, f)
		case <-time.After(100 * time.Millisecond):
			// The handshake has returned, so anything it was going to write is
			// already queued; this is a short drain, not a timeout to wait out.
			return got, p.relistOnReconnect.Load()
		}
	}
}

// TestReplayHandshake_ReconnectTellsClientToRelistTools is the fix for a tool
// added by a daemon rebuild staying invisible.
//
// A restart swaps the daemon out, so the connection that would have fired
// tools/list_changed no longer exists — while the client's view of the server,
// this proxy, persists across the gap and therefore never re-lists. A tool the
// rebuilt daemon gained stays unusable until the CLIENT is restarted, which is
// exactly backwards for a proxy whose whole purpose is that the daemon can be
// rebuilt under a live session.
func TestReplayHandshake_ReconnectTellsClientToRelistTools(t *testing.T) {
	t.Parallel()
	got, relist := replayHandshakeForTest(t, true)

	for _, f := range got {
		if strings.Contains(f, `"result"`) {
			t.Errorf("a reconnect must not re-send the initialize result the client already has: %s", f)
		}
	}
	// The notification itself is emitted at the end of reconnect(), once the
	// in-flight requests have been answered — the handshake only arms it, so that
	// a client waiting on its own response does not have to read past an
	// unrelated notification first.
	if !relist {
		t.Fatal("a reconnect must arm the tools/list_changed notification")
	}
}

// TestReplayHandshake_FirstHandshakeForwardsResultAndDoesNotNotify: when the
// daemon died before the client ever got its initialize result, the replayed
// result is forwarded instead. The client is only now completing its handshake
// and will list tools as a matter of course, so a change notification would be
// noise about a list it has not yet fetched.
func TestReplayHandshake_FirstHandshakeForwardsResultAndDoesNotNotify(t *testing.T) {
	t.Parallel()
	got, relist := replayHandshakeForTest(t, false)

	var sawResult bool
	for _, f := range got {
		if strings.Contains(f, `"result"`) {
			sawResult = true
		}
	}
	if relist {
		t.Error("a first handshake must not arm the notification — the client lists tools anyway")
	}
	if !sawResult {
		t.Fatalf("a client that never received its initialize result must get the replayed one; frames = %v", got)
	}
}

// TestProxy_ReconnectNotifiesClientToRelistTools is the end-to-end guard, and it
// exists because the flag-level test above is not one.
//
// An independent review deleted the emission block in reconnect() and the entire
// suite still passed: the other test asserts that the flag is ARMED, which is an
// implementation detail, while the behaviour the change exists for — a frame
// actually reaching the client — had no coverage at all. This drives a real
// daemon crash, a real reconnect, and reads the real wire.
func TestProxy_ReconnectNotifiesClientToRelistTools(t *testing.T) {
	t.Parallel()

	_, initialProxySide := newPipeDaemon(func(m *mockDaemon) { m.crashOnTool = true })
	h := startProxy(t, initialProxySide, 0, 0)
	_, replacement := newPipeDaemon(nil)
	h.dialQueue <- replacement
	h.start()
	h.handshake()
	h.write(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`) // kills the daemon

	// readAny, not read: read() deliberately skips notifications, which is the
	// very frame under test here.
	deadline := time.Now().Add(10 * time.Second)
	for range 5 {
		if strings.Contains(h.readAny(time.Until(deadline)), `"method":"notifications/tools/list_changed"`) {
			return
		}
	}
	t.Fatal("no tools/list_changed reached the client after a reconnect — a tool added by " +
		"the rebuilt daemon would stay invisible until the client itself restarted")
}
