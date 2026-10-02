package cli

// serve_resume_e2e_test.go — the serve-replacement story end to end (#556), through the
// REAL proxy: a reconnectingProxy in front of an in-process daemon (a real mcp.Server and
// connSession per connection, every tool registered, a shared durable session-state
// store). Nothing in the proxy is stubbed: the credential travels daemon → proxy → disk
// → a different proxy process → daemon, and the assertions are on what the client sees.
//
// The flow: establish; take the credential (it never reaches the client); kill the serve
// and its daemon the way a reboot does; start a replacement serve with a new proxy
// credential; it presents at session_start; the SAME internal session ID, name, mailbox
// binding and thread seat come back. Then again, so the successor each hop stored is
// proven to be the one the next hop presents.
//
// What this cannot cover is the OS process boundary: both serves live in this test
// process, over net.Pipe. cmd/smoke (integration tag) is the version over real binaries.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
)

// e2eServe is a serve proxy with a client attached, and the daemon connections it has
// made, in one process.
type e2eServe struct {
	t         *testing.T
	w         *identityWorld
	oldDaemon bool
	cancel    context.CancelFunc
	runDone   chan struct{}
	clientIn  *io.PipeWriter
	frames    chan string

	mu     sync.Mutex
	seen   []string // every frame the client received
	conns  []*identityConn
	nextID int
}

// startServe starts a proxy as `plumb serve` would build it: a fresh proxy session ID, the
// given resume store, and a dial hook that connects to a fresh in-process daemon
// connection. oldDaemon builds those connections without the consumer hook, which is what
// a daemon that predates the credential does with the announcement: nothing.
func (w *identityWorld) startServe(t *testing.T, store *resumeStore, proxyID string, oldDaemon bool) *e2eServe {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	sv := &e2eServe{
		t: t, w: w, oldDaemon: oldDaemon, cancel: cancel, runDone: make(chan struct{}), clientIn: inW,
		frames: make(chan string, 256), nextID: 1000,
	}
	go func() {
		fr := newFrameReader(outR)
		for {
			b, err := fr.read()
			if err != nil {
				close(sv.frames)
				return
			}
			sv.frames <- string(b)
		}
	}()
	p := newReconnectingProxy(proxyDeps{
		in:             inR,
		out:            outW,
		initial:        sv.newDaemonConn(),
		dial:           func(context.Context) (net.Conn, error) { return sv.newDaemonConn(), nil },
		proxySessionID: proxyID,
		resumeStore:    store,
		maxReconnects:  3,
		baseBackoff:    time.Millisecond,
		handshakeWait:  10 * time.Second,
	})
	go func() {
		_ = p.run(ctx)
		_ = outW.Close()
		close(sv.runDone)
	}()
	t.Cleanup(sv.kill)
	return sv
}

// newDaemonConn is a connection to a fresh in-process daemon session: the proxy's end.
func (sv *e2eServe) newDaemonConn() net.Conn {
	proxySide, daemonSide := net.Pipe()
	ic := sv.w.conn("")
	if sv.oldDaemon {
		ic.srv.OnResumeCredentialConsumer = nil
	}
	sv.mu.Lock()
	sv.conns = append(sv.conns, ic)
	sv.mu.Unlock()
	go func() {
		_ = ic.srv.Serve(ic.s.ctx, daemonSide, daemonSide)
		_ = daemonSide.Close()
	}()
	return proxySide
}

// kill ends the serve and its daemon together, the way a reboot does: no graceful handover.
func (sv *e2eServe) kill() {
	sv.cancel()
	_ = sv.clientIn.Close()
	select {
	case <-sv.runDone:
	case <-time.After(10 * time.Second):
		sv.t.Error("the proxy did not stop")
	}
	sv.mu.Lock()
	conns := append([]*identityConn(nil), sv.conns...)
	sv.mu.Unlock()
	for _, ic := range conns {
		ic.s.close()
	}
}

func (sv *e2eServe) read(id int) string {
	sv.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case f, ok := <-sv.frames:
			if !ok {
				sv.t.Fatal("the proxy closed the client stream")
			}
			sv.mu.Lock()
			sv.seen = append(sv.seen, f)
			sv.mu.Unlock()
			if e := parseEnvelope([]byte(f)); e.isResponse() && idKey(e.ID) == itoa(id) {
				return f
			}
		case <-deadline:
			sv.t.Fatalf("no response to request %d", id)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// initialize performs the client's MCP handshake.
func (sv *e2eServe) initialize() {
	sv.t.Helper()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{
		"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "e2e", "version": "0"},
	}})
	if err := writeFrame(sv.clientIn, req); err != nil {
		sv.t.Fatal(err)
	}
	sv.read(1)
	if err := writeFrame(sv.clientIn, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		sv.t.Fatal(err)
	}
}

