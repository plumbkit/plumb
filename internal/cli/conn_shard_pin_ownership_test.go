package cli

// conn_shard_pin_ownership_test.go — a shard must be able to tell a workspace
// its agent CHOSE from one it was merely SEEDED with (issue #468). Three
// defects followed from it not being able to, and all three are pinned here.
//
// 1. A shard is seeded from the connection's pin AND from that pin's origin, so
//    a connection whose origin some other caller's same-root session_start had
//    promoted handed every shard built afterwards a PinSourceSessionStart it
//    never asked for. The per-agent sticky guard then refused that agent's
//    FIRST explicit pin as a drift away from a workspace it had never held,
//    with force: true — which displaces a peer on a pooled connection — as the
//    only way through. This is the path the reported session took.
//
// 2. An explicit session_start that lands on the CONNECTION (the caller is the
//    only identity the connection has seen, so no shard exists yet) is recorded
//    only under the connection-level agent id. Nothing remembers who chose it,
//    so when a peer later turns the connection shared and the agent's shard is
//    built, shardFor restores whatever per-agent row survived the last proxy
//    reconnect — a project the agent has since left — and that stale row
//    outranks the pin the agent just made.
//
// 3. repinAgent's same-root early return fires BEFORE selfPinned is set, so an
//    agent whose explicit session_start names the root its shard was already
//    seeded at never records that it chose one. Its shard keeps following the
//    connection, and a later connection move drags it off a workspace it had
//    explicitly named — with no call of its own in between. The comment under
//    that early return has always claimed this case.
//
// In every case the agent's workspace-relative calls resolve inside a root it
// did not choose, with nothing in the response saying so. The fixture is the
// reported shape — a git worktree UNDER its parent checkout — because that
// containment is why the drift stayed silent: the same relative path exists in
// both roots, so the wrong root returned a plausible file instead of a boundary
// error.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// worktreeUnderParent builds a parent checkout and a git worktree nested inside
// it, each holding its own copy of the same relative path with distinguishable
// contents.
func worktreeUnderParent(t *testing.T) (parent, worktree string) {
	t.Helper()
	parent = freshTempDir(t)
	mustGitDir(t, parent)
	worktree = filepath.Join(parent, ".claude", "worktrees", "feature")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitDir(t, worktree)
	mustWrite(t, filepath.Join(parent, "notes.md"), "the parent checkout\n")
	mustWrite(t, filepath.Join(worktree, "notes.md"), "the worktree, already edited\n")
	return parent, worktree
}

// TestSeededShardDoesNotInheritAPeersStickiness is the sequence the production
// daemon log records for the reported incident, step for step.
//
// The connection attached to the parent checkout from the client's roots, so
// its pin origin was PinSourceRoots. A peer then named that same root in an
// explicit session_start, which takes attachOrRepinTo's same-root promotion
// branch and upgrades the CONNECTION's pin origin to PinSourceSessionStart —
// silently, since no root moved and the branch logs nothing. The peer's
// declaration turned the connection shared. When the reporting agent's shard
// was built on its next call, shardFor copied that promoted origin onto the
// shard, and the per-agent sticky guard then refused the agent's FIRST EVER
// explicit pin as though it were drifting away from a workspace it had chosen
// — offering force: true, which on a pooled connection displaces a peer.
//
// Until the agent's pin lands, every workspace-relative call resolves inside
// the seeded root instead. With the worktree nested under the parent, that is
// a real file in the wrong repository rather than a boundary error.
func TestSeededShardDoesNotInheritAPeersStickiness(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)

	s := newPersistSession(t, store, ss, "proxy-seeded")
	// The connection attaches to the parent from the client's reported roots.
	s.attachWorkspace(context.Background(), "file://"+parent)

	// A peer names that same root explicitly, promoting the pin origin.
	ctxPeer := mcp.WithLogicalAgent(context.Background(), "peer")
	if _, err := s.repinWorkspace(ctxPeer, parent, "", false); err != nil {
		t.Fatalf("the peer's same-root session_start: %v", err)
	}
	s.recordLogicalAgentAttach("peer")

	// The reporting agent's FIRST session_start, naming its own worktree. It
	// has never pinned anything on this connection.
	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")
	root, err := s.repinWorkspace(ctxAgent, worktree, "", false)
	if err != nil {
		t.Fatalf("an agent's first explicit pin was refused off a root it never chose: %v", err)
	}
	if root != worktree {
		t.Fatalf("session_start echoed %q, want %q", root, worktree)
	}
	if got := s.workspaceFor(ctxAgent); got != worktree {
		t.Errorf("the agent resolves to %q, want %q — a workspace-relative read would have returned %q",
			got, worktree, filepath.Join(got, "notes.md"))
	}

	// The peer is untouched: per-agent isolation, not a shared pin.
	if got := s.workspaceFor(ctxPeer); got != parent {
		t.Errorf("the peer moved to %q; it must keep %q", got, parent)
	}
	if got := s.workspace(); got != parent {
		t.Errorf("the connection pin moved to %q; it must keep %q", got, parent)
	}
}

