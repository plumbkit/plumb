package tui

import (
	"fmt"
	"time"

	"charm.land/lipgloss/v2"

	"github.com/golimpio/plumb/internal/session"
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
		indicator := "∙"
		if selected {
			indicator = "❯"
		}
		name := s.Name
		if name == "" {
			name = s.ID
		}
		firstLine := " " + indicator + " " + name
		if badge := sessionListLangLabel(s); badge != "" {
			firstLine += " " + sessionLangBadge(badge, selected, lf)
		}
		if s.Health == "blocked" {
			firstLine += " !"
		}
		if s.Synthetic {
			firstLine += " (auto)"
		}
		if sessionIsIdle(s, idleThreshold(m.settingsCfg.Session.IdleThresholdMinutes)) {
			firstLine += " ~"
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

// idleThreshold converts the configured minute value to a duration, falling
// back to session.IdleSessionThreshold when the value is zero or negative
// (unset / invalid config).
func idleThreshold(minutes int) time.Duration {
	if minutes > 0 {
		return time.Duration(minutes) * time.Minute
	}
	return session.IdleSessionThreshold
}

// sessionIsIdle reports whether a session's last-seen time exceeds the given
// threshold. Falls back to StartedAt when LastSeenAt is not yet populated.
func sessionIsIdle(s session.Info, threshold time.Duration) bool {
	lastSeen := s.LastSeenAt
	if lastSeen.IsZero() {
		lastSeen = s.StartedAt
	}
	return !lastSeen.IsZero() && time.Since(lastSeen) >= threshold
}

// sessionLangLabel maps a session to its language display label. Prefer the
// marker-detected project language so Java/Rust/etc. still show their project
// type when no LSP adapter is attached; "?" is only for truly unknown projects.
func sessionLangLabel(s session.Info) string {
	language := s.DetectedLanguage
	if language == "" {
		language = s.Language
	}
	switch language {
	case "":
		return ""
	case "none":
		return "?"
	default:
		return language
	}
}

// adapterLangShort maps an LSP adapter binary to a short language label for the
// session-list badge, so a multi-language session shows which extra servers it
// drives (e.g. "go +html").
var adapterLangShort = map[string]string{
	"gopls":                       "go",
	"pyright":                     "python",
	"jdtls":                       "java",
	"rust-analyzer":               "rust",
	"sourcekit-lsp":               "swift",
	"zls":                         "zig",
	"typescript-language-server":  "ts",
	"kotlin-language-server":      "kotlin",
	"vscode-html-language-server": "html",
}

// sessionListLangLabel is the session-list badge text: the primary language plus
// a "+lang" hint for each secondary language server the session is driving. A
// Go web app also serving HTML shows "go +html"; single-language sessions are
// unchanged. The Details pane keeps the primary in Language and the full server
// set in Adapters, so this list-only label stays compact.
func sessionListLangLabel(s session.Info) string {
	label := sessionLangLabel(s)
	if len(s.Adapters) <= 1 {
		return label
	}
	for _, a := range s.Adapters[1:] {
		if l := adapterLangShort[a]; l != "" {
			label += " +" + l
		}
	}
	return label
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
