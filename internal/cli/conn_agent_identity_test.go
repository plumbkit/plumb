package cli

// conn_agent_identity_test.go — issue #556: on a connection several agents share,
// each agent is somebody, and nobody is somebody else.
//
// Every test here drives real tools through a real server (see
// conn_agent_identity_harness_test.go). The rule under test is one sentence: the
// connection's own identity — its name, its session ID, the predecessor IDs it
// inherited — belongs to the conversation the connection is linked to, and to no
// other agent that happens to be multiplexed over it. Before the fix a subagent
// answered as its parent: it was told the parent's name, it consumed the parent's
// mail, it signed its commits with the parent's name, and on a resumed connection
// it was told it had "resumed".

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Symptoms 1, 2 and 5: a stamped subagent that shares its parent's connection and
// its parent's root, so nothing about its pin gives it an identity of its own.
func TestSubagentOnTheParentsConnectionIsNotTheParent(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	parent := w.conn("")
	peer := w.conn("")
	const conv, sub = "conv-1", "conv-1/agent-7"

	parent.start(conv, ws, conv, nil)
	parentName := parent.s.sessionName()
	subOut := parent.start(sub, "", sub, nil)

	subName := parent.s.sessionNameFor(stampedCtx(sub))
	if subName == "" || subName == parentName {
		t.Fatalf("the subagent's name = %q, want a name of its own distinct from the parent's %q", subName, parentName)
	}
	subLine := sessionLine(subOut)
	if strings.Contains(subLine, parentName) {
		t.Errorf("symptom 1: the subagent's orientation names it %q: %s", parentName, subLine)
	}
	if !strings.Contains(subLine, subName) {
		t.Errorf("the subagent's orientation does not state its own name %q: %q", subName, subLine)
	}

	// Symptom 2. A peer writes to the PARENT; the subagent polls first.
	peer.call("", "session_start", map[string]any{"workspace": ws})
	if out, isErr := peer.call("", "leave_note", map[string]any{"to": parentName, "body": "for the PARENT only"}); isErr {
		t.Fatalf("peer leave_note: %s", out)
	}
	if out, _ := parent.call(sub, "check_messages", nil); strings.Contains(out, "for the PARENT only") {
		t.Errorf("symptom 2: the subagent consumed a note addressed to its parent: %q", out)
	}
	if out, _ := parent.call(conv, "check_messages", nil); !strings.Contains(out, "for the PARENT only") {
		t.Errorf("the parent never received its own note after the subagent polled: %q", out)
	}

	// Symptom 5. The subagent's commit names the subagent.
	writeIdentityFile(t, filepath.Join(ws, "a.txt"), "x\n")
	parent.call(sub, "git", map[string]any{"subcommand": "add", "files": []string{"a.txt"}})
	if out, isErr := parent.call(sub, "git", map[string]any{"subcommand": "commit", "message": "subagent commit"}); isErr {
		t.Fatalf("git commit: %s", out)
	}
	log, err := exec.Command("git", "-C", ws, "log", "-1", "--format=%(trailers)").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	if got := string(log); !strings.Contains(got, "Plumb-Session: "+subName) || strings.Contains(got, parentName) {
		t.Errorf("symptom 5: the subagent's commit carries %q, want Plumb-Session: %s and never %s", strings.TrimSpace(got), subName, parentName)
	}
}

// A subagent that has re-pinned elsewhere holds a roster row already, which is the
// case the first fix for #472 covered. session_start's mailbox claim still ran
// against the CONNECTION's inbox, so the row bought it a name and nothing else.
func TestSubagentWithARootOfItsOwnCannotConsumeTheParentsMailAtSessionStart(t *testing.T) {
	w := newIdentityWorld(t)
	ws, ws2 := identityRepo(t), identityRepo(t)
	parent := w.conn("")
	peer := w.conn("")
	const conv, sub = "conv-1", "conv-1/agent-7"

	parent.start(conv, ws, conv, nil)
	parent.start(sub, ws2, sub, map[string]any{"force": true})
	peer.call("", "session_start", map[string]any{"workspace": ws})
	if out, isErr := peer.call("", "leave_note", map[string]any{"to": parent.s.sessionName(), "body": "for the PARENT only"}); isErr {
		t.Fatalf("peer leave_note: %s", out)
	}

	out := parent.start(sub, "", sub, nil)
	if strings.Contains(out, "for the PARENT only") {
		t.Errorf("symptom 2 through session_start's Messages block: the subagent consumed the parent's note: %q", out)
	}
	if got, _ := parent.call(conv, "check_messages", nil); !strings.Contains(got, "for the PARENT only") {
		t.Errorf("the parent lost its note to its subagent's session_start: %q", got)
	}
}

