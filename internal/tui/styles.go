// Package tui implements the Bubble Tea v2 sessions dashboard for plumb.
package tui

import "charm.land/lipgloss/v2"

// Component styles. All colours come exclusively from ActiveTheme — no hex
// literals or terminal-palette indices appear here. Call RebuildStyles()
// after changing ActiveTheme and before rendering.
var (
	TitleStyle           lipgloss.Style
	PanelHeaderStyle     lipgloss.Style
	PanelHeaderDimStyle  lipgloss.Style
	SepStyle             lipgloss.Style
	SepDimStyle          lipgloss.Style
	KeyStyle             lipgloss.Style
	ValStyle             lipgloss.Style
	ItemStyle            lipgloss.Style
	SelectedStyle        lipgloss.Style
	DimStyle             lipgloss.Style
	MutedStyle           lipgloss.Style
	TimestampActiveStyle lipgloss.Style
	TimestampDimStyle    lipgloss.Style // cursor row when right panel focused (readable)
	TimestampUnfocusedStyle lipgloss.Style // non-cursor rows when right panel focused
	DetailStyle          lipgloss.Style
	HintStyle            lipgloss.Style
	StatusStyle          lipgloss.Style
	OkStyle              lipgloss.Style
	WarnStyle            lipgloss.Style
	ScrollThumbStyle     lipgloss.Style
	ScrollTrackStyle     lipgloss.Style
)

func init() { RebuildStyles() }

// RebuildStyles rebuilds all package-level styles from ActiveTheme.
// Must be called after changing ActiveTheme and before rendering.
func RebuildStyles() {
	t := ActiveTheme

	TitleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent)

	PanelHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.PanelTitle)

	// Dimmed panel title — used for the unfocused side of the popup border.
	PanelHeaderDimStyle = lipgloss.NewStyle().
		Foreground(t.TextDim)

	SepStyle = lipgloss.NewStyle().
		Foreground(t.Border)

	// SepDimStyle: border characters for the background panel when the popup
	// is overlaid on top. Uses TextDim so they recede without disappearing.
	SepDimStyle = lipgloss.NewStyle().
		Foreground(t.TextDim)

	// KeyStyle has a fixed width so detail-row values align consistently.
	KeyStyle = lipgloss.NewStyle().
		Width(12).
		Foreground(t.Key)

	ValStyle = lipgloss.NewStyle().
		Foreground(t.TextPrimary)

	ItemStyle = lipgloss.NewStyle().
		Foreground(t.ItemText)

	// SelectedStyle: bold accent foreground only.
	// No background is ever set — the terminal's own background is respected.
	SelectedStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent)

	// DimStyle: applied to every character in the background panel when the
	// popup overlay is active, making the underlying content recede.
	DimStyle = lipgloss.NewStyle().
		Foreground(t.TextDim)

	// MutedStyle: low-priority secondary text (timestamps, durations, etc.).
	MutedStyle = lipgloss.NewStyle().
		Foreground(t.TextMuted)

	// TimestampActiveStyle: timestamps in the focused popup-left panel.
	TimestampActiveStyle = lipgloss.NewStyle().
		Foreground(t.TextPrimary)

	// TimestampDimStyle: cursor row in the left panel when right panel holds focus.
	// Readable — this is still the foreground panel, just not the focused one.
	TimestampDimStyle = lipgloss.NewStyle().
		Foreground(t.TextUnfocused)

	// TimestampUnfocusedStyle: non-cursor rows when right panel holds focus.
	TimestampUnfocusedStyle = lipgloss.NewStyle().
		Foreground(t.TextUnfocused)

	// DetailStyle: values in the call-detail right panel.
	DetailStyle = lipgloss.NewStyle().
		Foreground(t.TextSecondary)

	// HintStyle: column headers and the global key-hint bar.
	HintStyle = lipgloss.NewStyle().
		Foreground(t.TextHint)

	StatusStyle = lipgloss.NewStyle().
		Foreground(t.TextHint)

	OkStyle = lipgloss.NewStyle().
		Foreground(t.Success)

	WarnStyle = lipgloss.NewStyle().
		Foreground(t.Warning)

	ScrollThumbStyle = lipgloss.NewStyle().
		Foreground(t.ScrollThumb)

	ScrollTrackStyle = lipgloss.NewStyle().
		Foreground(t.ScrollTrack)
}
