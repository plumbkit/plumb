package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
)

func TestLinkageIDOf(t *testing.T) {
	cases := map[string]string{
		"conv":              "conv",
		"conv/agent-1":      "conv",
		"conv/agent/nested": "conv",
		"":                  "",
		"/agent":            "",
	}
	for in, want := range cases {
		if got := linkageIDOf(in); got != want {
			t.Errorf("linkageIDOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLogicalAgentLabel(t *testing.T) {
	cases := map[string]string{
		"21190478-3026-4589-b8f5-cd23c3f8517f":              "21190478",
		"21190478-3026-4589-b8f5-cd23c3f8517f/a1b2c3d4e5f6": "21190478/agent-a1b2c3d4",
		"short/x": "short/agent-x",
		"":        "",
	}
	for in, want := range cases {
		if got := logicalAgentLabel(in); got != want {
			t.Errorf("logicalAgentLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFirstShardInheritsConnectionReads: the agent that WAS the connection
// before a peer turned it shared keeps the reads it made as the connection; the
// peer starts empty. Without the seeding, every edit the first agent had in
// flight fails "has not been read" the moment a subagent declares itself.
func TestFirstShardInheritsConnectionReads(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	root := freshTempDir(t)
	mustGitDir(t, root)
	path := root + "/main.go"
	when := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	// Single-agent phase: the first agent attaches and reads as the connection.
	s.recordLogicalAgentAttach("conv")
	s.readTracker.Record(path, when, "sha-1")

	// A subagent declares itself: the connection is now shared.
	s.recordLogicalAgentCall("conv/agent-1")

	first := s.readTrackerFor(mcp.WithLogicalAgent(context.Background(), "conv"))
	if first == s.readTracker {
		t.Fatal("the first agent must be routed to its own shard once the connection is shared")
	}
	if got := first.Mtime(path); !got.Equal(when) {
		t.Fatalf("first agent's shard lost the read it made as the connection: mtime %v, want %v", got, when)
	}
	second := s.readTrackerFor(mcp.WithLogicalAgent(context.Background(), "conv/agent-1"))
	if got := second.Mtime(path); !got.IsZero() {
		t.Fatalf("a later agent must start with no reads, got mtime %v", got)
	}
}

// TestFirstIDIsTheFirstCommittedIdentity pins that a refused-then-retried or
// re-declared identity cannot displace the first.
func TestFirstIDIsTheFirstCommittedIdentity(t *testing.T) {
	var l logicalAgentState
	if got := l.firstID(); got != "" {
		t.Fatalf("empty state must have no first id, got %q", got)
	}
	l.record("")
	l.record("a")
	l.record("b")
	l.record("a")
	if got := l.firstID(); got != "a" {
		t.Fatalf("firstID = %q, want a", got)
	}
}

// TestSubagentLinkageKeepsTheConversation: a hook-stamped subagent id
// `<conversation>/<agent>` is committed as its own logical agent, but the
// session RECORD stays linked to the conversation, so the parent's mail
// address and wake stamp keep resolving after the subagent declares itself.
func TestSubagentLinkageKeepsTheConversation(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)
	if s.sessionID() == "" {
		t.Fatal("session did not register; the linkage cannot be observed")
	}

	s.linkExternalID("conv-1")
	if got := s.externalID(); got != "conv-1" {
		t.Fatalf("externalID after the parent linked = %q, want conv-1", got)
	}
	s.linkExternalID("conv-1/agent-7")
	if got := s.externalID(); got != "conv-1" {
		t.Fatalf("a subagent's attach rewrote the conversation linkage to %q", got)
	}
	if !s.logicalAgents.sharedWith("") {
		t.Fatal("the subagent's full id must be committed as a second logical agent")
	}
	if got := s.logicalAgents.firstID(); got != "conv-1" {
		t.Fatalf("firstID = %q, want conv-1", got)
	}
	// The subagent attaching FIRST links the same conversation.
	fresh := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(fresh.close)
	fresh.linkExternalID("conv-2/agent-1")
	if got := fresh.externalID(); got != "conv-2" {
		t.Fatalf("externalID after a subagent-first attach = %q, want conv-2", got)
	}
}

// TestExternalIDLinkerIsWired guards the wiring the way
// TestDeclaredAgentChannelIsWired does: the rooting only happens if
// session_start is handed linkExternalID rather than an inline closure.
func TestExternalIDLinkerIsWired(t *testing.T) {
	src, err := os.ReadFile("conn_register.go")
	if err != nil {
		t.Fatalf("reading conn_register.go: %v", err)
	}
	body := registerAllToolsBody(string(src))
	if !strings.Contains(body, "WithExternalID(s.linkExternalID)") {
		t.Error("session_start is registered without WithExternalID(s.linkExternalID): a subagent's session_id would rewrite the conversation's linkage")
	}
}