// TestChosenShardStaysSticky is the other side of the guard: an agent that
// actually chose a workspace is still refused a non-forced move away from it,
// which is the protection issue #182 asks for.
func TestChosenShardStaysSticky(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	other := freshTempDir(t)
	mustGitDir(t, other)

	s := newPersistSession(t, store, ss, "proxy-chosen")
	s.attachWorkspace(context.Background(), "file://"+parent)
	s.recordLogicalAgentAttach("peer")

	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")
	if _, err := s.repinWorkspace(ctxAgent, worktree, "", false); err != nil {
		t.Fatalf("the agent's own pin: %v", err)
	}

	_, err := s.repinWorkspace(ctxAgent, other, "", false)
	if err == nil {
		t.Fatal("a move away from the workspace this agent chose must be refused without force")
	}
	if !strings.Contains(err.Error(), "sticky") {
		t.Errorf("the refusal lost its diagnosis: %v", err)
	}
	if got := s.workspaceFor(ctxAgent); got != worktree {
		t.Errorf("the refused re-pin moved the shard to %q; it must stay at %q", got, worktree)
	}
}

// TestAgentPinSurvivesItsShardMaterialising is the incident, reproduced: an
// agent explicitly pins its worktree, a peer then declares itself, and the
// agent's next workspace-relative call must still resolve inside the worktree.
func TestAgentPinSurvivesItsShardMaterialising(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)
	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")

	// Earlier in the client session this agent worked in the parent checkout,
	// on a connection that was already shared — so the pin was recorded under
	// its own logical-agent id.
	first := newPersistSession(t, store, ss, "proxy-drift")
	first.recordLogicalAgentAttach("agent-A")
	first.recordLogicalAgentAttach("peer")
	if _, err := first.repinWorkspace(ctxAgent, parent, "", false); err != nil {
		t.Fatalf("setup: pinning the parent checkout: %v", err)
	}
	first.close()

	// The proxy reconnects (a daemon restart, or plumb restart after a
	// rebuild): same proxy session, a fresh connection whose observed-identity
	// set starts empty, so this agent is once again the only identity known.
	second := newPersistSession(t, store, ss, "proxy-drift")
	root, err := second.repinWorkspace(ctxAgent, worktree, "", false)
	if err != nil {
		t.Fatalf("the agent's explicit session_start: %v", err)
	}
	if root != worktree {
		t.Fatalf("session_start echoed %q, want %q", root, worktree)
	}
	second.recordLogicalAgentAttach("agent-A")
	if got := second.workspaceFor(ctxAgent); got != worktree {
		t.Fatalf("precondition: the agent resolves to %q straight after its own pin, want %q", got, worktree)
	}

	// A peer declares itself. Nothing about this agent changed — but the
	// declaration is what turns the connection shared, so the agent's shard is
	// built on its very next call.
	second.recordLogicalAgentAttach("peer")

	if got := second.workspaceFor(ctxAgent); got != worktree {
		t.Errorf("the agent's workspace drifted to %q after a peer declared itself; a workspace-relative read would have returned %q instead of the worktree's copy, with no signal",
			got, filepath.Join(got, "notes.md"))
	}

	// Naming its own workspace again must not be refused: an agent that never
	// left is not a peer trying to steal a pin, and force: true is not a remedy
	// it can safely reach for on a connection it shares.
	if _, err := second.repinWorkspace(ctxAgent, worktree, "", false); err != nil {
		t.Errorf("re-pinning to the workspace the agent already chose was refused: %v", err)
	}
}

// TestExplicitConnectionPinIsAttributedToItsAgent is the narrow invariant behind
// the test above: when an identified caller's explicit session_start lands on
// the connection-level pin, it is recorded under that caller's logical-agent id
// as well, so the shard built later restores the pin the agent actually chose.
func TestExplicitConnectionPinIsAttributedToItsAgent(t *testing.T) {
	store, ss := newOriginStore(t)
	_, worktree := worktreeUnderParent(t)
	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")

	s := newPersistSession(t, store, ss, "proxy-attrib")
	if _, err := s.repinWorkspace(ctxAgent, worktree, "", false); err != nil {
		t.Fatalf("explicit pin: %v", err)
	}

	root, _, origin, ok, err := ss.LoadPinForAgent("proxy-attrib", "agent-A")
	if err != nil {
		t.Fatalf("LoadPinForAgent: %v", err)
	}
	if !ok {
		t.Fatal("an explicit session_start naming a workspace left no per-agent pin, so the agent's own shard cannot restore it later")
	}
	if root != worktree {
		t.Errorf("per-agent pin = %q, want %q", root, worktree)
	}
	if origin != sessionstate.PinSourceSessionStart {
		t.Errorf("per-agent pin origin = %q, want %q", origin, sessionstate.PinSourceSessionStart)
	}
}

