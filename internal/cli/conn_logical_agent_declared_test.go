package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/tools"
)

// Issue #513: on a shared connection, an identity nobody declared through
// session_start used to be admitted, handed a fresh shard seeded from the
// connection's root, and its relative write landed in the connection's
// workspace. The gate now asks whether the identity's LINKAGE was declared.
func TestRefuseUndeclaredIdentityOnSharedConnection(t *testing.T) {
	var s connSession
	s.recordLogicalAgentAttach("conv-a") // session_start(session_id: conv-a)
	s.recordLogicalAgentAttach("conv-b") // session_start(session_id: conv-b)

	err := s.refuseSharedStateChange(context.Background(), "write_file", "made-up")
	if err == nil {
		t.Fatal("a write under an identity no session_start declared was admitted on a shared connection")
	}
	// The refusal must name a remedy the caller can reach from inside a tool
	// call: session_start is not state-changing, so it is never refused.
	for _, want := range []string{"session_start", `"made-up"`, "no session_start on this connection has declared it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q: %v", want, err)
		}
	}
	if slices.Contains(tools.StateChangingToolNames(), "session_start") {
		t.Fatal("session_start became state-changing; the remedy the refusal names is no longer reachable")
	}
	// Reads are never refused, declared or not.
	if err := s.refuseSharedStateChange(context.Background(), "read_file", "made-up"); err != nil {
		t.Errorf("a read under an undeclared identity must not be refused: %v", err)
	}

	// Positive controls: the declared agents and a hook-stamped subagent of a
	// declared conversation are admitted. Without them the refusal above could
	// be the gate refusing every identified call.
	for _, id := range []string{"conv-a", "conv-b", "conv-a/agent-1"} {
		if err := s.refuseSharedStateChange(context.Background(), "write_file", id); err != nil {
			t.Errorf("%s must be admitted: %v", id, err)
		}
	}
	// A subagent whose CONVERSATION was never declared is not vouched for.
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "made-up/agent-1"); err == nil {
		t.Error("a subagent of an undeclared conversation was admitted")
	}

	// The remedy works: once session_start runs under the identity, it is admitted.
	s.declareSessionStartCaller(mcp.WithLogicalAgent(context.Background(), "made-up"), "session_start", false)
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "made-up"); err != nil {
		t.Errorf("an identity declared by a successful session_start must be admitted: %v", err)
	}
}

// Only a SUCCESSFUL session_start declares: a failed one (its re-pin refused)
// never attached, and no other tool is a declaration.
func TestOnlyASuccessfulSessionStartDeclares(t *testing.T) {
	var s connSession
	s.recordLogicalAgentAttach("conv-a")
	s.recordLogicalAgentAttach("conv-b")
	ctx := mcp.WithLogicalAgent(context.Background(), "made-up")

	s.declareSessionStartCaller(ctx, "session_start", true)
	s.declareSessionStartCaller(ctx, "read_file", false)
	s.declareSessionStartCaller(ctx, "write_file", false)
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "made-up"); err == nil {
		t.Fatal("a failed session_start or a non-session_start call declared the identity")
	}
	s.declareSessionStartCaller(ctx, "session_start", false)
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "made-up"); err != nil {
		t.Fatalf("positive control: a successful session_start must declare: %v", err)
	}
}

// A single-agent connection is unaffected: whether or not the one identity it
// knows ever called session_start, and whatever identity a first call carries.
func TestUndeclaredIdentityOnSingleAgentConnectionIsAdmitted(t *testing.T) {
	var none connSession
	if err := none.refuseSharedStateChange(context.Background(), "write_file", "made-up"); err != nil {
		t.Errorf("the first identity a connection sees is the connection; it must not be refused: %v", err)
	}
	var one connSession
	one.recordLogicalAgentCall("stamped-never-declared")
	if err := one.refuseSharedStateChange(context.Background(), "write_file", "stamped-never-declared"); err != nil {
		t.Errorf("a single-agent connection's own identity must not be refused: %v", err)
	}
	if err := one.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Errorf("a single-agent connection's anonymous call must not be refused: %v", err)
	}
}

// The refusal is judged against the caller COUNTED — the predicate shardFor
// routes on — so the second identity on a connection is refused from its first
// write rather than admitted once (onto a fresh shard of the connection's root)
// and refused only from its second.
func TestUndeclaredSecondIdentityIsRefusedFromItsFirstWrite(t *testing.T) {
	var s connSession
	s.recordLogicalAgentAttach("conv-a")
	if !s.logicalAgents.sharedWith("made-up") {
		t.Fatal("precondition: shardFor would give made-up a shard of its own")
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "made-up"); err == nil {
		t.Fatal("the first write of an undeclared second identity was admitted")
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "conv-a/sub"); err != nil {
		t.Errorf("positive control: a subagent of the declared conversation must be admitted: %v", err)
	}
	// A refusal commits nothing: the connection is still single-agent.
	if got := s.logicalAgents.count(); got != 1 {
		t.Errorf("the gate recorded an identity: %d observed, want 1", got)
	}
}

// A daemon restart must not lock out an agent that declared itself before it.
// The in-memory declared set is gone, while seen refills from the stamps the
// hook keeps sending, so without restoration the connection turns shared again
// and every declared agent's write is refused. An identity that was only ever
// OBSERVED (an invented id's admitted read) must not come back declared.
func TestDeclarationsSurviveADaemonRestart(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-513-restart"

	before := newPersistSession(t, store, ss, proxyID)
	before.linkExternalID("conv-a")      // session_start(session_id: conv-a)
	before.declareLogicalAgent("conv-b") // session_start under conv-b's stamp
	before.recordLogicalAgentCall("made-up")
	before.close()

	after := newPersistSession(t, store, ss, proxyID) // onProxySession restores
	after.recordLogicalAgentCall("conv-a")
	after.recordLogicalAgentCall("conv-b")
	for _, id := range []string{"conv-a", "conv-b", "conv-a/agent-1"} {
		if err := after.refuseSharedStateChange(context.Background(), "write_file", id); err != nil {
			t.Errorf("%s declared before the restart and was refused after it: %v", id, err)
		}
	}
	if err := after.refuseSharedStateChange(context.Background(), "write_file", "made-up"); err == nil {
		t.Error("an identity only ever observed came back declared after the restart")
	}
}

// declared_linkage ages out with the TTL; the identity record's own linkage is
// never pruned, so the conversation the session is linked to stays declared
// even when a restart pruned its row.
func TestIdentityRecordLinkageSurvivesAPrunedDeclaration(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-513-pruned"

	before := newPersistSession(t, store, ss, proxyID)
	before.linkExternalID("conv-a")
	before.declareLogicalAgent("conv-b")
	before.close()
	// Every expendable row is older than this cutoff; session_names is kept.
	if err := ss.Prune(time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("prune: %v", err)
	}

	after := newPersistSession(t, store, ss, proxyID)
	after.recordLogicalAgentCall("conv-a")
	after.recordLogicalAgentCall("conv-b")
	if err := after.refuseSharedStateChange(context.Background(), "write_file", "conv-a"); err != nil {
		t.Errorf("the identity record's linkage must survive a prune: %v", err)
	}
	// The documented limit, and the control that the prune really removed the
	// declared_linkage row: conv-b must re-declare.
	if err := after.refuseSharedStateChange(context.Background(), "write_file", "conv-b"); err == nil {
		t.Error("conv-b's pruned declaration came back; the prune control is vacuous")
	}
}
