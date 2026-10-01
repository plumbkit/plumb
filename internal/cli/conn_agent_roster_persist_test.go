package cli

// conn_agent_roster_persist_test.go — an agent's own roster identity across a
// daemon restart (#526).
//
// A subagent on a shared connection holds a name and session ID of its own. They
// used to live only in the session directory, so a restart stranded the mail bound
// to the ID, let the name go to whoever drew it next, and brought the agent back as
// somebody new. They are recorded under (proxy session, agent) now, and come back
// only to a connection presenting the same proxy secret: the typed stamp
// `<conversation>/<agent>` selects nothing on its own (threat-model A6).

import (
	"errors"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/session"
)

const (
	rosterConv = "conv-1"
	rosterSub  = "conv-1/agent-7"
	rosterNote = "for the SUBAGENT only"
)

// rosterFixture is a connection that ran a parent and a subagent under proxyX and
// then died with its daemon, with a note written to the subagent's own name.
type rosterFixture struct {
	w    *identityWorld
	peer *identityConn
	ws   string

	connName, connID string // the connection's own identity before the restart
	subName, subID   string // the subagent's roster identity before the restart
}

func newRosterFixture(t *testing.T) *rosterFixture {
	t.Helper()
	w := newIdentityWorld(t).withState()
	f := &rosterFixture{w: w, ws: identityRepo(t)}
	f.peer = w.conn("")
	f.peer.call("", "session_start", map[string]any{"workspace": f.ws})

	first := w.conn("proxyX")
	first.start(rosterConv, f.ws, rosterConv, nil)
	first.start(rosterSub, f.ws, rosterSub, nil)
	f.connName, f.connID = first.s.sessionName(), first.s.sessionID()
	f.subName, f.subID = first.s.sessionNameFor(stampedCtx(rosterSub)), first.s.sessionIDFor(stampedCtx(rosterSub))
	if f.subID == "" || f.subID == f.connID || f.subName == f.connName {
		t.Fatalf("precondition: the subagent holds no identity of its own (%q %q, connection %q %q)", f.subName, f.subID, f.connName, f.connID)
	}
	if out, isErr := f.peer.call("", "leave_note", map[string]any{"to": f.subName, "body": rosterNote}); isErr {
		t.Fatalf("peer leave_note: %s", out)
	}
	// The daemon dies: the connection ends and retires every row it registered.
	first.s.close()
	return f
}

// reconnect is the proxy coming back to a fresh daemon under proxy, with the
// parent's session_start first, as it would make it.
func (f *rosterFixture) reconnect(proxy string) *identityConn {
	c := f.w.conn(proxy)
	c.start(rosterConv, f.ws, rosterConv, nil)
	return c
}

func TestRosterIdentity_ComesBackAfterARestartWithTheSameProxy(t *testing.T) {
	f := newRosterFixture(t)
	c := f.reconnect("proxyX")

	// The connection's own identity came back too: the subagent's is not it.
	if c.s.sessionID() != f.connID || c.s.sessionName() != f.connName {
		t.Fatalf("precondition: the connection did not restore its own identity (%q %q, was %q %q)",
			c.s.sessionName(), c.s.sessionID(), f.connName, f.connID)
	}

	// Mail bound to the subagent's id before the restart is readable by it after.
	if out, _ := c.call(rosterSub, "check_messages", nil); !strings.Contains(out, rosterNote) {
		t.Fatalf("the subagent did not receive the note bound to its pre-restart identity: %q", out)
	}
	ctx := stampedCtx(rosterSub)
	if got := c.s.sessionNameFor(ctx); got != f.subName {
		t.Errorf("the subagent came back as %q, want its own name %q", got, f.subName)
	}
	if got := c.s.sessionIDFor(ctx); got != f.subID {
		t.Errorf("the subagent came back with id %q, want its own %q", got, f.subID)
	}
	if got := c.s.sessionIDFor(ctx); got == c.s.sessionID() {
		t.Errorf("the subagent came back as the connection (%q)", got)
	}
}

