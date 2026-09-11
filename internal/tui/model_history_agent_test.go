package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
)

// TestRightLinesHistory_DistinguishesAgentsOnOneSession is the TUI half of
// PLAN-401. Two agents multiplexed over one connection share a session name,
// so if the Session column renders only that name — or truncates before the
// part that differs — the History tab relocates the defect instead of fixing
// it. The column is sized so the distinguishing half survives.
func TestRightLinesHistory_DistinguishesAgentsOnOneSession(t *testing.T) {
	now := time.Now()
	m := &Model{recentCalls: []stats.RecentCall{
		{Tool: "write_file", SessionID: "s1", SessionName: "brave-lake", CalledAt: now, Success: true, LogicalAgent: "conv-9/a1b2c3d4e5"},
		{Tool: "write_file", SessionID: "s1", SessionName: "brave-lake", CalledAt: now, Success: true, LogicalAgent: "conv-9/ffffffff00"},
		{Tool: "read_file", SessionID: "s1", SessionName: "brave-lake", CalledAt: now, Success: true},
	}}

	out := strings.Join(m.rightLinesHistory(100), "\n")
	for _, want := range []string{"brave-lake/a1b2c3d", "brave-lake/fffffff"} {
		if !strings.Contains(out, want) {
			t.Errorf("history did not render %q — the agents are indistinguishable:\n%s", want, out)
		}
	}
	// The unattributed row keeps the bare name: exactly the two stamped rows
	// are qualified.
	if n := strings.Count(out, "brave-lake/"); n != 2 {
		t.Errorf("%d qualified rows, want 2 — a row with no agent id must render the bare session name:\n%s", n, out)
	}
}

// TestPopupRendersTheAgent: the History column is narrow, so the detail pane
// is where a reader goes to find out WHICH agent — it must not answer with the
// bare session name. (The row it renders comes from CallsForTool, which had to
// learn to select the column for this to be possible at all.)
func TestPopupRendersTheAgent(t *testing.T) {
	now := time.Now()
	m := Model{
		popupCalls: []stats.RecentCall{{Tool: "write_file", SessionID: "s1", SessionName: "brave-lake", CalledAt: now, Success: true, LogicalAgent: "conv-9/a1b2c3d4e5"}},
	}
	out := strings.Join(m.popupRightAll(80), "\n")
	if !strings.Contains(out, "brave-lake/a1b2c3d") {
		t.Errorf("the detail pane does not name the agent:\n%s", out)
	}
}
