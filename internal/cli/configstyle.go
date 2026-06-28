package cli

import (
	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/theme"
)

// configstyle.go keeps `plumb config show` off the Bubble Tea layer.
//
// The command used to borrow its status-glyph and table styles from
// internal/tui, dragging the whole TUI package onto the CLI config path purely
// for a handful of foreground colours. Those colours live in the render-neutral
// internal/theme palette catalogue (no lipgloss, no bubbletea), so the styles
// are rebuilt here from the default palette instead.
//
// `config show` always renders with plumb's default palette: it never assigns
// internal/tui.ActiveTheme (only the bare TUI launch does), so the styles are
// fixed to theme.Default — byte-for-byte the same output as before.

// configShowPalette returns the palette `config show` renders with — always
// plumb's default, matching the TUI's default ActiveTheme.
func configShowPalette() theme.Palette {
	p, _ := theme.Get(theme.Default)
	return p
}

// configShowFG builds a foreground-only style from a palette hex string.
func configShowFG(hex string) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(lipgloss.Color(hex))
}

func configShowOkStyle() lipgloss.Style    { return configShowFG(configShowPalette().Grn) }
func configShowWarnStyle() lipgloss.Style  { return configShowFG(configShowPalette().Warn) }
func configShowMutedStyle() lipgloss.Style { return configShowFG(configShowPalette().Soft) }
func configShowValStyle() lipgloss.Style   { return configShowFG(configShowPalette().Text) }
func configShowSepStyle() lipgloss.Style   { return configShowFG(configShowPalette().Rule) }

// configShowKeyStyle fixes the key column to a 12-cell width so values align,
// matching the TUI's KeyStyle. The Plumb theme keys off its sage-green (Grn).
func configShowKeyStyle() lipgloss.Style {
	return lipgloss.NewStyle().Width(12).Foreground(lipgloss.Color(configShowPalette().Grn))
}
