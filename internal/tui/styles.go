// Package tui implements the Bubble Tea v2 sessions dashboard for plumb.
package tui

import "charm.land/lipgloss/v2"

// Component styles. All colours come exclusively from ActiveTheme — no hex
// literals or terminal-palette indices appear here. Call RebuildStyles()
// after changing ActiveTheme and before rendering.
var (
	TitleStyle               lipgloss.Style
	PanelHeaderStyle         lipgloss.Style // active/focused panel title
	PanelHeaderInactiveStyle lipgloss.Style // panel is behind a popup — #3E444F
	PanelHeaderFadedStyle    lipgloss.Style // panel active but lost focus to sibling — #6C768A
	SepStyle                 lipgloss.Style
	SepInactiveStyle         lipgloss.Style // border chars when panel is behind popup
	KeyStyle                 lipgloss.Style
	ValStyle                 lipgloss.Style
	ItemStyle                lipgloss.Style
	SelectedStyle            lipgloss.Style
	SessionLangStyle         lipgloss.Style
	SessionLangSelectedStyle lipgloss.Style
	SessionLangFadedStyle    lipgloss.Style
	InactiveStyle            lipgloss.Style // full panel content behind popup
	MutedStyle               lipgloss.Style
	FadedStyle               lipgloss.Style // content in active-but-unfocused panel
	TimestampActiveStyle     lipgloss.Style
	TimestampFadedStyle      lipgloss.Style // timestamp rows when sibling panel has focus
	DetailStyle              lipgloss.Style
	HintStyle                lipgloss.Style
	TabActiveStyle           lipgloss.Style
	TabInactiveStyle         lipgloss.Style
	TabActiveEdgeStyle       lipgloss.Style
	TabInactiveEdgeStyle     lipgloss.Style
	StatusStyle              lipgloss.Style
	StatusKeyStyle           lipgloss.Style
	LogStatusStyle           lipgloss.Style
	LogSelectedStyle         lipgloss.Style

	// Settings/theme-picker footer bar: subtle background with normal text,
	// brighter keys, and a primary-coloured status message.
	SettingsBarStyle     lipgloss.Style
	SettingsBarKeyStyle  lipgloss.Style
	SettingsBarMsgStyle  lipgloss.Style
	LogDetailKeyStyle    lipgloss.Style
	LogDetailGutterStyle lipgloss.Style

	OkStyle          lipgloss.Style
	WarnStyle        lipgloss.Style
	ScrollThumbStyle lipgloss.Style
	ScrollTrackStyle lipgloss.Style

	// Derived from rightTabBar — moved here so they rebuild with RebuildStyles.
	RightTabActiveLabel   lipgloss.Style
	RightTabActiveBracket lipgloss.Style
	RightTabInactive      lipgloss.Style
	RightTabMuted         lipgloss.Style

	// WarningMsgStyle is used for the "press ctrl+c again" status bar message.
	WarningMsgStyle lipgloss.Style

	// DashLabelStyle is used for the key-column label in dashboard memory rows.
	DashLabelStyle lipgloss.Style
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

	// PanelHeaderInactiveStyle: panel title when the panel is fully behind a
	// popup overlay — non-interactive. #3E444F.
	PanelHeaderInactiveStyle = lipgloss.NewStyle().
		Foreground(t.TextInactive)

	// PanelHeaderFadedStyle: panel title when the panel is active but has
	// lost focus to its sibling. #6C768A.
	PanelHeaderFadedStyle = lipgloss.NewStyle().
		Foreground(t.TextFaded)

	SepStyle = lipgloss.NewStyle().
		Foreground(t.Border)

	// SepInactiveStyle: border chars for the panel behind a popup overlay.
	SepInactiveStyle = lipgloss.NewStyle().
		Foreground(t.TextInactive)

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

	SessionLangStyle = lipgloss.NewStyle().
		Background(t.Border).
		Foreground(t.TextPrimary)

	SessionLangSelectedStyle = lipgloss.NewStyle().
		Bold(true).
		Background(t.Accent).
		Foreground(t.ContrastText)

	SessionLangFadedStyle = lipgloss.NewStyle().
		Background(t.TextInactive).
		Foreground(t.TextFaded)

	// InactiveStyle: every character in the panel that is behind a popup.
	InactiveStyle = lipgloss.NewStyle().
		Foreground(t.TextInactive)

	// MutedStyle: low-priority secondary text (durations, hints, etc.).
	MutedStyle = lipgloss.NewStyle().
		Foreground(t.TextMuted)

	// FadedStyle: content in a panel that is active but lost focus to sibling.
	// Used for session list items and timestamp rows. #6C768A.
	FadedStyle = lipgloss.NewStyle().
		Foreground(t.TextFaded)

	// TimestampActiveStyle: timestamp rows when the left panel has focus.
	TimestampActiveStyle = lipgloss.NewStyle().
		Foreground(t.TextPrimary)

	// TimestampFadedStyle: timestamp rows when the right panel has focus. #6C768A.
	TimestampFadedStyle = lipgloss.NewStyle().
		Foreground(t.TextFaded)

	// DetailStyle: values in the call-detail right panel.
	DetailStyle = lipgloss.NewStyle().
		Foreground(t.TextSecondary)

	// HintStyle: column headers and the global key-hint bar.
	HintStyle = lipgloss.NewStyle().
		Foreground(t.TextHint)

	TabActiveStyle = lipgloss.NewStyle().
		Background(t.Accent).
		Foreground(t.ContrastText).
		Align(lipgloss.Center)

	TabInactiveStyle = lipgloss.NewStyle().
		Background(t.Border).
		Foreground(t.TextPrimary).
		Align(lipgloss.Center)

	TabActiveEdgeStyle = lipgloss.NewStyle().Foreground(t.Accent)
	TabInactiveEdgeStyle = lipgloss.NewStyle().Foreground(t.Border)

	RightTabActiveLabel = lipgloss.NewStyle().Foreground(t.Accent).Bold(true)
	RightTabActiveBracket = lipgloss.NewStyle().Foreground(t.Accent)
	RightTabInactive = lipgloss.NewStyle().Foreground(t.TextFaded)
	RightTabMuted = lipgloss.NewStyle().Foreground(t.TextMuted)

	WarningMsgStyle = lipgloss.NewStyle().Foreground(t.Warning)

	DashLabelStyle = lipgloss.NewStyle().Foreground(t.Key)

	StatusStyle = lipgloss.NewStyle().
		Foreground(t.TextHint)

	StatusKeyStyle = lipgloss.NewStyle().
		Foreground(t.TextMuted)

	LogStatusStyle = lipgloss.NewStyle().
		Background(t.SelectionBackground).
		Foreground(t.TextPrimary)

	SettingsBarStyle = lipgloss.NewStyle().
		Background(t.SelectionBackground).
		Foreground(t.TextHint)

	SettingsBarKeyStyle = lipgloss.NewStyle().
		Bold(true).
		Background(t.SelectionBackground).
		Foreground(t.Accent)

	SettingsBarMsgStyle = lipgloss.NewStyle().
		Background(t.SelectionBackground).
		Foreground(t.TextPrimary)

	LogSelectedStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(t.Accent)

	LogDetailKeyStyle = lipgloss.NewStyle().
		Foreground(t.Key)

	LogDetailGutterStyle = lipgloss.NewStyle().
		Foreground(t.TextFaded)

	OkStyle = lipgloss.NewStyle().
		Foreground(t.Success)

	WarnStyle = lipgloss.NewStyle().
		Foreground(t.Warning)

	ScrollThumbStyle = lipgloss.NewStyle().
		Foreground(t.ScrollThumb)

	ScrollTrackStyle = lipgloss.NewStyle().
		Foreground(t.ScrollTrack)
}
