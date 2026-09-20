//go:build integration

package cli

// multiagent_force_guard_integration_test.go — the anonymous forced re-pin.
//
// session_start is deliberately absent from the shared-connection write gate:
// it is the only channel by which an agent on a client that cannot stamp a
// per-call identity declares itself, so refusing it anonymously would make
// identity undeclarable and the connection permanently unusable. cb71cf33
// recorded that exemption as safe on the grounds that the state change is
// "guarded where it happens, by repinAgent's sticky-pin guard".
//
// That reasoning does not hold for the case it most needed to cover. repinShard
// returns nil when the call carries no logical-agent identity, so repinAgent —
// and its guard — never runs for an anonymous caller. The call falls through to
// the CONNECTION-level re-pin, where `force: true` bypasses the sticky guard by
// design, and followConnectionShards then drags every peer shard that has not
// pinned a root of its own onto the new one, resetting its read, write and undo
// state on the way.
//
// The result is a state change across several agents, performed by a caller
// that cannot be attributed to any of them — which is precisely what acceptance
// (a) says must not happen. The exemption is still right; the claim that
// something else was guarding it was not.
//
// WHY THIS IS SKIPPED RATHER THAN FIXED.
//
// A guard refusing the anonymous forced move was written and it worked — and it
// broke three of PLAN-398's regression tests, because PLAN-398 DELIBERATELY
// makes a seeded shard follow exactly this move so that an agent is not
// stranded on a stale root (TestShardSeededBeforeRefusalFollowsTheConnection,
// TestSelfPinnedShardDoesNotFollowTheConnection,
// TestConfirmingASeededWorkspaceStopsTheShardFollowing). On a connection whose
// pin came from session_start, force is the ONLY way that pin moves: a roots
// notification re-pins unforced (conn_roots.go) and is refused by the sticky
// guard, and an IDENTIFIED caller routes to repinAgent and moves its own shard
// instead, never the connection. So refusing the anonymous forced move freezes
// a shared connection's pin permanently.
//
// That is a conflict between two shipped designs, not a defect with an obvious
// fix, and PLAN-440 lists "session_start (with workspace/force)" as a KNOWN gap
// in the gate's coverage. It needs an owner decision: either acceptance (a) is
// narrowed to exclude the connection's own pin, or PLAN-398's follow behaviour
// gives up the anonymous route and agents re-declare instead.
//
// The reproduction stays here, skipped, so the gap is visible in the suite
// rather than surviving only as a sentence in a card.

import (
	"path/filepath"
	"testing"
)

func TestAnonymousForcedRepinIsRefusedOnASharedConnection(t *testing.T) {
	t.Skip("OPEN GAP, reproduction kept deliberately — see the note below. " +
		"Closing it conflicts with PLAN-398's shipped behaviour and needs an owner decision.")

	m := newMultiAgentConn(t)
	wsA, wsC := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, wsA)
	mustGitDir(t, wsC)

	// Two DECLARED agents: the connection is genuinely shared, so the gate is
	// armed for every other state-changing tool.
	if err := m.sessionStart(t, map[string]any{"session_id": "coordinator", "workspace": wsA}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"session_id": "subagent"}); err != nil {
		t.Fatalf("subagent session_start: %v", err)
	}
	if !committedShared(m.s) {
		t.Fatal("precondition: two declared identities should make the connection shared")
	}
	pinned := m.s.workspace()
	if filepath.Clean(pinned) != filepath.Clean(wsA) {
		t.Fatalf("precondition: connection pinned to %q, want %q", pinned, wsA)
	}

	// The attack: no session_id, no per-call identity, but force: true.
	err := m.sessionStart(t, map[string]any{"workspace": wsC, "force": true})

	if err == nil {
		t.Error("an anonymous forced re-pin was ACCEPTED on a shared connection; " +
			"an unattributable caller must not move state that belongs to several agents")
	}
	if got := m.s.workspace(); filepath.Clean(got) != filepath.Clean(wsA) {
		t.Errorf("the connection pin moved to %q under an anonymous forced re-pin; want it held at %q", got, wsA)
	}
}

// The exemption itself must survive: a bare session_start, and one carrying a
// session_id, are how an agent declares itself on a client that cannot stamp.
// Refusing those would make identity undeclarable and the connection unusable,
// which is a far worse failure than the one being closed.
func TestAnonymousDeclarationsStillWorkOnASharedConnection(t *testing.T) {
	m := newMultiAgentConn(t)
	wsA := freshTempDir(t)
	mustGitDir(t, wsA)

	if err := m.sessionStart(t, map[string]any{"session_id": "coordinator", "workspace": wsA}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"session_id": "subagent"}); err != nil {
		t.Fatalf("subagent session_start: %v", err)
	}
	if !committedShared(m.s) {
		t.Fatal("precondition: the connection should be shared")
	}

	// A bare orientation call: no identity, no workspace, no force.
	if err := m.sessionStart(t, map[string]any{}); err != nil {
		t.Errorf("a bare session_start must never be refused — it is how an agent orients: %v", err)
	}
	// A declaration: no per-call stamp, but it names itself.
	if err := m.sessionStart(t, map[string]any{"session_id": "third"}); err != nil {
		t.Errorf("a session_start declaring an identity must not be refused: %v", err)
	}
}
