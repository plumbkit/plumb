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
// WHY THIS COULD NOT BE CLOSED BY A GUARD ALONE.
//
// Refusing the anonymous forced move breaks PLAN-398, which DELIBERATELY makes
// a seeded shard follow exactly this move so an agent is not stranded on a
// stale root. On a connection pinned by session_start, force was the ONLY way
// that pin moved: a roots notification re-pins unforced and the sticky guard
// refuses it, and an IDENTIFIED caller routed to repinAgent and moved its own
// shard, never the connection. Refusing the anonymous route therefore froze a
// shared connection's pin permanently — a guard that closes a hole by removing
// a capability.
//
// The missing piece was an ATTRIBUTABLE way to move the connection. session_start
// now takes scope: "connection", available to an identified caller, so the move
// has an author. With that in place the anonymous route is refused and nothing
// is lost: PLAN-398's follow still happens, driven by a caller who can be named.

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAnonymousForcedRepinIsRefusedOnASharedConnection(t *testing.T) {
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
	if err != nil && !strings.Contains(err.Error(), "connection") {
		t.Errorf("the refusal must point at the attributable route (scope: \"connection\"), got: %v", err)
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

// The capability that makes refusing the anonymous route acceptable: an agent
// that identifies itself CAN move the connection's pin, and the move is
// attributed to it. Operator-only would have been too restrictive — agents
// legitimately need to reposition the connection they share.
func TestIdentifiedAgentCanMoveTheConnectionPin(t *testing.T) {
	m := newMultiAgentConn(t)
	wsA, wsC := freshTempDir(t), freshTempDir(t)
	mustGitDir(t, wsA)
	mustGitDir(t, wsC)

	if err := m.sessionStart(t, map[string]any{"session_id": "coordinator", "workspace": wsA}); err != nil {
		t.Fatalf("coordinator session_start: %v", err)
	}
	if err := m.sessionStart(t, map[string]any{"session_id": "subagent"}); err != nil {
		t.Fatalf("subagent session_start: %v", err)
	}

	// An identified agent asking for the CONNECTION's pin, not its own shard.
	if err := m.sessionStart(t, map[string]any{
		"session_id": "coordinator", "workspace": wsC, "force": true, "scope": "connection",
	}); err != nil {
		t.Fatalf("an identified agent must be able to move the connection pin: %v", err)
	}
	if got := m.s.workspace(); filepath.Clean(got) != filepath.Clean(wsC) {
		t.Errorf("connection pin = %q, want %q", got, wsC)
	}
}