// resumeFixture is the daemon-restart-without-a-credential shape: a conversation
// had a connection, a peer wrote to it, the thread was claimed, a second note was
// left unread, and the connection ended. A fresh connection then opens with no
// credential, so the only link back is the conversation id.
type resumeFixture struct {
	w        *identityWorld
	ws       string
	peer     *identityConn
	name1    string
	threadID string
}

const (
	resumeConv    = "conv-1"
	resumeSubConv = "conv-1/agent-9"
)

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	w := newIdentityWorld(t)
	f := &resumeFixture{w: w, ws: identityRepo(t), peer: w.conn("")}
	f.peer.call("", "session_start", map[string]any{"workspace": f.ws})

	first := w.conn("")
	first.start(resumeConv, f.ws, resumeConv, nil)
	f.name1 = first.s.sessionName()

	f.peer.call("", "leave_note", map[string]any{"to": f.name1, "body": "hello parent"})
	out, _ := first.call(resumeConv, "check_messages", nil)
	f.threadID = conversationIDOf(t, out)
	if out, isErr := first.call(resumeConv, "leave_note", map[string]any{"conversation_id": f.threadID, "body": "reply from the original"}); isErr || strings.Contains(out, "not one of yours") {
		t.Fatalf("precondition: the original session cannot reply in its own thread: %q", out)
	}
	// Left unread: bound to the original connection's session ID.
	f.peer.call("", "leave_note", map[string]any{"to": f.name1, "body": "second note, unread"})
	first.s.close()
	return f
}

// Symptom 3, and the half of symptom 6 that is about who is TOLD: the conversation's
// main thread resumes its thread and its unread mail; the subagent that happened to
// reach the new connection first is not told it resumed anything.
func TestResumedConversationKeepsItsThreadsAndOnlyItsOwnerIsToldItResumed(t *testing.T) {
	f := newResumeFixture(t)
	second := f.w.conn("")

	// The first stamped call is the subagent's: the parent is parked on the Agent tool.
	subOut := second.start(resumeSubConv, f.ws, resumeSubConv, nil)
	if line := sessionLine(subOut); strings.Contains(line, "resumed") {
		t.Errorf("symptom 6: a subagent that never existed before is told it resumed: %s", line)
	}
	if got := second.s.sessionName(); got != f.name1 {
		t.Errorf("the connection did not take back its conversation's name: %q, want %q", got, f.name1)
	}
	// Alone on the connection, and first, the subagent is still not the connection: its
	// parent is merely parked, and the name the connection took back is the parent's.
	if got := second.s.sessionNameFor(stampedCtx(resumeSubConv)); got == "" || got == f.name1 {
		t.Errorf("the first subagent on a restarted connection answers to %q, want a name of its own and not its parent's %q", got, f.name1)
	}
	if line := sessionLine(subOut); strings.Contains(line, f.name1) {
		t.Errorf("the first subagent on a restarted connection was told it is its parent: %s", line)
	}

	parentOut := second.start(resumeConv, "", resumeConv, nil)
	line := sessionLine(parentOut)
	if !strings.Contains(line, "resumed") {
		t.Errorf("the conversation's own main thread is not told it resumed: %q", line)
	}
	if strings.Contains(line, "not inherited") {
		t.Errorf("the owner inherited its predecessor's threads but is told it did not: %q", line)
	}

	out, isErr := second.call(resumeConv, "leave_note", map[string]any{"conversation_id": f.threadID, "body": "reply from the recovered session"})
	if isErr || strings.Contains(out, "not one of yours") {
		t.Errorf("symptom 3: the recovered session cannot reply in its own thread: %q", out)
	}
	// Delivered by session_start's own Messages block, which claims before check_messages
	// can: the grant has to be in place by the end of the call that made it.
	if !strings.Contains(parentOut, "second note, unread") {
		t.Errorf("mail bound to the predecessor did not follow the conversation: %q", parentOut)
	}
}

