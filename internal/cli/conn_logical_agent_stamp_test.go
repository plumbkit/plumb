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
	// The gap this table had: every case above declares through the per-call
	// channel, so every connection it builds is armed. A client that declares
	// only at attach — the one whose write lane the ceiling took down — was
	// never represented, so the note and the gate could disagree about it
	// without failing anything.
	attachOnly := []struct {
		name     string
		declare  []string
		callerID string
	}{
		{name: "attach-only, anonymous call", declare: []string{"a", "b"}, callerID: ""},
		{name: "attach-only, identified call", declare: []string{"a", "b"}, callerID: "a"},
	}
	for _, tc := range attachOnly {
		t.Run(tc.name, func(t *testing.T) {
			var s connSession
			for _, id := range tc.declare {
				s.recordLogicalAgentAttach(id)
			}
			ctx := mcp.WithLogicalAgent(context.Background(), tc.callerID)
			st := s.stampChannelState(ctx)
			noteWarnsRefused := st.Shared && !st.PerCallStamped
			gateRefuses := s.refuseSharedStateChange(ctx, "write_file", tc.callerID) != nil
			if noteWarnsRefused != gateRefuses {
				t.Errorf("note says refusing=%v but the gate says refusing=%v (state %+v)", noteWarnsRefused, gateRefuses, st)
			}
		})
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

// A guard that cannot be satisfied is not a guard. The ceiling refuses an
// unattributable state-changing call so the write can be ROUTED to the agent
// that issued it — PLAN-440 is explicit that acceptance (a) is "routing, not
// authorisation". On a connection where NO caller has ever presented a per-call
// identity, refusing achieves no routing: there is no address to route to, and
// no call will ever carry one, so every write is refused forever and the
// refusal's remedy cannot be followed.
//
// That is the shape of the field outage: local-agent-mode-plumb drops the
// PreToolUse argument rewrite, so its calls are permanently anonymous, and
// arming the ceiling took the write lane down entirely.
//
// So the ceiling arms on DEMONSTRATED capability: once any caller on this
// connection has stamped a call, the channel provably works, an anonymous call
// is a real attribution gap, and it is refused exactly as before.
func TestCeilingDoesNotArmWhereNoCallerCanEverStamp(t *testing.T) {
	var s connSession
	// Two agents, declared the only way this client can: through session_start.
	// Neither ever carries a per-call identity, because its runtime cannot.
	s.recordLogicalAgentAttach("conversation-a")
	s.recordLogicalAgentAttach("conversation-b")

	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Errorf("a client that can never stamp was refused its writes with no remedy it could apply: %v", err)
	}
}

// The other side, and the reason this is not simply a hole: the moment ANY
// caller demonstrates the channel works, an anonymous call is a genuine
// attribution gap rather than a client limitation, and the ceiling arms.
func TestCeilingArmsOnceAnyCallerHasStamped(t *testing.T) {
	var s connSession
	s.recordLogicalAgentAttach("conversation-a")
	s.recordLogicalAgentAttach("conversation-b")
	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Fatalf("precondition: an unstamped connection should not be armed: %v", err)
	}

	// One stamped call proves the client can address its agents.
	s.recordLogicalAgentCall("conversation-a")

	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Error("once the per-call channel is proven to work, an anonymous write is an attribution gap and must be refused")
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "conversation-b"); err != nil {
		t.Errorf("an attributed write must still be admitted: %v", err)
	}
}
