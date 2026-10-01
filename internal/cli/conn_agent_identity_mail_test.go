package cli

// conn_agent_identity_mail_test.go — whose mail a predecessor's session ID carries
// (#556), and who may NOT have it.
//
// Mail and threads bound to a session ID belong to the conversation that held the
// session. The only authority that may hand them to a successor is the serve
// proxy's own credential, and then only when it also got the name back. A
// conversation id is a routing key the model can type (docs/threat-model.md, A6),
// so it authorises neither a thread seat nor a predecessor's mail, and a refused
// adoption must not leave a connection to be claimed by whoever arrives first.
//
// These tests pin the SAFE outcome of two cases the first cut of #556 got wrong:
// a proxy reconnect that overlaps its predecessor, and a connection that merely
// claims a resumed conversation's id.

import (
	"strings"
	"testing"
)

const bound = "SECRET bound to the predecessor"

// overlapFixture is a proxy reconnect that overlaps its predecessor: conv-1 ran on
// `first` under proxyX, a peer wrote to it (the note is bound to first's session
// ID), and the same proxy reconnected as `second` while `first` is still live. The
// adoption of first's ID and the name are both refused, so `second` runs under a
// temporary identity and holds nothing of first's.
type overlapFixture struct {
	w      *identityWorld
	ws     string
	first  *identityConn
	second *identityConn
}

func newOverlapFixture(t *testing.T) *overlapFixture {
	t.Helper()
	w := newIdentityWorld(t).withState()
	f := &overlapFixture{w: w, ws: identityRepo(t)}
	peer := w.conn("")
	peer.call("", "session_start", map[string]any{"workspace": f.ws})

	f.first = w.conn("proxyX")
	f.first.start("conv-1", f.ws, "conv-1", nil)
	if out, isErr := peer.call("", "leave_note", map[string]any{"to": f.first.s.sessionName(), "body": bound}); isErr {
		t.Fatalf("peer leave_note: %s", out)
	}

	f.second = w.conn("proxyX")
	if f.second.s.sessionID() == f.first.s.sessionID() {
		t.Fatal("precondition: the overlapping reconnect adopted the live session ID, so nothing is refused here")
	}
	if f.second.s.sessionName() == f.first.s.sessionName() {
		t.Fatal("precondition: the overlapping reconnect took the live name, so it is not the overlap this test is about")
	}
	return f
}

// A refused adoption leaves the connection unlinked, so the first agent to call on
// it would otherwise become its owner and read the original conversation's mail:
// another conversation, a subagent of it, or a call the hook failed to stamp.
func TestAnOverlappingReconnectGrantsNobodyThePredecessorsMail(t *testing.T) {
	// The control comes first: the note is there to be read, and the connection that
	// holds the session it is bound to reads it. Without it every absence below
	// would also pass on a note that was never delivered.
	t.Run("control: the original conversation on its own connection reads it", func(t *testing.T) {
		f := newOverlapFixture(t)
		if out, _ := f.first.call("conv-1", "check_messages", nil); !strings.Contains(out, bound) {
			t.Fatalf("the note bound to the original session is not delivered to it, so the cases below prove nothing: %q", out)
		}
	})

	cases := []struct {
		name  string
		agent string // the stamp, "" for a call the hook failed to stamp
		start bool   // whether the caller opens with a session_start of its own conversation
		conv  string
	}{
		{name: "another conversation that links the connection", agent: "conv-2", start: true, conv: "conv-2"},
		{name: "a subagent that calls before anyone has called session_start", agent: "conv-1/agent-7"},
		{name: "an unstamped caller", agent: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newOverlapFixture(t)
			var out string
			if c.start {
				out = f.second.start(c.agent, f.ws, c.conv, nil)
			}
			got, _ := f.second.call(c.agent, "check_messages", nil)
			if strings.Contains(out+got, bound) {
				t.Errorf("%s read the original conversation's mail:\nsession_start: %q\ncheck_messages: %q", c.name, out, got)
			}
			if ids := f.second.s.inheritedSessionIDsFor(stampedCtx(c.agent)); len(ids) != 0 {
				t.Errorf("%s was handed the predecessor's identity %v", c.name, ids)
			}
		})
	}
}

// The linkage path needs no secret, only a conversation id, so it must grant no
// predecessor identity to anyone: not to a connection that stamps itself as the
// conversation either. It takes the NAME back, as every client always could, and
// never the threads or the mail bound to the predecessor's session ID.
func TestAClaimedConversationIdInheritsNoMailOrThreads(t *testing.T) {
	cases := []struct {
		name  string
		agent string // the stamp, "" for an unstamped call
		arg   string // the session_id it declares
	}{
		{name: "the conversation's own stamped main thread, with no credential", agent: resumeConv, arg: resumeConv},
		{name: "a subagent of the conversation", agent: resumeSubConv, arg: resumeSubConv},
		{name: "a stamped different conversation", agent: "conv-2", arg: "conv-2"},
		{name: "a stamped different conversation claiming this one's id", agent: "conv-2", arg: resumeConv},
		{name: "a subagent claiming the conversation's id outright", agent: resumeSubConv, arg: resumeConv},
		{name: "an unstamped caller", agent: "", arg: resumeConv},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newResumeFixture(t)
			second := f.w.conn("") // no proxy credential at all
			out := second.start(c.agent, f.ws, c.arg, nil)

			if reply, isErr := second.call(c.agent, "leave_note", map[string]any{"conversation_id": f.threadID, "body": "intruding"}); !isErr && !strings.Contains(reply, "not one of yours") {
				t.Errorf("%s replied in the predecessor's thread: %q", c.name, reply)
			}
			got, _ := second.call(c.agent, "check_messages", nil)
			for _, note := range []string{"second note, unread", "hello parent"} {
				if strings.Contains(out+got, note) {
					t.Errorf("%s read mail bound to the predecessor (%q):\nsession_start: %q\ncheck_messages: %q", c.name, note, out, got)
				}
			}
			if ids := second.s.inheritedSessionIDsFor(stampedCtx(c.agent)); len(ids) != 0 {
				t.Errorf("%s was handed the predecessor's identity %v", c.name, ids)
			}
			if ids := second.s.inheritedSessionIDs(); len(ids) != 0 {
				t.Errorf("the connection itself holds the predecessor's identity %v after %s", ids, c.name)
			}
		})
	}
}