// The safety rule's negative controls. Inheritance is the one grant here that is
// authorised by a CLAIM (a conversation id) rather than by the proxy secret, so
// each of these is a caller that must walk away with nothing.
func TestThreadInheritanceIsRefusedToEveryoneButTheConversationsMainThread(t *testing.T) {
	cases := []struct {
		name  string
		agent string // the stamp, "" for an unstamped call
		arg   string // the session_id it declares
	}{
		{name: "a subagent of the conversation", agent: resumeSubConv, arg: resumeSubConv},
		{name: "a stamped different conversation", agent: "conv-2", arg: "conv-2"},
		{name: "a stamped different conversation claiming this one's id", agent: "conv-2", arg: resumeConv},
		{name: "a subagent claiming the conversation's id outright", agent: resumeSubConv, arg: resumeConv},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newResumeFixture(t)
			second := f.w.conn("")
			second.start(c.agent, f.ws, c.arg, nil)

			if out, isErr := second.call(c.agent, "leave_note", map[string]any{"conversation_id": f.threadID, "body": "intruding"}); !isErr && !strings.Contains(out, "not one of yours") {
				t.Errorf("%s replied in the predecessor's thread: %q", c.name, out)
			}
			if got, _ := second.call(c.agent, "check_messages", nil); strings.Contains(got, "second note, unread") || strings.Contains(got, "hello parent") {
				t.Errorf("%s read mail bound to the predecessor: %q", c.name, got)
			}
			if ids := second.s.inheritedSessionIDsFor(stampedCtx(c.agent)); len(ids) != 0 {
				t.Errorf("%s was handed the predecessor's identity %v", c.name, ids)
			}
		})
	}

	// An unstamped call is a claim nothing vouches for. It may still take back the
	// NAME, as every non-hook client always could, but not the threads.
	t.Run("an unstamped caller", func(t *testing.T) {
		f := newResumeFixture(t)
		second := f.w.conn("")
		second.start("", f.ws, resumeConv, nil)

		if out, isErr := second.call("", "leave_note", map[string]any{"conversation_id": f.threadID, "body": "intruding"}); !isErr && !strings.Contains(out, "not one of yours") {
			t.Errorf("an unstamped caller replied in the predecessor's thread: %q", out)
		}
		if ids := second.s.inheritedSessionIDsFor(stampedCtx("")); len(ids) != 0 {
			t.Errorf("an unstamped caller was handed the predecessor's identity %v", ids)
		}
	})
}

// #564 and symptom 4: two conversations share one connection, which is the normal
// case for Claude desktop's Code tab. The second conversation's session_start used
// to replace the connection's linkage and rename the connection.
func TestSecondConversationDoesNotTakeOverTheConnection(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)

	// conv-B had a connection of its own once, and a name.
	b0 := w.conn("")
	b0.start("conv-B", ws, "conv-B", nil)
	oldB := b0.s.sessionName()
	b0.s.close()

	c := w.conn("")
	c.start("conv-A", ws, "conv-A", nil)
	nameA, idA := c.s.sessionName(), c.s.sessionID()
	if got := c.s.externalID(); got != "conv-A" {
		t.Fatalf("precondition: linkage = %q, want conv-A", got)
	}

	out := c.start("conv-B", "", "conv-B", nil)
	if got := c.s.externalID(); got != "conv-A" {
		t.Errorf("#564: conv-B replaced the connection's linkage: %q, want conv-A", got)
	}
	if got := c.s.sessionName(); got != nameA {
		t.Errorf("#564: conv-B renamed the connection: %q, want %q", got, nameA)
	}
	if got := c.s.sessionID(); got != idA {
		t.Errorf("conv-B changed the connection's session ID: %q, want %q", got, idA)
	}
	if line := sessionLine(out); strings.Contains(line, "resumed") || strings.Contains(line, nameA) {
		t.Errorf("conv-B was told it is the connection (%q) or that it resumed: %s", nameA, line)
	}
	if !strings.Contains(out, "different conversation") {
		t.Errorf("conv-B is not told its session_id was left unlinked: %q", out)
	}
	if got := c.s.sessionNameFor(stampedCtx("conv-B")); got == "" || got == nameA || got == oldB {
		t.Errorf("conv-B's own name = %q, want a fresh one distinct from %q and its predecessor's %q", got, nameA, oldB)
	}

	// conv-A, back on its own connection, is still conv-A.
	outA := c.start("conv-A", "", "conv-A", nil)
	if line := sessionLine(outA); !strings.Contains(line, nameA) {
		t.Errorf("conv-A no longer sees its own name %q: %s", nameA, line)
	}
}

