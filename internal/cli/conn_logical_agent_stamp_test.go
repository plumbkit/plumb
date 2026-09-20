package cli

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

// The note session_start emits and the gate that refuses the write must answer
// the same question. Restating the predicate in the wiring is how they drift:
// invert it, or swap sharedWith for refuse, and the orientation packet starts
// promising writes that are about to be refused — or worse, stays silent while
// they are. So this derives the expectation from refuseSharedStateChange rather
// than hard-coding it, which is PLAN-440 item 1's standing lesson applied to the
// one piece of wiring that had no test.
func TestStampChannelStateAgreesWithTheWriteGate(t *testing.T) {
	cases := []struct {
		name     string
		declare  []string
		callerID string
	}{
		{name: "no identity at all", declare: nil, callerID: ""},
		{name: "single agent, anonymous call", declare: []string{"only"}, callerID: ""},
		{name: "single agent, identified call", declare: []string{"only"}, callerID: "only"},
		{name: "shared, anonymous call", declare: []string{"a", "b"}, callerID: ""},
		{name: "shared, identified call", declare: []string{"a", "b"}, callerID: "a"},
		{name: "shared, stranger identified", declare: []string{"a", "b"}, callerID: "c"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s connSession
			for _, id := range tc.declare {
				s.recordLogicalAgentCall(id)
			}
			ctx := mcp.WithLogicalAgent(context.Background(), tc.callerID)

			st := s.stampChannelState(ctx)
			// "The note would warn that writes are being refused" must hold
			// exactly when the gate would in fact refuse one.
			noteWarnsRefused := st.Shared && !st.PerCallStamped
			gateRefuses := s.refuseSharedStateChange(ctx, "write_file", tc.callerID) != nil

			if noteWarnsRefused != gateRefuses {
				t.Errorf("note says refusing=%v but the gate says refusing=%v (state %+v)", noteWarnsRefused, gateRefuses, st)
			}
		})
	}
}

// PerCallStamped is the fact the whole disclosure turns on, so it is pinned
// against the ctx directly rather than inferred from the parity above: a state
// that got both fields wrong in the same direction would satisfy parity alone.
func TestStampChannelStateReadsTheIdentityFromCtx(t *testing.T) {
	var s connSession
	s.recordLogicalAgentCall("a")
	s.recordLogicalAgentCall("b")

	if st := s.stampChannelState(mcp.WithLogicalAgent(context.Background(), "a")); !st.PerCallStamped {
		t.Error("a call carrying an identity must read as stamped")
	}
	if st := s.stampChannelState(context.Background()); st.PerCallStamped {
		t.Error("a call carrying no identity must not read as stamped")
	}
	if st := s.stampChannelState(context.Background()); !st.Shared {
		t.Error("two declared identities must read as a shared connection")
	}
}

// The user's opt-out. The ceiling refuses an unattributable state-changing call
// and tells the caller to identify itself — advice that assumes a channel to do
// it with. A client whose runtime drops the per-call stamp has none, so on a
// shared connection every write is refused permanently and the remedy cannot be
// followed. That is an outage, not a guard, and the user must be able to accept
// the attribution risk on their own machine.
func TestAllowUnidentifiedWritesLiftsTheCeiling(t *testing.T) {
	var s connSession
	s.recordLogicalAgentCall("a")
	s.recordLogicalAgentCall("b")

	// Default: refused, exactly as before.
	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Fatal("precondition: an anonymous write on a shared connection must refuse by default")
	}

	s.setCollabAllowUnidentifiedWritesForTest(true)

	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Errorf("with the opt-out set the write must be admitted: %v", err)
	}
	// Reads were never refused and must stay that way.
	if err := s.refuseSharedStateChange(context.Background(), "read_file", ""); err != nil {
		t.Errorf("a read must never refuse: %v", err)
	}
}
