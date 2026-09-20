package cli

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// logicalAgentState.seen is in-memory and scoped to the connection's life, so a
// daemon restart made a shared connection read as UNSHARED: len(seen) <= 1, and
// refuse admits every anonymous state-changing call until two identities happen
// to re-declare. The window is not theoretical — after a restart, clients
// reconnect and start calling before they re-declare, which is exactly when the
// fail-closed ceiling is supposed to be holding.
//
// Seeding from the per-agent pins already persisted under this proxy session
// closes it. PLAN-440 item 2.

func TestSeedArmsTheGateBeforeAnyAgentRedeclares(t *testing.T) {
	var l logicalAgentState
	l.seed([]string{"coordinator", "subagent"})

	if !l.refuse("") {
		t.Error("a connection with two persisted identities must refuse an anonymous state-changing call immediately after a restart")
	}
	if l.refuse("coordinator") {
		t.Error("an identified call must still be admitted")
	}
}

// A single persisted identity is not a shared connection: the connection IS
// that agent, and refusing its anonymous calls would break every single-agent
// client that reconnects.
func TestSeedWithOneIdentityDoesNotArmTheGate(t *testing.T) {
	var l logicalAgentState
	l.seed([]string{"only"})

	if l.refuse("") {
		t.Error("one persisted identity must not make the connection read as shared")
	}
}

// The documented invariant: seen only grows, so a later re-check cannot un-see
// an ID and flip the shared flag back off. A seed carrying a NARROWER durable
// view than what this connection has already observed must not shrink it —
// otherwise a pruned or partially-written pin table would silently disarm a
// gate that is currently holding.
func TestSeedCannotUnseeAnIdentityAlreadyObserved(t *testing.T) {
	var l logicalAgentState
	l.record("a")
	l.record("b")
	if !l.refuse("") {
		t.Fatal("precondition: two observed identities should arm the gate")
	}

	l.seed([]string{"a"})

	if !l.refuse("") {
		t.Error("seed shrank the observed set and disarmed the gate; seen must only grow")
	}
}

// An empty or absent durable view is simply no information, not evidence that
// the connection is unshared.
func TestSeedWithNothingIsANoOp(t *testing.T) {
	var l logicalAgentState
	l.record("a")
	l.seed(nil)
	l.seed([]string{"", "  "})

	if l.refuse("") {
		t.Error("seeding nothing must not invent a second identity")
	}
	if got := l.count(); got != 1 {
		t.Errorf("blank ids must not be recorded; observed %d identities, want 1", got)
	}
}

// The restart, end to end: two agents recorded pins under this proxy session
// before the daemon went down, so the reconnecting connection must refuse an
// unattributable write from its very first call — not from whenever the second
// agent happens to re-declare.
func TestReconnectAfterRestartRefusesAnonymousWritesImmediately(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-restart-arming"
	ws := freshTempDir(t)
	if err := ss.UpsertPinForAgent(proxyID, "coordinator", ws, "go", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist coordinator pin: %v", err)
	}
	if err := ss.UpsertPinForAgent(proxyID, "subagent", ws, "go", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist subagent pin: %v", err)
	}

	// newPersistSession fires onProxySession, exactly as handleInitialize does.
	s := newPersistSession(t, store, ss, proxyID)

	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Error("a reconnecting shared connection admitted an anonymous write before any agent re-declared")
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "coordinator"); err != nil {
		t.Errorf("an identified write must still be admitted: %v", err)
	}
	if err := s.refuseSharedStateChange(context.Background(), "read_file", ""); err != nil {
		t.Errorf("a read must never be refused: %v", err)
	}
}

// The control: one persisted agent is not a shared connection. Arming here
// would refuse every single-agent client's anonymous writes after a restart,
// which is a far worse failure than the one being fixed.
func TestReconnectWithOneAgentStillAdmitsAnonymousWrites(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-restart-single"
	ws := freshTempDir(t)
	if err := ss.UpsertPinForAgent(proxyID, "only", ws, "go", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist pin: %v", err)
	}

	s := newPersistSession(t, store, ss, proxyID)

	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Errorf("a single-agent connection must still admit its own anonymous write: %v", err)
	}
}

// The seed's durable source must cover how agents actually declare themselves.
// A pinned_workspace row is written only when an agent's own session_start
// named a workspace; a subagent that identifies itself with a per-call _meta
// stamp, or with session_start carrying just a session_id, inherits the
// connection's pin and writes no row of its own. That is the COMMON topology —
// a coordinator that named a workspace and a subagent that did not — so a seed
// reading pins alone sees one identity, skips, and leaves the gate disarmed
// after a restart for exactly the case item 2 exists to cover.
func TestReconnectArmsWhenOnlyOneAgentEverPinnedAWorkspace(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-restart-mixed-declaration"
	ws := freshTempDir(t)

	// Before the restart: the coordinator named a workspace; the subagent only
	// ever declared an identity.
	if err := ss.UpsertPinForAgent(proxyID, "coordinator", ws, "go", sessionstate.PinSourceSessionStart); err != nil {
		t.Fatalf("persist coordinator pin: %v", err)
	}
	if err := ss.RecordLogicalAgent(proxyID, "coordinator"); err != nil {
		t.Fatalf("record coordinator identity: %v", err)
	}
	if err := ss.RecordLogicalAgent(proxyID, "subagent"); err != nil {
		t.Fatalf("record subagent identity: %v", err)
	}

	s := newPersistSession(t, store, ss, proxyID)

	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Error("a connection whose second agent never pinned a workspace came back disarmed; " +
			"the seed's durable source must cover every declaration channel, not just pins")
	}
}

// End to end through the LIVE path: the declarations are made on a real
// connection, which must record them itself, and a second connection on the
// same proxy session — the restart — must come back armed. Without this, a
// persistLogicalAgent that was never wired in would still pass every test
// above, because they seed the store by hand.
func TestDeclarationsMadeOnALiveConnectionSurviveTheRestart(t *testing.T) {
	store, ss := newOriginStore(t)
	const proxyID = "proxy-live-declarations"

	before := newPersistSession(t, store, ss, proxyID)
	before.recordLogicalAgentCall("coordinator")
	before.recordLogicalAgentCall("subagent")
	if err := before.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Fatal("precondition: the live connection should already be refusing anonymous writes")
	}

	// The restart: a fresh connection adopting the same proxy session.
	after := newPersistSession(t, store, ss, proxyID)

	if err := after.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Error("the reconnecting connection came back disarmed; declarations made on the live " +
			"connection were not durably recorded")
	}
}
