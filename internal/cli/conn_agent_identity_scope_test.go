package cli

// conn_agent_identity_scope_test.go — the identity accessors, one rule at a time
// (#556): whose they are, what a caller with none gets, and when a row is made.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/session"
)

func newIdentitySession(t *testing.T) *connSession {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	s := newConnSession(context.Background(), detectTestPool(), nil, config.NewStore(config.Defaults()), nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	return s
}

// The predecessor's mail and threads are the conversation's main thread's alone.
func TestInheritedSessionIDsAreTheOwnersAlone(t *testing.T) {
	s := newIdentitySession(t)
	session.SetExternalID(s.sessionID(), "conv")
	s.inheritSessionID("pred-1")

	// One agent on the connection: it IS the connection, stamped or not.
	s.recordLogicalAgentCall("conv")
	for _, agent := range []string{"conv", ""} {
		if got := s.inheritedSessionIDsFor(stampedCtx(agent)); len(got) != 1 || got[0] != "pred-1" {
			t.Errorf("the only agent on the connection (%q) inherited %v, want [pred-1]", agent, got)
		}
	}

	// A second agent turns it shared.
	s.recordLogicalAgentCall("conv/agent-1")
	s.recordLogicalAgentCall("conv-2")
	cases := []struct {
		agent string
		owner bool
	}{
		{"conv", true},
		{"conv/agent-1", false}, // a subagent of the conversation
		{"conv-2", false},       // another conversation
		{"", false},             // nobody can say who this is
	}
	for _, c := range cases {
		got := s.inheritedSessionIDsFor(stampedCtx(c.agent))
		if c.owner != (len(got) == 1 && got[0] == "pred-1") || (!c.owner && len(got) != 0) {
			t.Errorf("agent %q on a shared connection inherited %v (owner=%v)", c.agent, got, c.owner)
		}
	}
}

// A subagent alone on the connection is still not the connection. After a restart its
// parent is routinely parked while it is the first stamped caller the new connection
// sees, so "the only agent here" cannot mean "the owner".
func TestALoneSubagentOfTheLinkedConversationIsNotTheOwner(t *testing.T) {
	s := newIdentitySession(t)
	session.SetExternalID(s.sessionID(), "conv")
	s.inheritSessionID("pred-1")
	s.recordLogicalAgentCall("conv/agent-1")
	if s.logicalAgents.sharedWith("conv/agent-1") {
		t.Fatal("precondition: the connection must look unshared, with one identity seen")
	}
	sub := stampedCtx("conv/agent-1")
	if got := s.inheritedSessionIDsFor(sub); len(got) != 0 {
		t.Errorf("a lone subagent inherited its parent's predecessor: %v", got)
	}
	if name := s.sessionNameFor(sub); name == "" || name == s.sessionName() {
		t.Errorf("a lone subagent answers to %q, want a name of its own and not the connection's %q", name, s.sessionName())
	}

	// With no linkage there is nobody to be a subagent OF: a stamped caller is the
	// connection's only agent, as every client that never links one always was.
	bare := newIdentitySession(t)
	bare.recordLogicalAgentCall("conv/agent-1")
	if got := bare.sessionNameFor(sub); got != bare.sessionName() {
		t.Errorf("with no linkage the only stamped agent answers to %q, want the connection's %q", got, bare.sessionName())
	}
}

// An agent with no identity of its own has none: never the connection's. The
// connection is closing here, so no row can be registered for it.
func TestAnAgentThatCannotBeGivenARowHasNoIdentity(t *testing.T) {
	s := newIdentitySession(t)
	session.SetExternalID(s.sessionID(), "conv")
	s.inheritSessionID("pred-1")
	s.recordLogicalAgentCall("conv")
	s.recordLogicalAgentCall("conv/agent-1")
	s.cancel() // registerAgentRow refuses on a closing connection

	sub := stampedCtx("conv/agent-1")
	if name, id := s.sessionNameFor(sub), s.sessionIDFor(sub); name != "" || id != "" {
		t.Errorf("a subagent with no row was given %q / %q", name, id)
	}
	if got := s.addressableNameFor(sub); got != "" {
		t.Errorf("a subagent with no row is addressable as %q", got)
	}
	in := s.inboxFor(sub)
	if in.Self != "" || in.SelfID != "" || len(in.InheritedIDs) != 0 {
		t.Errorf("a subagent with no row has an inbox: %+v", in)
	}
	if keys := in.Keys(); len(keys) != 0 {
		t.Errorf("a subagent with no row is woken by %v", keys)
	}

	// The owner is unaffected: its identity is the connection's, which exists.
	owner := stampedCtx("conv")
	if s.sessionNameFor(owner) != s.sessionName() || s.sessionIDFor(owner) != s.sessionID() {
		t.Error("the owner lost the connection's identity")
	}
	if in := s.inboxFor(owner); in.Self != s.sessionName() || len(in.InheritedIDs) != 1 {
		t.Errorf("the owner's inbox = %+v", in)
	}

	// An unattributable caller on a shared connection is nobody.
	anon := stampedCtx("")
	if s.sessionNameFor(anon) != "" || s.sessionIDFor(anon) != "" || s.addressableNameFor(anon) != "" {
		t.Error("an unattributable caller on a shared connection was named")
	}
}

// A preview that rides every tool result must not give every subagent that runs a
// tool a roster row; a claim does.
func TestKnownInboxForRegistersNothing(t *testing.T) {
	s := newIdentitySession(t)
	session.SetExternalID(s.sessionID(), "conv")
	s.recordLogicalAgentCall("conv")
	s.recordLogicalAgentCall("conv/agent-1")
	sub := stampedCtx("conv/agent-1")

	rowOf := func() string {
		sh := s.shardFor(sub)
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		return sh.rosterID
	}
	if in := s.knownInboxFor(sub); in.Self != "" {
		t.Errorf("an agent that has needed no identity has an inbox: %+v", in)
	}
	if rowOf() != "" {
		t.Fatal("previewing registered a roster row")
	}
	if in := s.inboxFor(sub); in.Self == "" || in.SelfID == "" || in.Self == s.sessionName() {
		t.Errorf("claiming did not give the subagent an identity of its own: %+v", in)
	}
	if rowOf() == "" {
		t.Error("claiming did not register a row")
	}
	if in := s.knownInboxFor(sub); in.Self == "" {
		t.Error("a registered agent's preview has no address")
	}
}

// A non-owner's row is its identity, so it follows the agent wherever it works.
// followConnectionShards and followParentShard move a shard's root without touching
// the row, and the agent's next call brings the row along.
func TestANonOwnersRosterRowFollowsItsRoot(t *testing.T) {
	s := newIdentitySession(t)
	session.SetExternalID(s.sessionID(), "conv")
	root, moved := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, root)
	mustGitDir(t, moved)
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false, false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	s.recordLogicalAgentCall("conv")
	s.recordLogicalAgentCall("conv/agent-1")
	sub := stampedCtx("conv/agent-1")

	id := s.sessionIDFor(sub)
	if id == "" {
		t.Fatal("the subagent was given no row")
	}
	folderOf := func() string {
		rows, err := session.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == id {
				return r.Folder
			}
		}
		t.Fatalf("row %s is not listed", id)
		return ""
	}
	if got := folderOf(); filepath.Clean(got) != filepath.Clean(root) {
		t.Fatalf("the row is listed in %q, want the root the agent works in, %q", got, root)
	}

	sh := s.shardFor(sub)
	sh.mu.Lock()
	sh.root = moved
	sh.mu.Unlock()
	s.touchAgentRoster("conv/agent-1")
	if got := folderOf(); filepath.Clean(got) != filepath.Clean(moved) {
		t.Errorf("the row stayed in %q after the agent moved to %q", got, moved)
	}
}