// What an UNSTAMPED call may do on a connection (the hook failed open): it can
// link a connection that has no linkage and nobody else on it, which is every
// client without a hook, and it can do nothing else.
func TestUnstampedSessionStartCannotRelinkAConnection(t *testing.T) {
	w := newIdentityWorld(t)
	ws, ws2 := identityRepo(t), identityRepo(t)

	t.Run("it still links a connection nothing else is on", func(t *testing.T) {
		c := w.conn("")
		c.start("", ws, "typed-by-the-model", nil)
		if got := c.s.externalID(); got != "typed-by-the-model" {
			t.Errorf("an unstamped session_start on a lone connection did not link it: %q", got)
		}
	})

	t.Run("it cannot replace a linkage", func(t *testing.T) {
		c := w.conn("")
		const conv, sub = "conv-1", "conv-1/agent-7"
		c.start(conv, ws, conv, nil)
		c.start(sub, ws2, sub, map[string]any{"force": true})
		name := c.s.sessionName()
		out := c.start("", "", "subagent-7", nil)
		if got := c.s.externalID(); got != conv {
			t.Errorf("an unstamped session_start replaced the linkage: %q, want %q", got, conv)
		}
		if got := c.s.sessionName(); got != name {
			t.Errorf("an unstamped session_start renamed the connection: %q, want %q", got, name)
		}
		if line := sessionLine(out); strings.Contains(line, name) {
			t.Errorf("an unattributable caller was told it is %q: %s", name, line)
		}
	})
}

// The proxy-credential path grants the predecessor to the connection whatever
// became of its name, and the connection hands it to its owner alone. Here the
// predecessor is still live, so both the ID and the name are refused and the
// connection runs under a temporary identity: the case the old gate on the name
// stranded.
func TestCredentialGrantReachesOnlyTheOwner(t *testing.T) {
	w := newIdentityWorld(t).withState()
	ws := identityRepo(t)
	peer := w.conn("")
	peer.call("", "session_start", map[string]any{"workspace": ws})
	const conv, sub = "conv-1", "conv-1/agent-7"

	first := w.conn("proxyX")
	first.start(conv, ws, conv, nil)
	firstName, firstID := first.s.sessionName(), first.s.sessionID()
	// Bound to the first connection's session ID, which this connection will not hold.
	if out, isErr := peer.call("", "leave_note", map[string]any{"to": firstName, "body": "bound to the predecessor"}); isErr {
		t.Fatalf("peer leave_note: %s", out)
	}

	// The same proxy reconnects while the predecessor still holds the name and the ID.
	second := w.conn("proxyX")
	if got := second.s.sessionID(); got == firstID {
		t.Fatalf("precondition: the overlapping reconnect adopted the live ID %s", firstID)
	}
	// The subagent is first, so the connection is shared before the owner calls, and
	// the subagent polls BEFORE the owner does: had it been granted the predecessor
	// it would win the mail.
	second.start(sub, ws, sub, nil)
	if out, _ := second.call(sub, "check_messages", nil); strings.Contains(out, "bound to the predecessor") {
		t.Errorf("a subagent read mail bound to the owner's predecessor: %q", out)
	}
	ownerOut := second.start(conv, "", conv, nil)

	if got := second.s.inheritedSessionIDsFor(stampedCtx(conv)); len(got) != 1 || got[0] != firstID {
		t.Errorf("the owner inherited %v, want [%s]", got, firstID)
	}
	if got := second.s.inheritedSessionIDsFor(stampedCtx(sub)); len(got) != 0 {
		t.Errorf("a subagent was granted the owner's predecessor: %v", got)
	}
	if !strings.Contains(ownerOut, "bound to the predecessor") {
		t.Errorf("the owner did not receive mail bound to its predecessor: %q", ownerOut)
	}
}
