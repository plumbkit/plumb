package tui

import (
	"fmt"

	"charm.land/lipgloss/v2"
)

func (m Model) leftLines() []string {
	if m.currentSection == 2 {
		return m.memoryLeftLines()
	}
	if m.currentSection == 4 {
		return m.settingsLeftLines()
	}
	lf := m.focusPanel == focusSessions

	var titleStyle lipgloss.Style
	if lf {
		titleStyle = PanelHeaderStyle
	} else {
		titleStyle = PanelHeaderFadedStyle
	}
	titleText := fmt.Sprintf(" Sessions (%d)", len(m.sessions))

	lines := []string{titleStyle.Render(titleText), ""}
	if len(m.sessions) == 0 {
		return append(lines, emptySessionsLines(lf)...)
	}
	for i, s := range m.sessions {
		selected := i == m.cursor
		indicator := "○"
		if selected {
			indicator = "❯"
		}
		name := s.Name
		if name == "" {
			name = s.ID
		}
		firstLine := " " + indicator + " " + name
		if s.Language != "" && s.Language != "none" {
			firstLine += " " + sessionLangBadge(s.Language, selected, lf)
		}
		if s.Synthetic {
			firstLine += " (auto)"
		}
		path := "resolving…"
		if s.Folder != "" {
			mf := max(m.leftWidth-len([]rune("    ╰─ ")), 0)
			path = contractPath(s.Folder, mf)
		}
		secondLine := "    ╰─ " + path
		lines = append(lines, leftSessionRowLines(firstLine, secondLine, selected, lf)...)
		lines = append(lines, "")
	}
	return lines
}

func emptySessionsLines(lf bool) []string {
	m1, m2 := " Daemon running.", " Call a tool to begin."
	if !daemonRunning() {
		m1, m2 = " No active sessions.", ""
	}
	style := InactiveStyle
	if lf {
		style = MutedStyle
	}
	lines := []string{style.Render(m1)}
	if m2 != "" {
		lines = append(lines, style.Render(m2))
	}
	return lines
}

func leftSessionRowLines(firstLine, secondLine string, selected, lf bool) []string {
	if selected {
		return []string{SelectedStyle.Render(firstLine), SelectedStyle.Render(secondLine)}
	}
	if lf {
		return []string{ItemStyle.Render(firstLine), MutedStyle.Render(secondLine)}
	}
	return []string{FadedStyle.Render(firstLine), FadedStyle.Render(secondLine)}
}

func sessionLangBadge(language string, selected, focused bool) string {
	badge := " " + language + " "
	switch {
	case selected:
		return SessionLangSelectedStyle.Render(badge)
	case focused:
		return SessionLangStyle.Render(badge)
	default:
		return SessionLangFadedStyle.Render(badge)
	}
}
