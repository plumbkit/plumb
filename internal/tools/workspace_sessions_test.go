package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/session"
	"github.com/golimpio/plumb/internal/stats"
)

func TestFileFromInputJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"file_path", `{"file_path":"/ws/foo.go","content":"x"}`, "/ws/foo.go"},
		{"from (rename/copy)", `{"from":"/ws/a.go","to":"/ws/b.go"}`, "/ws/a.go"},
		{"path alias", `{"path":"/ws/p.go"}`, "/ws/p.go"},
		{"transaction first op", `{"operations":[{"file_path":"/ws/t1.go"},{"file_path":"/ws/t2.go"}]}`, "/ws/t1.go"},
		{"git (no path)", `{"subcommand":"commit","message":"x"}`, ""},
		{"empty", ``, ""},
		{"malformed", `{not json`, ""},
		{"empty file_path falls through", `{"file_path":""}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fileFromInputJSON(tt.in); got != tt.want {
				t.Errorf("fileFromInputJSON(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatWorkspaceSessions_Alone(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "lone-otter", Folder: "/ws", LastSeenAt: now},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, nil, now)

	if !strings.Contains(out, "you:  lone-otter") {
		t.Errorf("expected own session name in output:\n%s", out)
	}
	if !strings.Contains(out, "only active session") {
		t.Errorf("a single peer must report the agent is alone:\n%s", out)
	}
	if !strings.Contains(out, "authoritative") {
		t.Errorf("alone case should note the view is authoritative:\n%s", out)
	}
	if !strings.Contains(out, "recent writes: none recorded") {
		t.Errorf("empty writes should be reported:\n%s", out)
	}
}

func TestFormatWorkspaceSessions_MultiplePeersAndWrites(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "me-fox", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now.Add(-10 * time.Second)},
		{ID: "peer-2", Name: "brave-lake", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now.Add(-40 * time.Minute)},
	}
	writes := []stats.RecentCall{
		{Tool: "edit_file", SessionName: "brave-lake", CalledAt: now.Add(-30 * time.Second), InputJSON: `{"file_path":"/ws/internal/pool.go"}`},
		{Tool: "git", SessionName: "brave-lake", CalledAt: now.Add(-2 * time.Minute), InputJSON: `{"subcommand":"commit"}`},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, writes, now)

	if !strings.Contains(out, "active sessions: 2 (including you)") {
		t.Errorf("expected a count of 2 active sessions:\n%s", out)
	}
	if !strings.Contains(out, "me-fox (you)") {
		t.Errorf("the caller's own session must be marked (you):\n%s", out)
	}
	if !strings.Contains(out, "brave-lake") || !strings.Contains(out, "idle") {
		t.Errorf("the idle peer should be listed and marked idle:\n%s", out)
	}
	// Recent write paths are rendered relative to the workspace.
	if !strings.Contains(out, "internal/pool.go") {
		t.Errorf("expected the relative write path:\n%s", out)
	}
	if strings.Contains(out, "/ws/internal/pool.go") {
		t.Errorf("path should be relative to the workspace, not absolute:\n%s", out)
	}
	// A git write with no path still appears (tool name only).
	if !strings.Contains(out, "git") {
		t.Errorf("a path-less write (git) should still be listed:\n%s", out)
	}
}

// TestWorkspaceSessions_ConcurrentNoDeadlock proves that concurrent Execute
// calls do not deadlock. Each call takes the session-dir flock (LOCK_EX) for a
// brief read then releases — concurrent callers queue behind it, which is
// correct serialisation, not deadlock. The wsSessionsTimeout backstop (500ms)
// inside Execute is a second line of defence, but this test verifies the
// access is inherently safe: all goroutines must finish well inside the
// deadline regardless of which code path fires first.
func TestWorkspaceSessions_ConcurrentNoDeadlock(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	const ws = "/tmp/plumb-ws-sessions-concurrency-v3"

	selfID, err := session.Register(session.Info{ID: "self-nd", Name: "self-nd", Folder: ws})
	if err != nil {
		t.Fatalf("register self: %v", err)
	}
	if _, err := session.Register(session.Info{ID: "peer-nd", Name: "peer-nd", Folder: ws}); err != nil {
		t.Fatalf("register peer: %v", err)
	}

	tool := NewWorkspaceSessions(func() string { return ws }, selfID)

	// 6 goroutines × 4 calls = 24 Execute invocations, each taking the flock
	// once. At ~1ms per flock+read (local tmpfs), 24 serial acquisitions fit
	// inside 1 second by a wide margin; we budget 5s for slow CI.
	const goroutines, callsEach = 6, 4
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range callsEach {
				out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
				if err != nil {
					t.Errorf("Execute returned an error: %v", err)
					return
				}
				if out == "" {
					t.Error("Execute returned empty output")
				}
			}
		})
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent workspace_sessions.Execute did not finish in 5s — possible deadlock")
	}
}

func TestFormatWorkspaceSessions_UnknownSelf(t *testing.T) {
	now := time.Now()
	// Self not present in the peer list (e.g. session file not yet flushed).
	peers := []session.Info{
		{ID: "peer-2", Name: "brave-lake", Folder: "/ws", LastSeenAt: now},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, nil, now)
	if !strings.Contains(out, "you:  (unknown)") {
		t.Errorf("missing self should render as (unknown):\n%s", out)
	}
}
