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
		case <-time.After(500 * time.Millisecond):
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
