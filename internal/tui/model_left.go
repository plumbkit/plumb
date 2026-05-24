package tui

import (
	"fmt"

	"charm.land/lipgloss/v2"
)

func (m Model) leftLines() []string {
	if m.currentSection == 2 {
		return m.memoryLeftLines()
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
		if badge := sessionLangLabel(s.Language); badge != "" {
			firstLine += " " + sessionLangBadge(badge, selected, lf)
		}
		if s.Synthetic {
			firstLine += " (auto)"
		}
		path := "resolving…"
		if s.Folder != "" {
			// Subtract one extra so the rendered line stays within m.leftWidth-1
			// and the cell renderer never appends a trailing ….
			mf := max(m.leftWidth-len([]rune("    ╰─ "))-1, 0)
			path = contractPath(s.Folder, mf, m.settingsCfg.UI.PathStyle)
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

// sessionLangLabel maps a session's internal language value to its display
// label. An empty language (workspace not yet resolved) yields no badge; the
// LanguageNone sentinel ("none") becomes "?" — a valid session whose language
// could not be detected but still serves language-independent calls.
func sessionLangLabel(language string) string {
	switch language {
	case "":
		return ""
	case "none":
		return "?"
	default:
		return language
	}
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
