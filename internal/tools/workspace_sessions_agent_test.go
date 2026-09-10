package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// TestFormatWorkspaceSessions_AttributesWritesToAgents is PLAN-401's
// acceptance at the render seam: four logical agents writing through ONE
// connection must show as four distinct writers, a write carrying no agent id
// still shows the bare session name, and the session's own main thread is
// not qualified by its own conversation id.
func TestFormatWorkspaceSessions_AttributesWritesToAgents(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "me-fox", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now},
		{ID: "peer-2", Name: "brave-lake", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now},
	}
	writes := []stats.RecentCall{
		{Tool: "write_file", SessionID: "peer-2", SessionName: "brave-lake", CalledAt: now, Success: true, InputJSON: `{"file_path":"/ws/a.go"}`, LogicalAgent: "conv-9/a1b2c3d4e5"},
		{Tool: "write_file", SessionID: "peer-2", SessionName: "brave-lake", CalledAt: now, Success: true, InputJSON: `{"file_path":"/ws/b.go"}`, LogicalAgent: "conv-9/ffffffff00"},
		{Tool: "edit_file", SessionID: "peer-2", SessionName: "brave-lake", CalledAt: now, Success: true, InputJSON: `{"file_path":"/ws/c.go"}`, LogicalAgent: "agent-alpha"},
		{Tool: "git", SessionID: "peer-2", SessionName: "brave-lake", CalledAt: now, Success: true, InputJSON: `{"subcommand":"commit"}`, OutputText: "a1b2c3d x", LogicalAgent: "conv-9/a1b2c3d4e5"},
		{Tool: "edit_file", SessionID: "peer-2", SessionName: "brave-lake", CalledAt: now, Success: true, InputJSON: `{"file_path":"/ws/d.go"}`},
	}
	out := formatWorkspaceSessions("/ws", "self-1", peers, writes, nil, now)

	for _, want := range []struct{ who, rest string }{
		{"brave-lake/agent-a1b2c3d4", "write_file"},
		{"brave-lake/agent-ffffffff", "write_file"},
		{"brave-lake/agent-al", "c.go"},
		{"brave-lake/agent-a1b2c3d4", "git commit"},
		// The blank-id row keeps the bare name: not qualified by anything.
		{"brave-lake", "d.go"},
	} {
		if !feedLineWith(out, want.who, want.rest) {
			t.Errorf("expected a feed line by %q containing %q:\n%s", want.who, want.rest, out)
		}
	}
}

// feedLineWith reports whether a feed line is attributed to exactly `who`
// (the who column is padded, so the name must be followed by a space) and
// carries rest.
func feedLineWith(out, who, rest string) bool {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, who+" ") && strings.Contains(line, rest) {
			return true
		}
	}
	return false
}

// TestWriterLabels_MemoisesTheExternalIDLookup pins that the main-thread
// exception is evaluated against the session's external id, and that the id
// is read once per session however many rows it labels.
func TestWriterLabels_MemoisesTheExternalIDLookup(t *testing.T) {
	l := newWriterLabels()
	l.extIDs["sess-1"] = "conv-1" // seeded: the lookup would otherwise hit the session store
	rows := []stats.RecentCall{
		{SessionID: "sess-1", SessionName: "me-fox", LogicalAgent: "conv-1"},
		{SessionID: "sess-1", SessionName: "me-fox", LogicalAgent: "conv-1/zz"},
		{SessionID: "sess-1", SessionName: "me-fox"},
	}
	want := []string{"me-fox", "me-fox/agent-zz", "me-fox"}
	for i, r := range rows {
		if got := l.label(r); got != want[i] {
			t.Errorf("row %d label = %q, want %q", i, got, want[i])
		}
	}
	if len(l.extIDs) != 1 {
		t.Errorf("memo grew to %d entries for one session", len(l.extIDs))
	}
}
