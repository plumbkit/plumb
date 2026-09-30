package cli

import (
	"context"
	"strings"
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

// [collab] allow_unidentified_writes is retired. It lifted the ceiling, and on
// Claude desktop's connector (whose stamp was dropped in transit) that let an
// anonymous edit resolve through the connection pin into another agent's
// checkout (2026-09-30). A user who still has it set is told it is ignored.
func TestAllowUnidentifiedWritesIsRetired(t *testing.T) {
	var s connSession
	s.recordLogicalAgentCall("a")
	s.recordLogicalAgentCall("b")

	err := s.refuseSharedStateChange(context.Background(), "write_file", "")
	if err == nil {
		t.Fatal("an anonymous write on a shared connection must refuse")
	}
	if strings.Contains(err.Error(), "allow_unidentified_writes") {
		t.Errorf("with the key unset the refusal must not mention it: %v", err)
	}

	s.setCollabAllowUnidentifiedWritesForTest(true)
	err = s.refuseSharedStateChange(context.Background(), "write_file", "")
	if err == nil {
		t.Fatal("the retired opt-out still lifted the ceiling")
	}
	if !strings.Contains(err.Error(), "allow_unidentified_writes is set but no longer honoured") {
		t.Errorf("the refusal must say the opt-out is ignored: %v", err)
	}
	if err := s.refuseSharedStateChange(context.Background(), "read_file", ""); err != nil {
		t.Errorf("a read must never refuse: %v", err)
	}
}

// The ceiling arms on two identities, however they were declared. The
// exemption for "no caller has ever stamped" is gone: it was the path the
// 2026-09-30 incident took, where two conversations declared through
// session_start and every write resolved through one connection pin.
func TestCeilingArmsWhenAgentsDeclaredOnlyAtAttach(t *testing.T) {
	var s connSession
	s.recordLogicalAgentAttach("conversation-a")
	s.recordLogicalAgentAttach("conversation-b")

	err := s.refuseSharedStateChange(context.Background(), "write_file", "")
	if err == nil {
		t.Fatal("an anonymous write on a connection two agents declared must refuse")
	}
	for _, want := range []string{"plumb hooks install claude-code", mcp.MetaLogicalAgentKey, "one plumb serve per agent"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name the remedy %q: %v", want, err)
		}
	}
	// Not "pass plumb_agent yourself": without the hook an invented id is
	// admitted as a fresh agent on the connection root, the misroute itself.
	if strings.Contains(err.Error(), mcp.ArgLogicalAgentDeclaredKey) {
		t.Errorf("the refusal must not invite typing %s: %v", mcp.ArgLogicalAgentDeclaredKey, err)
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "conversation-b"); err != nil {
		t.Errorf("an attributed write must be admitted: %v", err)
	}
}

// A single identity is not a shared connection: nothing is refused.
func TestCeilingStaysDownForOneAgent(t *testing.T) {
	var s connSession
	s.recordLogicalAgentAttach("only")
	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Errorf("a single-agent connection refused an anonymous write: %v", err)
	}
}
