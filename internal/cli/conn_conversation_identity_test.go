package cli

// conn_conversation_identity_test.go — one identity per conversation on a shared
// connection, and Claude Code's /clear handing the connection over (#564).
//
// Two conversations that share a `plumb serve` connection (Claude desktop's Code
// tab) used to be one identity: the second one's session_start relinked the
// connection, and it took the first one's linkage, name and mail. A stamped second
// conversation now keeps its own; a /clear marker, sent by the SessionStart hook,
// is the one thing that hands the connection to a new conversation id.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	convA   = "conv-A"
	convB   = "conv-B"
	convOld = "conv-OLD"
	convNew = "conv-NEW"
)

// leaveNote has the peer write body to name.
func leaveNote(t *testing.T, from *identityConn, name, body string) {
	t.Helper()
	if out, isErr := from.call("", "leave_note", map[string]any{"to": name, "body": body}); isErr {
		t.Fatalf("leave_note to %q: %s", name, out)
	}
}

// convFixture is a connection linked to convOld, a peer to write to it, and a note
// waiting under the connection's name.
type convFixture struct {
	w    *identityWorld
	ws   string
	peer *identityConn
	c    *identityConn
	name string // the connection's name, which convOld holds
}

func newConvFixture(t *testing.T) *convFixture {
	t.Helper()
	w := newIdentityWorld(t)
	f := &convFixture{w: w, ws: identityRepo(t)}
	f.peer = w.conn("")
	f.peer.call("", "session_start", map[string]any{"workspace": f.ws})
	f.c = w.conn("")
	f.c.start(convOld, f.ws, convOld, nil)
	f.name = f.c.s.sessionName()
	leaveNote(t, f.peer, f.name, "NOTE to the established name")
	return f
}

// #564: a second stamped conversation does not take the connection from the first.
func TestConversationIdentity_SecondStampedConversationKeepsItsOwn(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	peer := w.conn("")
	peer.call("", "session_start", map[string]any{"workspace": ws})
	c := w.conn("")
	c.start(convA, ws, convA, nil)
	nameA := c.s.sessionName()

	out := c.start(convB, ws, convB, nil)

	if got := c.s.externalID(); got != convA {
		t.Fatalf("the connection was relinked to %q, want it left on %q", got, convA)
	}
	if got := c.s.sessionName(); got != nameA {
		t.Fatalf("the connection was renamed %q, want %q", got, nameA)
	}
	if got := c.s.sessionNameFor(stampedCtx(convA)); got != nameA {
		t.Errorf("the first conversation answers to %q, want %q", got, nameA)
	}
	nameB := c.s.sessionNameFor(stampedCtx(convB))
	if nameB == "" || nameB == nameA {
		t.Fatalf("the second conversation has no name of its own: %q (first: %q)", nameB, nameA)
	}
	if line := sessionLine(out); !strings.Contains(line, nameB) || strings.Contains(line, nameA) {
		t.Errorf("the second conversation was oriented as %q, want its own name %q", line, nameB)
	}

	leaveNote(t, peer, nameA, "for A only")
	leaveNote(t, peer, nameB, "for B only")
	// B reads first, so a leak would take A's note before A could.
	if got, _ := c.call(convB, "check_messages", nil); !strings.Contains(got, "for B only") || strings.Contains(got, "for A only") {
		t.Errorf("the second conversation's mailbox = %q, want its own note and not the first's", got)
	}
	if got, _ := c.call(convA, "check_messages", nil); !strings.Contains(got, "for A only") || strings.Contains(got, "for B only") {
		t.Errorf("the first conversation's mailbox = %q, want its own note and not the second's", got)
	}
}

// The stamp is what makes the second conversation a conversation: with none, the
// call cannot be told from the one it would replace, and relinks as it always did.
func TestConversationIdentity_UnstampedSessionStartStillRelinks(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	c := w.conn("")
	c.start(convA, ws, convA, nil)
	name := c.s.sessionName()

	c.start("", ws, convB, nil) // no stamp: the typed session_id is all there is

	if got := c.s.externalID(); got != convB {
		t.Errorf("an unstamped session_start left the linkage on %q, want it relinked to %q", got, convB)
	}
	if got := c.s.sessionName(); got != name {
		t.Errorf("the connection's name changed to %q, want %q", got, name)
	}
}

