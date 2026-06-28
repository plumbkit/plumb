package cli

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/theme"
	"github.com/plumbkit/plumb/internal/tui"
)

// sameColour reports whether two colours resolve to the same RGBA, so a
// lowercase palette hex ("#5cb88a") and the TUI's uppercase literal ("#5CB88A")
// compare equal.
func sameColour(t *testing.T, got, want color.Color) {
	t.Helper()
	gr, gg, gb, ga := got.RGBA()
	wr, wg, wb, wa := want.RGBA()
	if gr != wr || gg != wg || gb != wb || ga != wa {
		t.Errorf("colour mismatch: got %v, want %v", got, want)
	}
}

// TestConfigShowStyles_MatchTUIPlumb locks the config-show styles to the TUI's
// default Plumb theme. `config show` no longer imports internal/tui, so this
// guard catches a future palette drift that would change its output.
func TestConfigShowStyles_MatchTUIPlumb(t *testing.T) {
	p := configShowPalette()

	sameColour(t, lipgloss.Color(p.Grn), tui.Plumb.Success)
	sameColour(t, lipgloss.Color(p.Warn), tui.Plumb.Warning)
	sameColour(t, lipgloss.Color(p.Soft), tui.Plumb.TextMuted)
	sameColour(t, lipgloss.Color(p.Text), tui.Plumb.TextPrimary)
	sameColour(t, lipgloss.Color(p.Rule), tui.Plumb.Border)
	sameColour(t, lipgloss.Color(p.Grn), tui.Plumb.Key)

	if w := configShowKeyStyle().GetWidth(); w != 12 {
		t.Errorf("key style width = %d, want 12 (must match tui.KeyStyle)", w)
	}
}

// TestConfigPrintChroma_ParityWithTUI confirms the theme.Get chroma mapping now
// driving `config print` matches the TUI catalogue it replaced, for every theme.
func TestConfigPrintChroma_ParityWithTUI(t *testing.T) {
	for name, tt := range tui.AvailableThemes {
		p, ok := theme.Get(name)
		if !ok {
			t.Errorf("theme.Get(%q): not found in palette catalogue", name)
			continue
		}
		if p.ChromaStyle != tt.ChromaStyle {
			t.Errorf("chroma style for %q: theme=%q, tui=%q", name, p.ChromaStyle, tt.ChromaStyle)
		}
	}
}
