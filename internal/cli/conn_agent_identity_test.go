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

// The half of symptom 6 that is about who is TOLD: a subagent that happened to reach
// a restarted connection first is not told it resumed anything, and the
// conversation's own main thread, which arrives later, is told once, and only as far
// as it is true. The connection took the NAME back; the predecessor's threads and the
// mail bound to its session ID did not follow (the linkage is a conversation id, which
// authorises neither: see conn_agent_identity_mail_test.go).
func TestOnlyTheConversationsOwnerIsToldItResumed(t *testing.T) {
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

	line := sessionLine(second.start(resumeConv, "", resumeConv, nil))
	if !strings.Contains(line, "resumed") {
		t.Errorf("the conversation's own main thread is not told it resumed: %q", line)
	}
	if !strings.Contains(line, "not inherited") {
		t.Errorf("the owner is not told that its predecessor's threads and mail did not follow it: %q", line)
	}
	if again := sessionLine(second.start(resumeConv, "", resumeConv, nil)); strings.Contains(again, "resumed") {
		t.Errorf("the owner is told it resumed a second time: %q", again)
	}
}

// What was pending for one conversation is not told to the next one that links the
// connection: a subagent of conv-A reached the restarted connection first and the
// news waited for conv-A's main thread, which never came before conv-B relinked it.
// conv-B resumed nothing.
func TestAPendingResumeIsNotToldToAConversationThatRelinksTheConnection(t *testing.T) {
	f := newResumeFixture(t)
	second := f.w.conn("")
	second.start(resumeSubConv, f.ws, resumeSubConv, nil)

	out := second.start("conv-B", "", "conv-B", nil)
	if got := second.s.externalID(); got != "conv-B" {
		t.Fatalf("precondition: conv-B did not relink the connection: %q", got)
	}
	if line := sessionLine(out); strings.Contains(line, "resumed") {
		t.Errorf("conv-B is told it resumed a predecessor that was conv-A's: %s", line)
	}
}

// An unstamped call on a connection several agents share is nobody: nothing says
// which agent made it, so workspace_sessions must not show it as the owner. It
// printed the owner's row as "you" and, under that identity, the owner's sent notes
// and the ids of its threads (the listing omits bodies, but the thread id is the
// address a peer replies in).
func TestWorkspaceSessionsDoesNotShowAnUnstampedCallerAsTheOwner(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	peer, owner := w.conn(""), w.conn("")
	const conv, sub = "conv-1", "conv-1/agent-7"

	peer.call("", "session_start", map[string]any{"workspace": ws})
	owner.start(conv, ws, conv, nil)
	owner.start(sub, "", sub, nil) // the connection is now shared
	ownerName := owner.s.sessionName()
	if out, isErr := owner.call(conv, "leave_note", map[string]any{"to": peer.s.sessionName(), "body": "the owner's private note"}); isErr {
		t.Fatalf("leave_note: %s", out)
	}
	received, _ := peer.call("", "check_messages", nil)
	threadID := conversationIDOf(t, received)

	// The control: the owner is shown as itself, with its own sent note and thread.
	own, _ := owner.call(conv, "workspace_sessions", nil)
	for _, want := range []string{"you:  " + ownerName, ownerName + " (you)", "your recent notes", threadID} {
		if !strings.Contains(own, want) {
			t.Fatalf("the owner's own listing does not show %q, so the absences below prove nothing:\n%s", want, own)
		}
	}

	anon, _ := owner.call("", "workspace_sessions", nil)
	for _, leak := range []string{"you:", "(you)", "your recent notes", threadID} {
		if strings.Contains(anon, leak) {
			t.Errorf("an unstamped caller on a shared connection was shown the owner's identity (%q):\n%s", leak, anon)
		}
	}
}
