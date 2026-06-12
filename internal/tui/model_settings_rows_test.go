package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
)

// TestClampSettingsValueW covers the layout-corruption fix: a long value (e.g. a
// long path in extra_roots) must not widen the value column past the rows pane,
// which would wrap the row and break its borders. The column is capped to the
// pane width (accounting for label + control), floored at 8.
func TestClampSettingsValueW(t *testing.T) {
	items := []settingItem{{kind: settingList}} // control renders as "‹ edit ›"
	ctrlW := lipgloss.Width(settingControl(items[0]))
	const labelW = 10

	t.Run("value that fits is unchanged", func(t *testing.T) {
		if got := clampSettingsValueW(20, labelW, items, 120); got != 20 {
			t.Errorf("clampSettingsValueW = %d, want 20 (value fits the pane)", got)
		}
	})

	t.Run("over-wide value is clamped to the pane", func(t *testing.T) {
		const rowsW = 60
		want := rowsW - labelW - ctrlW - 3 // leading space + 2-cell cursor column
		if got := clampSettingsValueW(500, labelW, items, rowsW); got != want {
			t.Errorf("clampSettingsValueW = %d, want %d (clamped to pane width)", got, want)
		}
	})

	t.Run("clamp floors at 8 on a tiny pane", func(t *testing.T) {
		// rowsW - labelW - ctrlW - 3 goes negative; the floor keeps a usable cell.
		if got := clampSettingsValueW(500, labelW, items, 15); got != 8 {
			t.Errorf("clampSettingsValueW = %d, want 8 (floor on a tiny pane)", got)
		}
	})
}
