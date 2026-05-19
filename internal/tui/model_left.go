package tui

import (
	"fmt"

	"charm.land/lipgloss/v2"
)

func (m Model) leftLines() []string {
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
		m1, m2 := " Daemon running.", " Call a tool to begin."
		if !daemonRunning() {
			m1, m2 = " No active sessions.", ""
		}
		if lf {
			lines = append(lines, MutedStyle.Render(m1))
			if m2 != "" {
				lines = append(lines, MutedStyle.Render(m2))
			}
		} else {
			lines = append(lines, InactiveStyle.Render(m1))
			if m2 != "" {
				lines = append(lines, InactiveStyle.Render(m2))
			}
		}
		return lines
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
		if i == m.cursor {
			lines = append(lines, SelectedStyle.Render(firstLine))
			lines = append(lines, SelectedStyle.Render(secondLine))
		} else {
			if lf {
				lines = append(lines, ItemStyle.Render(firstLine))
				lines = append(lines, MutedStyle.Render(secondLine))
			} else {
				lines = append(lines, FadedStyle.Render(firstLine))
				lines = append(lines, FadedStyle.Render(secondLine))
			}
		}
		lines = append(lines, "")
	}
	return lines
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