// The conversation's owner holds the connection's identity, except while it works in
// a workspace the connection is not pinned to: there it holds a row, the roster of
// that workspace lists it by that row, and peers there address that name. Its
// identity is the one its peers can see, and a subagent of it is not.
func TestOwnerWithARosterRowAnswersToTheNamePeersSeeThere(t *testing.T) {
	w := newIdentityWorld(t)
	ws := identityRepo(t)
	worktree := filepath.Join(ws, "worktree")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitDir(t, worktree)
	parent, peer := w.conn(""), w.conn("")
	const conv, sub = "conv-1", "conv-1/agent-7"

	parent.start(conv, ws, conv, nil)
	parent.start(sub, "", sub, nil) // the connection is now shared
	parent.start(conv, worktree, conv, nil)
	owner := stampedCtx(conv)
	if got := parent.s.workspaceFor(owner); filepath.Clean(got) != filepath.Clean(worktree) {
		t.Fatalf("precondition: the owner works in %q, want %q", got, worktree)
	}
	sh := parent.s.shardFor(owner)
	sh.mu.RLock()
	rowName, rowID := sh.rosterName, sh.rosterID
	sh.mu.RUnlock()
	if rowName == "" {
		t.Fatal("precondition: the owner moved off the connection's root and should hold a roster row")
	}
	if got, id := parent.s.sessionNameFor(owner), parent.s.sessionIDFor(owner); got != rowName || id != rowID {
		t.Errorf("the owner answers to %q/%s, want the row peers in its workspace see: %q/%s", got, id, rowName, rowID)
	}
	if in := parent.s.inboxFor(owner); len(in.InheritedIDs) != 0 || in.Self != rowName {
		t.Errorf("the owner's inbox = %+v", in)
	}

	// A peer in the worktree reads the roster and writes to the name it lists.
	peer.call("", "session_start", map[string]any{"workspace": worktree})
	if out, isErr := peer.call("", "leave_note", map[string]any{"to": rowName, "body": "to the owner, by its roster name"}); isErr {
		t.Fatalf("peer leave_note: %s", out)
	}
	if out, _ := parent.call(sub, "check_messages", nil); strings.Contains(out, "by its roster name") {
		t.Errorf("a subagent read the owner's mail: %q", out)
	}
	if out, _ := parent.call(conv, "check_messages", nil); !strings.Contains(out, "by its roster name") {
		t.Errorf("the owner never received mail addressed to the name its workspace lists it by: %q", out)
	}
}

// Rows are not left behind: they are retired with the connection.
func TestAgentRowsAreRetiredWithTheConnection(t *testing.T) {
	s := newIdentitySession(t)
	session.SetExternalID(s.sessionID(), "conv")
	s.recordLogicalAgentCall("conv")
	s.recordLogicalAgentCall("conv/agent-1")
	id := s.sessionIDFor(stampedCtx("conv/agent-1"))
	if id == "" {
		t.Fatal("the subagent was given no row")
	}
	listed := func() bool {
		rows, err := session.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	if !listed() {
		t.Fatal("precondition: the row is not listed")
	}
	s.close()
	if listed() {
		t.Error("the subagent's row outlived its connection")
	}
}
