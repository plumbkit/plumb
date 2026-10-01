package cli

// conn_repin_scope_subagent_test.go — a connection-scoped re-pin's report must
// name the root a SUBAGENT caller really resolves against afterwards (#535
// review of the merge with #533). The report decided "follows the connection"
// from the shard's own flags alone, while followConnectionShards also leaves a
// subagent on its conversation's chosen root; the report then told the
// subagent it worked in the connection's new root while its relative paths
// still landed in its conversation's worktree.

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// Every case ends with one connection-scoped move to `other` made BY a
// subagent, and asserts that what the move reports as the caller's next root is
// what workspaceFor then resolves for it.
func TestConnScopeReportMatchesWhereASubagentResolves(t *testing.T) {
	cases := []struct {
		name string
		// setup arranges the connection and returns the expected next root,
		// given the three roots newThreeRootConn made.
		setup func(t *testing.T, s *connSession, rootMain, worktree, other string) string
		// persisted builds the session over a durable store instead.
		persisted bool
	}{
		{
			// The reviewer's reproduction: the conversation chose a worktree,
			// the subagent sits on it, so the connection move must leave it there.
			name: "existing shard on its conversation's chosen root",
			setup: func(t *testing.T, s *connSession, _, worktree, _ string) string {
				t.Helper()
				repinOrFail(t, s, "conv-y", worktree)
				if got := s.workspaceFor(idCtx("conv-y/sub")); got != worktree {
					t.Fatalf("precondition: sub on its conversation's worktree, got %q", got)
				}
				return worktree
			},
		},
		{
			// No shard yet: the next call seeds it from the conversation's
			// chosen root, not from the connection's new one.
			name: "no shard yet, conversation chose a root",
			setup: func(t *testing.T, s *connSession, _, worktree, _ string) string {
				t.Helper()
				repinOrFail(t, s, "conv-y", worktree)
				return worktree
			},
		},
		{
			// Positive control: a subagent whose conversation never chose a
			// root follows the connection, so the new root is the true answer.
			name: "conversation never chose: the subagent follows",
			setup: func(t *testing.T, s *connSession, rootMain, _, other string) string {
				t.Helper()
				if got := s.workspaceFor(idCtx("conv-y/sub")); got != rootMain {
					t.Fatalf("precondition: sub on the connection root, got %q", got)
				}
				return other
			},
		},
		{
			// The conversation's refused declaration is inherited: the
			// subagent resolves against nothing, and the report must say so.
			name: "conversation's declaration refused",
			setup: func(t *testing.T, s *connSession, rootMain, worktree, _ string) string {
				t.Helper()
				// An explicit (sticky) connection pin, so the seeded shard's
				// unforced move to an unrelated root is refused and recorded.
				if _, err := s.repinWorkspace(context.Background(), "file://"+rootMain, "", false, false); err != nil {
					t.Fatalf("explicit connection pin: %v", err)
				}
				if _, err := s.repinWorkspace(idCtx("conv-y"), "file://"+worktree, "", false, false); err == nil {
					t.Fatal("precondition: an unforced move of a seeded shard to an unrelated root is refused")
				}
				return ""
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, rootMain, worktree, other := newThreeRootConn(t)
			want := tc.setup(t, s, rootMain, worktree, other)
			assertConnScopeReportTrue(t, s, other, want)
		})
	}
}

// After a daemon restart the conversation is not in memory, so a new subagent
// shard is seeded from the conversation's PERSISTED pin (parentSeeded) and a
// connection move must neither drag it nor report that it did.
func TestConnScopeReportMatchesAParentSeededSubagent(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-535-parent-seeded"
	r := repinReportRoots(t, 3)
	rootMain, worktree, other := r[0], r[1], r[2]
	if err := ss.UpsertPinForAgent(proxyID, "conv-y", worktree, "", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist the conversation's pin: %v", err)
	}
	s := newPersistSession(t, store, ss, proxyID)
	newSharedConn(t, s, rootMain, "conv-x", "conv-y")
	if got := s.workspaceFor(idCtx("conv-y/sub")); got != worktree {
		t.Fatalf("precondition: sub seeded from its conversation's persisted pin, got %q", got)
	}
	assertConnScopeReportTrue(t, s, other, worktree)
}

// The rendered line is what the agent reads, so check it end to end once: the
// subagent is told it stays on its conversation's worktree, labelled as not the
// connection's pin, and that is where its relative paths go.
func TestConnScopeReportRendersASubagentsOwnRoot(t *testing.T) {
	s, _, worktree, other := newThreeRootConn(t)
	repinOrFail(t, s, "conv-y", worktree)
	ctxSub := idCtx("conv-y/sub")
	out := runRepinReport(t, s, ctxSub, map[string]any{"workspace": other, "scope": "connection", "force": true}, "brief")
	wantLines(t, out, "Next relative-path call resolves against: "+worktree+" (your own pin, not the connection's)\n")
	if got := s.workspaceFor(ctxSub); got != worktree {
		t.Errorf("sub resolves to %q, want its conversation's worktree %q", got, worktree)
	}
	if got := s.workspace(); got != other {
		t.Errorf("control: the connection pin = %q, want it moved to %q", got, other)
	}
}

func repinOrFail(t *testing.T, s *connSession, id, root string) {
	t.Helper()
	if _, err := s.repinWorkspace(idCtx(id), "file://"+root, "", true, false); err != nil {
		t.Fatalf("%s re-pin to %s: %v", id, root, err)
	}
}

// assertConnScopeReportTrue has conv-y/sub move the connection to other and
// checks the report's next root against both want and what workspaceFor
// resolves for the subagent afterwards.
func assertConnScopeReportTrue(t *testing.T, s *connSession, other, want string) {
	t.Helper()
	ctxSub := idCtx("conv-y/sub")
	out, err := s.repinConnection(ctxSub, "file://"+other, "", true)
	if err != nil {
		t.Fatalf("connection-scoped move: %v", err)
	}
	if got := s.workspace(); got != other {
		t.Fatalf("precondition: the connection moved to %q, got %q", other, got)
	}
	actual := s.workspaceFor(ctxSub)
	if out.effective != actual {
		t.Errorf("the report tells the subagent it resolves against %q, but its relative paths resolve against %q", out.effective, actual)
	}
	if actual != want {
		t.Errorf("the subagent resolves against %q, want %q", actual, want)
	}
}