// TestUnattributedPinIsNotAttributedToAnAgent is the other half: a roots
// notification or a reconnect replay carries no caller identity, and must not
// be written into any agent's per-agent pin row.
func TestUnattributedPinIsNotAttributedToAnAgent(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, _ := worktreeUnderParent(t)

	s := newPersistSession(t, store, ss, "proxy-anon")
	if _, err := s.repinWorkspace(context.Background(), parent, "", false); err != nil {
		t.Fatalf("unattributed pin: %v", err)
	}

	if _, _, _, ok, err := ss.LoadPinForAgent("proxy-anon", "agent-A"); err != nil || ok {
		t.Fatalf("an unattributed pin was attributed to an agent (ok=%v err=%v)", ok, err)
	}
	root, _, _, ok, err := ss.LoadPin("proxy-anon")
	if err != nil || !ok || root != parent {
		t.Fatalf("connection-level pin = %q ok=%v err=%v, want %q", root, ok, err, parent)
	}
}

// TestConfirmingASeededWorkspaceStopsTheShardFollowing is the third defect
// (issue #468): repinAgent's same-root early return fires BEFORE selfPinned is
// set, so an agent whose explicit session_start names the root its shard was
// already seeded at never records that it chose one. Its shard keeps following
// the connection, and a later connection move — a roots notification, or an
// anonymous forced re-pin — drags the agent off a workspace it had explicitly
// named, with no call of its own in between.
//
// The comment sitting under that early return has always claimed this case:
// "the agent has CHOSEN this root (even back to the seeded one, via a
// deliberate re-pin)". TestSelfPinnedShardDoesNotFollowTheConnection only ever
// exercised the CHANGED-root path, which is why the contradiction survived.
func TestConfirmingASeededWorkspaceStopsTheShardFollowing(t *testing.T) {
	store, ss := newOriginStore(t)
	parent, worktree := worktreeUnderParent(t)

	s := newPersistSession(t, store, ss, "proxy-confirm")
	// The connection pins the worktree, and the agent's shard is seeded there.
	if _, err := s.repinWorkspace(context.Background(), worktree, "", false); err != nil {
		t.Fatalf("connection pin to the worktree: %v", err)
	}
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentAttach("agent-A")
	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")
	if got := s.workspaceFor(ctxAgent); got != worktree {
		t.Fatalf("precondition: the shard seeded at %q, want %q", got, worktree)
	}

	// The agent names that workspace explicitly — a deliberate choice, even
	// though nothing moves.
	if _, err := s.repinWorkspace(ctxAgent, worktree, "", false); err != nil {
		t.Fatalf("the agent's same-root session_start: %v", err)
	}

	// The connection now moves elsewhere without the agent being involved.
	if _, err := s.repinWorkspace(context.Background(), parent, "", true); err != nil {
		t.Fatalf("connection move to the parent checkout: %v", err)
	}

	if got := s.workspaceFor(ctxAgent); got != worktree {
		t.Errorf("the connection's move dragged the agent to %q; it explicitly named %q, so a workspace-relative read now returns %q",
			got, worktree, filepath.Join(got, "notes.md"))
	}
}

// TestConfirmedWorkspaceIsPersisted: a shard is only persisted when repinAgent
// moves it, so a choice recorded solely in memory evaporates on the next daemon
// restart — the shard re-seeds from the connection and the fix above evaporates
// with it.
func TestConfirmedWorkspaceIsPersisted(t *testing.T) {
	store, ss := newOriginStore(t)
	_, worktree := worktreeUnderParent(t)

	s := newPersistSession(t, store, ss, "proxy-confirm-persist")
	if _, err := s.repinWorkspace(context.Background(), worktree, "", false); err != nil {
		t.Fatalf("connection pin: %v", err)
	}
	s.recordLogicalAgentAttach("coordinator")
	s.recordLogicalAgentAttach("agent-A")
	ctxAgent := mcp.WithLogicalAgent(context.Background(), "agent-A")
	_ = s.workspaceFor(ctxAgent) // seed the shard
	if _, err := s.repinWorkspace(ctxAgent, worktree, "", false); err != nil {
		t.Fatalf("the agent's same-root session_start: %v", err)
	}

	root, _, origin, ok, err := ss.LoadPinForAgent("proxy-confirm-persist", "agent-A")
	if err != nil {
		t.Fatalf("LoadPinForAgent: %v", err)
	}
	if !ok || root != worktree {
		t.Fatalf("per-agent pin = %q ok=%v, want %q — a confirmed workspace must survive a restart", root, ok, worktree)
	}
	if origin != sessionstate.PinSourceSessionStart {
		t.Errorf("per-agent pin origin = %q, want %q", origin, sessionstate.PinSourceSessionStart)
	}
}
