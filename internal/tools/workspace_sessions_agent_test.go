package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// TestFormatWorkspaceSessions_AttributesWritesToAgents is PLAN-401's
// acceptance at the render seam: several logical agents writing through ONE
// connection must show as distinct writers. It covers every shape the feed
// renders — a write with a path, a write without one, and both git shapes —
// because a label wired into one shape and not the others is the half-fix the
// card warns about.
func TestFormatWorkspaceSessions_AttributesWritesToAgents(t *testing.T) {
	now := time.Now()
	peers := []session.Info{
		{ID: "self-1", Name: "me-fox", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now},
		{ID: "peer-2", Name: "brave-lake", Folder: "/ws", ClientName: "claude-code", LastSeenAt: now},
	}
	row := func(tool, input, agent string) stats.RecentCall {
		return stats.RecentCall{
			Tool: tool, SessionID: "peer-2", SessionName: "brave-lake", CalledAt: now,
			Success: true, InputJSON: input, LogicalAgent: agent,
		}
	}
	commit := row("git", `{"subcommand":"commit"}`, "conv-9/a1b2c3d4e5")
	commit.OutputText = "a1b2c3d x"
	writes := []stats.RecentCall{
		row("write_file", `{"file_path":"/ws/a.go"}`, "conv-9/a1b2c3d4e5"),
		row("write_file", `{"file_path":"/ws/b.go"}`, "conv-9/ffffffff00"),
		row("edit_file", `{"file_path":"/ws/c.go"}`, "agent-alpha"),
		// no path at all: the tool-only shape
		row("transaction_apply", `{}`, "conv-9/a1b2c3d4e5"),
		// git, both shapes
		row("git", `{"subcommand":"add"}`, "conv-9/ffffffff00"),
		commit,
		row("edit_file", `{"file_path":"/ws/d.go"}`, ""),
	}

	out := formatWorkspaceSessions("/ws", "self-1", peers, writes, nil, now)

	for _, want := range []struct{ who, rest string }{
		{"brave-lake/a1b2c3d…", "a.go"},              // path shape
		{"brave-lake/fffffff…", "b.go"},              // a second agent, same session
		{"brave-lake/agent-a…", "c.go"},              // a plain _meta-style id
		{"brave-lake/a1b2c3d…", "transaction_apply"}, // path-less shape
		{"brave-lake/fffffff…", "git add"},           // git, non-commit shape
		{"brave-lake/a1b2c3d…", "git commit"},        // git, commit shape
		// A write with no agent id keeps the bare session name: it was made
		// when the connection had one agent, so the name names it exactly.
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

// TestFormatWorkspaceSessions_AgentIDCannotForgeAFeedRow: the id is a
// client-supplied string rendered into the surface peers read for attribution.
// A newline in it would print a second, invented writer row.
func TestFormatWorkspaceSessions_AgentIDCannotForgeAFeedRow(t *testing.T) {
	now := time.Now()
	peers := []session.Info{{ID: "self-1", Name: "me-fox", Folder: "/ws", LastSeenAt: now}}
	writes := []stats.RecentCall{{
		Tool: "write_file", SessionID: "self-1", SessionName: "me-fox", CalledAt: now, Success: true,
		InputJSON: `{"file_path":"/ws/a.go"}`, LogicalAgent: "x\nroot-agent   write_file   passwd",
	}}
	out := formatWorkspaceSessions("/ws", "other", peers, writes, nil, now)
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "passwd") && !strings.Contains(line, "a.go") {
			t.Fatalf("an agent id forged its own feed row:\n%s", out)
		}
	}
}
