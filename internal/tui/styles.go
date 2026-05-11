// Package tui implements the Bubble Tea v2 sessions dashboard for plumb.
package tui

import "charm.land/lipgloss/v2"

// Component styles. Rebuilt by RebuildStyles() — callers must invoke it before
// rendering so styles are consistent after any future theme change.
var (
	TitleStyle       lipgloss.Style
	PanelHeaderStyle lipgloss.Style
	SepStyle         lipgloss.Style
	KeyStyle         lipgloss.Style
	ValStyle         lipgloss.Style
	ItemStyle        lipgloss.Style
	SelectedStyle    lipgloss.Style
	MutedStyle       lipgloss.Style
	HintStyle        lipgloss.Style
	OkStyle          lipgloss.Style
	WarnStyle        lipgloss.Style
)

func init() { RebuildStyles() }

// RebuildStyles rebuilds all package-level styles. Call this after any theme change.
func RebuildStyles() {
	TitleStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("12")) // bright blue

	PanelHeaderStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("7")) // light gray

	SepStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("8")) // dim gray

	KeyStyle = lipgloss.NewStyle().
		Width(12).
		Foreground(lipgloss.Color("6")) // cyan

	ValStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("15")) // bright white

	ItemStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("7")) // light gray

	SelectedStyle = lipgloss.NewStyle().
		Bold(true).
		Foreground(lipgloss.Color("12")) // bright blue

	MutedStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("8")) // dim gray

	HintStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("8")) // dim gray

	OkStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("2")) // green

	WarnStyle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("3")) // yellow
}
