package cli

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/mcp"
)

func TestLogicalAgentStateRefuse(t *testing.T) {
	var l logicalAgentState
	if l.refuse("") {
		t.Fatal("a single-agent connection must never refuse")
	}
	l.record("A", true) // attach-time session_id
	if l.refuse("") {
		t.Fatal("an anonymous call attributable to the attach ID must not refuse")
	}
	if l.refuse("A") {
		t.Fatal("an explicit call ID must not refuse")
	}
	l.record("B", false) // a second agent arrives per-call
	if l.refuse("B") {
		t.Fatal("an explicit ID on a shared connection must not refuse")
	}
	if l.refuse("") {
		t.Fatal("an anonymous call with an attach-time fallback must not refuse")
	}
}

func TestLogicalAgentStateRefuseNoAttach(t *testing.T) {
	var l logicalAgentState
	l.record("A", false)
	l.record("B", false) // shared, no attach-time identity
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