// The handover is for a conversation's FIRST call, whichever tool it is: it need
// not be session_start. (A first call that changes state is refused until the
// conversation has declared itself, as for any stamped identity, and so does not
// reach the handover; a read does.)
func TestConversationIdentity_ClearHandsOverOnAnyFirstCall(t *testing.T) {
	f := newConvFixture(t)
	f.w.registry.clears.mark(convNew)

	if out, isErr := f.c.call(convNew, "workspace_sessions", nil); isErr {
		t.Fatalf("workspace_sessions: %s", out)
	}

	if ext := f.c.s.externalID(); ext != convNew {
		t.Fatalf("linkage = %q, want %q", ext, convNew)
	}
	got, _ := f.c.call(convNew, "check_messages", nil)
	if !strings.Contains(got, "NOTE to the established name") {
		t.Errorf("the conversation after /clear did not receive the connection's mail: %q", got)
	}
}

// Without a marker the newcomer is a second conversation: it has an identity of its
// own and none of the first one's mail, which stays with the first.
func TestConversationIdentity_NoMarkerMeansNoHandover(t *testing.T) {
	f := newConvFixture(t)

	f.c.start(convNew, "", convNew, nil)

	if got := f.c.s.externalID(); got != convOld {
		t.Fatalf("linkage = %q, want it left on %q", got, convOld)
	}
	got, _ := f.c.call(convNew, "check_messages", nil)
	if strings.Contains(got, "NOTE to the established name") {
		t.Errorf("a conversation with no /clear marker read the first one's mail: %q", got)
	}
	if name := f.c.s.sessionNameFor(stampedCtx(convNew)); name == "" || name == f.name {
		t.Errorf("the newcomer's name = %q, want one of its own (not %q)", name, f.name)
	}
	// The control: the note was there to be read, by the conversation it is for.
	if got, _ := f.c.call(convOld, "check_messages", nil); !strings.Contains(got, "NOTE to the established name") {
		t.Fatalf("the first conversation cannot read its own note, so the absence above proves nothing: %q", got)
	}
}

// A marker acts on the connection that receives the conversation's first call, once.
func TestConversationIdentity_MarkerIsConnectionScopedAndOneShot(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	c1, c2 := w.conn(""), w.conn("")
	c1.start("conv-ONE", ws, "conv-ONE", nil)
	c2.start("conv-TWO", ws, "conv-TWO", nil)
	name1, name2 := c1.s.sessionName(), c2.s.sessionName()

	w.registry.clears.mark(convNew)
	c2.start(convNew, "", convNew, nil)

	if got := c2.s.externalID(); got != convNew {
		t.Fatalf("the connection the conversation arrived on is linked to %q, want %q", got, convNew)
	}
	if got := c2.s.sessionName(); got != name2 {
		t.Errorf("the receiving connection was renamed %q, want %q", got, name2)
	}
	if got := c1.s.externalID(); got != "conv-ONE" {
		t.Errorf("the other connection was moved to %q by a marker that was not its own", got)
	}
	if got := c1.s.sessionName(); got != name1 {
		t.Errorf("the other connection was renamed %q, want %q", got, name1)
	}

	// Spent: the same conversation reaching the other connection is a newcomer there.
	c1.start(convNew, "", convNew, nil)
	if got := c1.s.externalID(); got != "conv-ONE" {
		t.Errorf("a spent marker handed a second connection over: linkage = %q", got)
	}
	if name := c1.s.sessionNameFor(stampedCtx(convNew)); name == "" || name == name1 {
		t.Errorf("the conversation has no identity of its own on the other connection: %q", name)
	}
}

// A marker that lapsed hands over nothing.
func TestConversationIdentity_ExpiredMarkerDoesNothing(t *testing.T) {
	f := newConvFixture(t)
	now := time.Now()
	f.w.registry.clears.now = func() time.Time { return now }
	f.w.registry.clears.mark(convNew)
	now = now.Add(clearMarkerTTL + time.Second)

	f.c.start(convNew, "", convNew, nil)

	if got := f.c.s.externalID(); got != convOld {
		t.Errorf("an expired marker moved the linkage to %q", got)
	}
}

// Once a conversation has an identity of its own on the connection, a marker that
// turns up late does not take the connection from the conversation that holds it.
func TestConversationIdentity_MarkerDoesNotApplyToAKnownConversation(t *testing.T) {
	f := newConvFixture(t)
	f.c.start(convNew, "", convNew, nil) // a newcomer first, with no marker

	f.w.registry.clears.mark(convNew)
	f.c.call(convNew, "check_messages", nil)

	if got := f.c.s.externalID(); got != convOld {
		t.Errorf("a marker for a conversation already known took the connection: linkage = %q", got)
	}
}

