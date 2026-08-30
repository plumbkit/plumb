package cli

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
)

func TestLogicalAgentStateRefuse(t *testing.T) {
	var l logicalAgentState
	if l.refuse("") {
		t.Fatal("a single-agent connection must never refuse")
	}
	l.record("A") // attach-time session_id channel
	// One committed identity: the connection IS the agent, so nothing is
	// refused. Note what does NOT explain this — there is no attach-time
	// fallback identity any more (PLAN-394 deleted it); the call is admitted
	// purely because len(seen) <= 1.
	if l.refuse("") {
		t.Fatal("a single-identity connection must not refuse an anonymous call")
	}
	if l.refuse("A") {
		t.Fatal("an explicit call ID must not refuse")
	}
	l.record("B") // a second agent arrives per-call
	if l.refuse("B") {
		t.Fatal("an explicit ID on a shared connection must not refuse")
	}
	// PLAN-394: once the connection is shared the attach-time id is whichever
	// peer attached LAST, not the caller — attributing on its strength was the
	// fail-open, so an anonymous call must refuse no matter how many attached.
	if !l.refuse("") {
		t.Fatal("an anonymous call on a shared connection must refuse; the attach-time id is a peer's, not the caller's")
	}
}

func TestLogicalAgentStateRefuseNoAttach(t *testing.T) {
	var l logicalAgentState
	l.record("A")
	l.record("B") // shared, via per-call identities
	if !l.refuse("") {
		t.Fatal("an anonymous call on a shared, no-attach connection must refuse")
	}
	if l.refuse("A") {
		t.Fatal("an explicit call ID on a shared connection must not refuse")
	}
}

func TestRefuseSharedStateChange(t *testing.T) {
	var s connSession
	s.recordLogicalAgentCall("agent-1")
	s.recordLogicalAgentCall("agent-2") // shared, no attach ID

	if err := s.refuseSharedStateChange(context.Background(), "read_file", ""); err != nil {
		t.Fatalf("a read must never refuse: %v", err)
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Fatal("an anonymous write on a shared connection must refuse")
	}
	if err := s.refuseSharedStateChange(context.Background(), "write_file", "agent-1"); err != nil {
		t.Fatalf("an attributed write must not refuse: %v", err)
	}

	var s2 connSession
	s2.recordLogicalAgentCall("only")
	if err := s2.refuseSharedStateChange(context.Background(), "write_file", ""); err != nil {
		t.Fatalf("a single-agent connection must not refuse an anonymous write: %v", err)
	}

	// PLAN-394: an attach-time session_id does not rescue an anonymous call on a
	// shared connection — that id belongs to whichever agent attached last, so
	// admitting the call on its strength wrote the work into a peer's trackers.
	var s3 connSession
	s3.recordLogicalAgentAttach("coordinator")
	s3.recordLogicalAgentAttach("subagent-last")
	if err := s3.refuseSharedStateChange(context.Background(), "write_file", ""); err == nil {
		t.Fatal("an anonymous write on a shared connection must refuse even with an attach-time id")
	}
	if err := s3.refuseSharedStateChange(context.Background(), "write_file", "coordinator"); err != nil {
		t.Fatalf("an identified write must not refuse: %v", err)
	}
}

// TestRecordLatchesSharedTransition pins PLAN-396's first half: record reports
// the TRANSITION to shared separately from the shared STATE. The transition
// drives the announcement, which the operator needs once; the state drives the
// health note, which must stay true for as long as the condition holds. Before
// the split, record returned only len(seen) > 1 and the announcement re-fired
// on every peer call carrying an identity.
func TestRecordLatchesSharedTransition(t *testing.T) {
	var l logicalAgentState
	if shared, transition := l.record("A"); shared || transition {
		t.Fatalf("the first identity must be neither shared nor a transition: shared=%v transition=%v", shared, transition)
	}
	if shared, transition := l.record("B"); !shared || !transition {
		t.Fatalf("the second distinct identity is the transition into shared: shared=%v transition=%v", shared, transition)
	}
	// Every later declaration still reports SHARED — that is what keeps the
	// health note re-assertable — but never again a transition, which is what
	// keeps the announcement to one.
	if shared, transition := l.record("C"); !shared || transition {
		t.Errorf("a third identity: shared=%v transition=%v, want shared with no transition", shared, transition)
	}
	if shared, transition := l.record("B"); !shared || transition {
		t.Errorf("a repeated identity: shared=%v transition=%v, want shared with no transition", shared, transition)
	}
}