func TestRosterIdentity_TheParentDoesNotReadTheSubagentsMail(t *testing.T) {
	f := newRosterFixture(t)
	c := f.reconnect("proxyX")
	if out, _ := c.call(rosterConv, "check_messages", nil); strings.Contains(out, rosterNote) {
		t.Fatalf("the parent read the subagent's note: %q", out)
	}
	// The control: it was there to be read, by the subagent it was written for.
	if out, _ := c.call(rosterSub, "check_messages", nil); !strings.Contains(out, rosterNote) {
		t.Fatalf("the note is not deliverable to the subagent, so the absence above proves nothing: %q", out)
	}
}

// The restore is keyed to the proxy secret. A typed stamp is a string a model can
// write, so a different proxy session (or none) that stamps the same
// `<conversation>/<agent>` gets a fresh identity and none of the mail.
func TestRosterIdentity_IsNotRestoredAcrossProxySessions(t *testing.T) {
	for _, tc := range []struct{ name, proxy string }{
		{"a different proxy session", "proxyY"},
		{"no proxy credential", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRosterFixture(t)
			c := f.reconnect(tc.proxy)
			out, _ := c.call(rosterSub, "check_messages", nil)
			ctx := stampedCtx(rosterSub)
			if id := c.s.sessionIDFor(ctx); id == f.subID {
				t.Errorf("a stamp alone took the recorded session id %q", id)
			}
			if name := c.s.sessionNameFor(ctx); name == f.subName {
				t.Errorf("a stamp alone took the recorded name %q", name)
			}
			if strings.Contains(out, rosterNote) {
				t.Errorf("a stamp alone read the recorded identity's mail: %q", out)
			}
			// The control: the proxy that owns the record still gets all of it, so the
			// refusals above are the key doing its work and not a lost note.
			own := f.reconnect("proxyX")
			if out, _ := own.call(rosterSub, "check_messages", nil); !strings.Contains(out, rosterNote) {
				t.Fatalf("the rightful proxy cannot read the note either: %q", out)
			}
		})
	}
}

// A live session holding the recorded ID is a conflict, not licence to take it:
// the reconnect runs under a fresh identity and the record stays whole for the
// next restart.
func TestRosterIdentity_AnOverlappingReconnectKeepsTheRecord(t *testing.T) {
	w := newIdentityWorld(t).withState()
	ws := identityRepo(t)
	first := w.conn("proxyX")
	first.start(rosterConv, ws, rosterConv, nil)
	first.start(rosterSub, ws, rosterSub, nil)
	subName, subID := first.s.sessionNameFor(stampedCtx(rosterSub)), first.s.sessionIDFor(stampedCtx(rosterSub))

	second := w.conn("proxyX") // first is still live
	second.start(rosterConv, ws, rosterConv, nil)
	second.call(rosterSub, "check_messages", nil)
	if id := second.s.sessionIDFor(stampedCtx(rosterSub)); id == "" || id == subID {
		t.Fatalf("the overlapping reconnect holds %q; want an identity of its own, not the live %q", id, subID)
	}
	rec, ok, err := w.ss.RosterIdentityFor("proxyX", rosterSub)
	if err != nil || !ok || rec.SessionID != subID || rec.Name != subName {
		t.Fatalf("the record was overwritten by the refused restore: %+v ok=%v err=%v, want %q %q", rec, ok, err, subName, subID)
	}
}

// A name reserved for a disconnected agent is not handed to a session that is not
// that agent: the draw redraws past it, a rename is refused, and the agent's own
// session id is the one entitled to it.
func TestRosterIdentity_ItsNameIsReservedWhileItIsAway(t *testing.T) {
	f := newRosterFixture(t)
	reserved := reservationsExcept(f.w.ss, "", "")
	if got := reserved[strings.ToLower(f.subName)]; got != f.subID {
		t.Fatalf("the disconnected subagent's name %q is not reserved for it (got holder %q, want %q)", f.subName, got, f.subID)
	}

	stranger := f.w.conn("")
	stranger.call("", "session_start", map[string]any{"workspace": f.ws})
	_, err := session.RenameReserved(stranger.s.sessionID(), f.subName, reserved)
	if !errors.Is(err, session.ErrNameTaken) {
		t.Fatalf("a stranger took the reserved name %q: err=%v", f.subName, err)
	}
	if _, err := session.RenameReserved(stranger.s.sessionID(), f.subName, reservationsExcept(f.w.ss, f.subID, "")); err != nil {
		t.Fatalf("the agent's own id is refused its own reservation: %v", err)
	}
}