// A subagent stamp is never a conversation's start: its marker stays for its parent.
func TestConversationIdentity_SubagentDoesNotSpendTheMarker(t *testing.T) {
	f := newConvFixture(t)
	f.w.registry.clears.mark(convNew)

	f.c.call(convNew+"/agent-1", "check_messages", nil)
	if got := f.c.s.externalID(); got != convOld {
		t.Fatalf("a subagent's call took the connection: linkage = %q", got)
	}

	f.c.start(convNew, "", convNew, nil)
	if got := f.c.s.externalID(); got != convNew {
		t.Errorf("the conversation's own first call found its marker spent: linkage = %q", got)
	}
}

// The second conversation's own identity is recorded under the proxy session and
// comes back after a daemon restart, with the mail bound to it (#582's path).
func TestConversationIdentity_SecondConversationSurvivesARestart(t *testing.T) {
	w := newIdentityWorld(t).withState()
	ws := identityRepo(t)
	peer := w.conn("")
	peer.call("", "session_start", map[string]any{"workspace": ws})

	first := w.conn("proxyX")
	first.start(convA, ws, convA, nil)
	first.start(convB, ws, convB, nil)
	nameB, idB := first.s.sessionNameFor(stampedCtx(convB)), first.s.sessionIDFor(stampedCtx(convB))
	if idB == "" || idB == first.s.sessionID() {
		t.Fatalf("precondition: the second conversation holds no identity of its own (%q)", idB)
	}
	leaveNote(t, peer, nameB, "for B across the restart")
	first.s.close() // the daemon dies

	c := w.conn("proxyX")
	c.start(convA, ws, convA, nil)
	if got := c.s.externalID(); got != convA {
		t.Fatalf("precondition: the restored connection is linked to %q, want %q", got, convA)
	}
	if got, _ := c.call(convA, "check_messages", nil); strings.Contains(got, "for B across the restart") {
		t.Errorf("the first conversation read the second's mail after the restart: %q", got)
	}
	if got, _ := c.call(convB, "check_messages", nil); !strings.Contains(got, "for B across the restart") {
		t.Errorf("the second conversation did not get its mail back after the restart: %q", got)
	}
	ctx := stampedCtx(convB)
	if got := c.s.sessionNameFor(ctx); got != nameB {
		t.Errorf("the second conversation came back as %q, want %q", got, nameB)
	}
	if got := c.s.sessionIDFor(ctx); got != idB {
		t.Errorf("the second conversation came back with id %q, want %q", got, idB)
	}
	if got := c.s.externalID(); got != convA {
		t.Errorf("the restart moved the linkage to %q", got)
	}
}

func TestClearMarkers_AreOneShotBoundedAndExpire(t *testing.T) {
	now := time.Now()
	m := newClearMarkers()
	m.now = func() time.Time { return now }

	m.mark("conv-1")
	if !m.take("conv-1") {
		t.Fatal("a fresh marker was not taken")
	}
	if m.take("conv-1") {
		t.Error("a marker was taken twice")
	}

	m.mark("conv-2")
	now = now.Add(clearMarkerTTL + time.Second)
	if m.take("conv-2") {
		t.Error("an expired marker was taken")
	}
	if m.size() != 0 {
		t.Errorf("an expired marker was left behind: %d", m.size())
	}

	for _, bad := range []string{"", "conv-1/agent-7", "two words", "line\nbreak", strings.Repeat("x", clearMarkerMaxIDLen+1)} {
		m.mark(bad)
	}
	if m.size() != 0 {
		t.Errorf("an id that cannot be a conversation was recorded (%d markers)", m.size())
	}

	for i := range clearMarkerCap * 2 {
		m.mark(fmt.Sprintf("conv-n%d", i))
		now = now.Add(time.Millisecond)
	}
	if m.size() > clearMarkerCap {
		t.Errorf("the table grew to %d, past its bound of %d", m.size(), clearMarkerCap)
	}
}

func TestClearMarkers_NilTableHoldsNothing(t *testing.T) {
	var m *clearMarkers
	m.mark("conv-1")
	if m.pending() || m.take("conv-1") || m.size() != 0 {
		t.Error("a nil table recorded a marker")
	}
}