// TestSharedMarkSurvivesAHealthClearingRepin is the regression this file was
// missing. conn_repin clears Health on ordinary successes — the same-root
// promotion (conn_repin.go:279) and the re-pin that moves the root
// (conn_repin.go:348) — so a mark written ONLY on the transition into shared is
// gone for good from the first re-pin onwards, while the connection is still
// shared and still refusing anonymous state-changing calls with no diagnostic
// left to explain why. Found by independent review of PLAN-396: the latch that
// stopped the mark re-announcing also stopped it re-asserting.
func TestSharedMarkSurvivesAHealthClearingRepin(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-plan396-repin")
	t.Cleanup(s.close)

	s.recordLogicalAgent("A")
	s.recordLogicalAgent("B") // the transition; the mark lands here
	if health, _ := sessionHealth(t, s.sessID); health != "shared_connection_detected" {
		t.Fatalf("precondition: health = %q, want the shared-connection mark", health)
	}

	// Exactly what a successful re-pin does to the session record.
	session.Patch(s.sessionID(), func(info *session.Info) {
		info.Health = ""
		info.HealthMessage = ""
	})

	// The connection has not stopped being shared, and its peers keep
	// declaring themselves.
	s.recordLogicalAgent("C")
	if !s.logicalAgents.refuse("") {
		t.Fatal("sanity: the connection must still be shared, so anonymous state-changing calls are still refused")
	}
	health, msg := sessionHealth(t, s.sessID)
	if health != "shared_connection_detected" {
		t.Errorf("health = %q after a re-pin cleared it, want the mark re-asserted — the connection is still shared and still refusing anonymous writes", health)
	}
	if msg == "" {
		t.Error("the health message carrying the one-serve-per-agent remedy was not restored with the mark")
	}

	// The re-assert must not have cost the non-clobber guard: a more specific
	// note written afterwards still wins, and later declarations leave it alone.
	session.Patch(s.sessionID(), func(info *session.Info) {
		info.Health = "contested_pin"
		info.HealthMessage = "the pin was forced between two projects"
	})
	s.recordLogicalAgent("D")
	if health, _ := sessionHealth(t, s.sessID); health != "contested_pin" {
		t.Errorf("health = %q, want contested_pin — re-asserting on every declaration must not clobber a more specific note", health)
	}
}

// TestSharedMarkDoesNotClobberASpecificNote pins the damaging half of
// PLAN-396: shared_connection_detected is a STATE, not an event, and it must
// never overwrite a more specific, more actionable note (contested_pin,
// blocked) that another code path has already written. Rewriting its own state
// stays idempotent, so a latched mark is never mistaken for a new one.
func TestSharedMarkDoesNotClobberASpecificNote(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-plan396")
	t.Cleanup(s.close)

	// A specific note lands BEFORE the connection becomes shared — the
	// ordering a pin fight produces ahead of the peers declaring themselves.
	session.Patch(s.sessionID(), func(info *session.Info) {
		info.Health = "contested_pin"
		info.HealthMessage = "the pin was forced between two projects"
	})

	s.recordLogicalAgent("A")
	s.recordLogicalAgent("B") // the transition; the mark fires here

	if health, msg := sessionHealth(t, s.sessID); health != "contested_pin" {
		t.Errorf("the shared-connection transition overwrote a more specific note: health=%q (%s)", health, msg)
	}
}

// TestSharedMarkIsAnnouncedOnce pins the announce rate through the real logger:
// exactly one Warn for the whole connection lifetime, however many identities
// declare themselves afterwards.
func TestSharedMarkIsAnnouncedOnce(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store, ss := newOriginStore(t)
	s := newPersistSession(t, store, ss, "proxy-plan396")
	t.Cleanup(s.close)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&logs, nil))

	s.recordLogicalAgent("A")
	s.recordLogicalAgent("B") // the transition — one Warn
	for _, id := range []string{"C", "D", "B", "A"} {
		s.recordLogicalAgent(id) // every one of these re-fired before PLAN-396
	}
	if n := strings.Count(logs.String(), "shared connection detected"); n != 1 {
		t.Errorf("the shared-connection mark announced %d times, want exactly once:\n%s", n, logs.String())
	}
	if health, _ := sessionHealth(t, s.sessID); health != "shared_connection_detected" {
		t.Errorf("health = %q, want the latched shared-connection state", health)
	}
}

// TestDeclaredAgentCtx pins the precedence between session_start's own identity
// channel and the per-call _meta one. session_id is the only channel a client
// that cannot inject _meta has, so it must reach the ctx; but _meta is asserted
// per call rather than per attach, so where both are present _meta wins and a
// stale or reused session_id can never re-label a call.
func TestDeclaredAgentCtx(t *testing.T) {
	s := &connSession{}
	if got := mcp.LogicalAgentFromCtx(s.declaredAgentCtx(context.Background(), "sub-1")); got != "sub-1" {
		t.Errorf("declared session_id = %q on the ctx, want %q", got, "sub-1")
	}
	// An empty declaration adds nothing.
	if got := mcp.LogicalAgentFromCtx(s.declaredAgentCtx(context.Background(), "")); got != "" {
		t.Errorf("empty session_id put %q on the ctx, want none", got)
	}
	// A per-call _meta identity outranks the declaration.
	withMeta := mcp.WithLogicalAgent(context.Background(), "meta-agent")
	if got := mcp.LogicalAgentFromCtx(s.declaredAgentCtx(withMeta, "sub-1")); got != "meta-agent" {
		t.Errorf("logical agent = %q, want the per-call _meta identity %q", got, "meta-agent")
	}
}

// TestDeclaredAgentChannelIsWired guards the wiring, not the behaviour: the
// declared-agent channel only closes issue #182 if session_start is actually
// handed it, and nothing else in the suite exercises registerAllTools' own
// chain (the harness wires its own tool). Dropping the line would leave every
// behavioural test green while every real subagent went back to moving the
// connection's pin. Same source-scanning idiom as
// TestBoundaryGuardWiringComplete above it.
func TestDeclaredAgentChannelIsWired(t *testing.T) {
	src, err := os.ReadFile("conn_register.go")
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	body := registerAllToolsBody(string(src))
	if body == "" {
		t.Fatal("could not locate registerAllTools in conn_register.go — was it renamed?")
	}
	if !strings.Contains(body, "WithDeclaredAgent(s.declaredAgentCtx)") {
		t.Error("session_start is registered without WithDeclaredAgent(s.declaredAgentCtx): " +
			"a multiplexed subagent's session_start would re-pin the whole connection again (issue #182)")
	}
}
