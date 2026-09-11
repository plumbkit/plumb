package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/stats"
)

// TestRecentCallWidths_AccountForTheAgentLabel: `plumb stats`' Name column is
// sized from the widest value it will print. Sizing it from the session name
// while printing the agent-qualified label truncates the column's whole point,
// so the width and the row must read the same value.
func TestRecentCallWidths_AccountForTheAgentLabel(t *testing.T) {
	now := time.Now()
	rows := []stats.RecentCall{
		{Tool: "write_file", SessionID: "s1", SessionName: "me-fox", CalledAt: now, LogicalAgent: "conv-1/a1b2c3d4e5"},
		{Tool: "read_file", SessionID: "s1", SessionName: "me-fox", CalledAt: now},
	}
	_, _, wName := calcRecentWidths(rows)

	label := stats.AgentLabel("me-fox", "conv-1/a1b2c3d4e5")
	if !strings.HasPrefix(label, "me-fox/") {
		t.Fatalf("precondition: expected a qualified label, got %q", label)
	}
	if wName < len(label) {
		t.Errorf("Name column is %d wide but the label it prints is %d bytes (%q) — "+
			"the agent half would be cut off", wName, len(label), label)
	}
}
