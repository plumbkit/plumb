package cli

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/session"
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

// TestLinkageOwnerInheritsConnectionReads: the agent that WAS the connection
// before a peer turned it shared keeps the reads it made as the connection;
// the peer starts empty. Without the seeding, every edit the parent had in
// flight fails "has not been read" the moment a subagent declares itself.
func TestLinkageOwnerInheritsConnectionReads(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	root := freshTempDir(t)
	mustGitDir(t, root)
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	path := root + "/main.go"
	when := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	// Single-agent phase: the parent links the conversation and reads as the
	// connection.
	s.linkExternalID("conv")
	s.readTracker.Record(path, when, "sha-1")

	// A subagent declares itself: the connection is now shared.
	s.recordLogicalAgentCall("conv/agent-1")

	parent := s.readTrackerFor(mcp.WithLogicalAgent(context.Background(), "conv"))
	if parent == s.readTracker {
		t.Fatal("the parent must be routed to its own shard once the connection is shared")
	}
	if got := parent.Mtime(path); !got.Equal(when) {
		t.Fatalf("the parent's shard lost the read it made as the connection: mtime %v, want %v", got, when)
	}
	sub := s.readTrackerFor(mcp.WithLogicalAgent(context.Background(), "conv/agent-1"))
	if got := sub.Mtime(path); !got.IsZero() {
		t.Fatalf("a subagent must start with no reads, got mtime %v", got)
	}
}

// TestConnectionReadsSeedOnlyTheLinkageOwner is the daemon-restart shape the
// first review of this change caught: the connection's reads are rehydrated,
// the linkage is restored from the durable record, no agent has re-declared
// itself yet, and the FIRST stamped call is the subagent's. It must not
// inherit the parent's reads — that would let it edit, in strict mode, files
// it never read. The parent, arriving later, still does.
func TestConnectionReadsSeedOnlyTheLinkageOwner(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())
	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	root := freshTempDir(t)
	mustGitDir(t, root)
	if _, err := s.repinWorkspace(context.Background(), "file://"+root, "", false); err != nil {
		t.Fatalf("pin: %v", err)
	}
	path := root + "/main.go"
	when := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	// "After the restart": linkage restored, reads rehydrated, nothing declared.
	session.SetExternalID(s.sessionID(), "conv")
	s.readTracker.Record(path, when, "sha-1")

	// The subagent's stamped call is the first identity the new session sees;
	// then the parent's.
	s.recordLogicalAgentCall("conv/agent-1")
	s.recordLogicalAgentCall("conv")

	sub := s.readTrackerFor(mcp.WithLogicalAgent(context.Background(), "conv/agent-1"))
	if got := sub.Mtime(path); !got.IsZero() {
		t.Fatalf("the subagent inherited the parent's read (mtime %v) because it was seen first", got)
	}
	parent := s.readTrackerFor(mcp.WithLogicalAgent(context.Background(), "conv"))
	if got := parent.Mtime(path); !got.Equal(when) {
		t.Fatalf("the linkage owner's shard must inherit the connection's reads: mtime %v, want %v", got, when)
	}

	// A shard pinned to a different root than the connection is not seeded
	// either: a read is only valid for the root it was made under.
	if s.seedsConnectionReads("conv", root+"/elsewhere", root) {
		t.Fatal("a shard on another root must not inherit the connection's reads")
	}
	if s.seedsConnectionReads("", root, root) || s.seedsConnectionReads("conv/agent-1", root, root) || s.seedsConnectionReads("other", root, root) {
		t.Fatal("only the linkage owner qualifies")
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

// TestSubagentAttachDoesNotResumeTwice: name inheritance from an ended
// predecessor runs once per linkage. The parent resumes the name; a subagent
// attaching under the same conversation must not run the inheritance again
// (a second rename against a name the session already holds, or a second
// resumedNewIdentity flip).
func TestSubagentAttachDoesNotResumeTwice(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	store := config.NewStore(config.Defaults())

	// An ended predecessor linked to the conversation, inside the grace window.
	prev, err := session.Register(session.Info{Name: "old-owl", Folder: "/w", Language: "go"})
	if err != nil {
		t.Fatalf("register predecessor: %v", err)
	}
	session.SetExternalID(prev.ID, "conv-r")
	session.Unregister(prev.ID)

	s := newConnSession(context.Background(), detectTestPool(), nil, store, nil, nil, newSharedBudgets())
	t.Cleanup(s.close)

	if got := s.linkExternalID("conv-r"); got != "old-owl" {
		t.Fatalf("the parent's attach must inherit the predecessor's name, got %q", got)
	}
	if !s.view().resumedNewIdentity {
		t.Fatal("the inheritance must be disclosed as a resumed identity")
	}
	if got := s.linkExternalID("conv-r/agent-1"); got != "" {
		t.Fatalf("a subagent attaching under an already-linked conversation must not resume again, got %q", got)
	}
	if got := s.externalID(); got != "conv-r" {
		t.Fatalf("linkage = %q, want conv-r", got)
	}
}
