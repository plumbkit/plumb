package cli

import (
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/tui"
)

// TestStatusStyle locks the status-to-style mapping. The styles are compared by
// foreground colour via sameColour (configstyle_test.go); an unmapped status
// must keep the zero style, whose GetForeground is NoColor.
func TestStatusStyle(t *testing.T) {
	tui.RebuildStyles()

	plain := lipgloss.NewStyle()
	cases := []struct {
		name   string
		status string
		want   lipgloss.Style
	}{
		{"ok registered", "registered", tui.OkStyle},
		{"ok updated", "updated", tui.OkStyle},
		{"ok installed", "installed", tui.OkStyle},
		{"warn missing", "missing", tui.WarnStyle},
		{"warn stale", "stale", tui.WarnStyle},
		{"warn stale with suffix", "stale (installed by 0.15.1)", tui.WarnStyle},
		{"warn error", "error", tui.WarnStyle},
		{"warn uninstall", "uninstall", tui.WarnStyle},
		{"warn unregistered", "unregistered", tui.WarnStyle},
		{"muted not installed", "not installed", tui.MutedStyle},
		{"muted not registered", "not registered", tui.MutedStyle},
		{"muted already current", "already current", tui.MutedStyle},
		{"muted skipped", "skipped", tui.MutedStyle},
		{"plain empty", "", plain},
		{"plain unknown", "pending", plain},
		// Matching is case-sensitive, so a capitalised status renders plain.
		{"plain capitalised", "Not Installed", plain},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sameColour(t, statusStyle(tc.status).GetForeground(), tc.want.GetForeground())
		})
	}
}