// call is a tools/call stamped as the conversation (what the Claude Code hook does), with
// the text a model sees and the `_meta` only a proxy reads.
func (sv *e2eServe) call(agent, tool string, args map[string]any) (text string, meta map[string]json.RawMessage) {
	sv.t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	if agent != "" {
		args[mcp.ArgLogicalAgentKey] = agent
	}
	sv.mu.Lock()
	sv.nextID++
	id := sv.nextID
	sv.mu.Unlock()
	if err := writeFrame(sv.clientIn, []byte(rcRequest(sv.t, id, tool, args, nil))); err != nil {
		sv.t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool                       `json:"isError"`
			Meta    map[string]json.RawMessage `json:"_meta"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(sv.read(id)), &resp); err != nil {
		sv.t.Fatal(err)
	}
	if resp.Error != nil {
		sv.t.Fatalf("%s: %s", tool, resp.Error.Message)
	}
	if len(resp.Result.Content) > 0 {
		text = resp.Result.Content[0].Text
	}
	if resp.Result.IsError {
		sv.t.Fatalf("%s failed: %s", tool, text)
	}
	return text, resp.Result.Meta
}

// start is session_start naming the conversation and workspace; it returns the internal
// session ID the daemon echoed, the name from the packet, and the packet.
func (sv *e2eServe) start(conv, workspace string) (id, name, packet string) {
	sv.t.Helper()
	packet, meta := sv.call(conv, "session_start", map[string]any{"session_id": conv, "workspace": workspace})
	if err := json.Unmarshal(meta[mcp.MetaSessionIDKey], &id); err != nil || id == "" {
		sv.t.Fatalf("session_start echoed no internal session ID; _meta %v", meta)
	}
	name, _, _ = strings.Cut(strings.TrimSpace(strings.TrimPrefix(sessionLine(packet), "Session:")), " (you")
	return id, name, packet
}

// assertNeverSaw fails when any frame the client received carried the credential key or
// a credential-shaped token. Its positive control runs first.
func (sv *e2eServe) assertNeverSaw(secrets ...string) {
	sv.t.Helper()
	if !leakScanWorks() {
		sv.t.Fatal("the leak scan does not fire on a credential")
	}
	sv.mu.Lock()
	all := strings.Join(sv.seen, "\n")
	sv.mu.Unlock()
	if strings.Contains(all, mcp.MetaResumeCredentialKey) {
		sv.t.Errorf("a frame the client received names %s", mcp.MetaResumeCredentialKey)
	}
	assertNoSecret(sv.t, "the frames the client received", all, secrets...)
}

const e2eConv = "conv-e2e"

// e2eEstablish runs a first serve: it links the conversation, builds up the state a
// replacement must get back (mail, a thread the conversation has replied in, and a note
// still unread), and is then killed.
type e2eFirst struct {
	id, name, threadID string
	peer               *identityConn
	ws                 string
}

func e2eEstablish(t *testing.T, w *identityWorld, store *resumeStore) e2eFirst {
	t.Helper()
	f := e2eFirst{ws: identityRepo(t), peer: w.conn("")}
	f.peer.call("", "session_start", map[string]any{"workspace": f.ws})

	s1 := w.startServe(t, store, "proxy-1", false)
	s1.initialize()
	f.id, f.name, _ = s1.start(e2eConv, f.ws)

	f.peer.call("", "leave_note", map[string]any{"to": f.name, "body": "hello"})
	out, _ := s1.call(e2eConv, "check_messages", nil)
	f.threadID = conversationIDOf(t, out)
	if reply, _ := s1.call(e2eConv, "leave_note", map[string]any{"conversation_id": f.threadID, "body": "reply from the original"}); strings.Contains(reply, "not one of yours") {
		t.Fatalf("precondition: the original cannot reply in its own thread: %s", reply)
	}
	f.peer.call("", "leave_note", map[string]any{"to": f.name, "body": credBoundNote})
	s1.assertNeverSaw()
	s1.kill()
	return f
}

// TestServeReplacement_RestoresTheFullIdentity is #556's end-to-end claim: after a serve
// replacement the conversation keeps its internal session ID, its name, the mail bound to
// the ID and its thread seat, because the replacement presented the credential its
// predecessor's proxy stored. Two hops, so the successor each hop stored is the one the
// next presents.
func TestServeReplacement_RestoresTheFullIdentity(t *testing.T) {
	w := newIdentityWorld(t).withState()
	dir, scope := t.TempDir(), "scope-e2e"
	store := rcStore(t, dir, scope)

	first := e2eEstablish(t, w, store)
	e1, ok := store.load(e2eConv)
	if !ok {
		t.Fatal("the first serve stored no credential: the daemon disclosed none, or the proxy dropped it " +
			"(did it announce that it consumes the key?)")
	}
	secrets := []string{e1.Secret}

	var lastID string
	unread := credBoundNote // the note sent to the conversation while no serve was attached
	for hop := 1; hop <= 2; hop++ {
		s := w.startServe(t, rcStore(t, dir, scope), fmt.Sprintf("proxy-%d", hop+1), false)
		s.initialize()
		id, name, packet := s.start(e2eConv, first.ws)

		if id != first.id {
			t.Fatalf("hop %d: the replacement serve came back as session %q, want the predecessor's %q: "+
				"the credential was not presented, or not accepted", hop, id, first.id)
		}
		if name != first.name {
			t.Errorf("hop %d: name = %q, want %q", hop, name, first.name)
		}
		if line := sessionLine(packet); !strings.Contains(line, "identity restored") {
			t.Errorf("hop %d: the packet does not report the full restore: %q", hop, line)
		}
		out, _ := s.call(e2eConv, "check_messages", nil)
		if !strings.Contains(packet+out, unread) {
			t.Errorf("hop %d: mail bound to the predecessor's ID did not come back:\n%s\n%s", hop, packet, out)
		}
		if reply, _ := s.call(e2eConv, "leave_note", map[string]any{"conversation_id": first.threadID, "body": "back"}); strings.Contains(reply, "not one of yours") {
			t.Errorf("hop %d: the thread seat did not come back: %q", hop, reply)
		}

		// The credential rotated, and the proxy kept the successor for the next hop.
		cur, ok := store.load(e2eConv)
		if !ok || cur.Secret == secrets[len(secrets)-1] || cur.Generation != hop+1 {
			t.Fatalf("hop %d: store = %+v (found %v); want a successor of generation %d, not the credential just spent", hop, cur, ok, hop+1)
		}
		secrets = append(secrets, cur.Secret)
		s.assertNeverSaw(secrets...)
		// Mail arrives while no serve is attached, for the next hop to find.
		unread = fmt.Sprintf("note while the hop %d serve was gone", hop)
		first.peer.call("", "leave_note", map[string]any{"to": first.name, "body": unread})
		s.kill()
		lastID = id
	}
	if lastID != first.id {
		t.Fatalf("the chain ended on session %q, want %q", lastID, first.id)
	}
}

// The control that gives the test its meaning: a replacement serve with no stored credential
// is what it was before the credential existed, a name-only resume that forks the identity.
// And a store that belongs to a different daemon presents nothing.
func TestServeReplacement_WithoutTheStoredCredentialStaysNameOnly(t *testing.T) {
	for name, build := range map[string]func(t *testing.T, dir string) *resumeStore{
		"an empty store": func(t *testing.T, _ string) *resumeStore { t.Helper(); return rcStore(t, t.TempDir(), "scope-e2e") },
		"a store bound to a different daemon": func(t *testing.T, dir string) *resumeStore {
			t.Helper()
			return rcStore(t, dir, "scope-another-daemon")
		},
		"a proxy that cannot keep a credential": func(*testing.T, string) *resumeStore { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			w := newIdentityWorld(t).withState()
			dir := t.TempDir()
			first := e2eEstablish(t, w, rcStore(t, dir, "scope-e2e"))

			s := w.startServe(t, build(t, dir), "proxy-2", false)
			s.initialize()
			id, nm, packet := s.start(e2eConv, first.ws)
			if id == first.id {
				t.Fatalf("the replacement serve resumed the internal session ID with no credential to present (D1)")
			}
			if nm != first.name {
				t.Errorf("name = %q, want the name-only resume %q", nm, first.name)
			}
			if out, _ := s.call(e2eConv, "check_messages", nil); strings.Contains(packet+out, credBoundNote) {
				t.Errorf("a name resume read mail bound to the predecessor:\n%s", out)
			}
			s.assertNeverSaw()
		})
	}
}

// A daemon that predates the credential ignores the announcement and the presentation: the
// replacement behaves exactly as it did, nothing is stored, nothing leaks.
func TestServeReplacement_AgainstAnOldDaemon(t *testing.T) {
	w := newIdentityWorld(t).withState()
	store := rcStore(t, t.TempDir(), "scope-e2e")
	ws, peer := identityRepo(t), w.conn("")
	peer.call("", "session_start", map[string]any{"workspace": ws})

	s1 := w.startServe(t, store, "proxy-1", true)
	s1.initialize()
	id1, name1, _ := s1.start(e2eConv, ws)
	if store.count() != 0 {
		t.Fatalf("the store holds %d entries from a daemon that disclosed nothing", store.count())
	}
	s1.kill()

	// Even given a credential from some earlier, newer daemon, an old one is not broken by it.
	if _, err := store.put(e2eConv, rcSecret(5)); err != nil {
		t.Fatal(err)
	}
	s2 := w.startServe(t, store, "proxy-2", true)
	s2.initialize()
	id2, name2, _ := s2.start(e2eConv, ws)
	if id2 == id1 || name2 != name1 {
		t.Errorf("an old daemon: id %q→%q name %q→%q; want today's behaviour, the name back under a new ID", id1, id2, name1, name2)
	}
	s2.assertNeverSaw(rcSecret(5))
}
