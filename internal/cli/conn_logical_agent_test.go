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
	if l.refuse("") {
		t.Fatal("an anonymous call attributable to the attach ID must not refuse")
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
// the TRANSITION to shared, not the state. Before it, record returned
// len(seen) > 1 — true on every declaration after the first two — so
// markSharedConnectionDetected re-fired (Warn + Health rewrite) on every peer
// call that carried an identity.
func TestRecordLatchesSharedTransition(t *testing.T) {
	var l logicalAgentState
	if l.record("A") {
		t.Fatal("the first identity must not report shared")
	}
	if !l.record("B") {
		t.Fatal("the second distinct identity is the transition and must report shared")
	}
	if l.record("C") {
		t.Error("a third identity re-reported shared — the mark would re-fire on every declaration")
	}
	if l.record("B") {
		t.Error("a repeated identity re-reported shared")
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
