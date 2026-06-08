package tui

import (
	"fmt"
	"strings"
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
		// One badge per active language server (a Go web app also serving HTML
		// shows two chips: GO HTML), each with its own background, separated by a
		// plain space.
		for _, b := range sessionLangs(s) {
			firstLine += " " + sessionLangBadge(b, selected, lf)
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

// adapterToLang maps an LSP adapter binary to its language name, so a session's
// recorded adapters can be turned back into per-language badges.
var adapterToLang = map[string]string{
	"gopls":                       "go",
	"pyright":                     "python",
	"jdtls":                       "java",
	"rust-analyzer":               "rust",
	"sourcekit-lsp":               "swift",
	"zls":                         "zig",
	"typescript-language-server":  "typescript",
	"kotlin-language-server":      "kotlin",
	"vscode-html-language-server": "html",
}

// langBadgeText maps a language name to its short, upper-case badge label.
// Unknown languages fall back to upper-casing the name.
var langBadgeText = map[string]string{
	"go":         "GO",
	"python":     "PY",
	"java":       "JAVA",
	"rust":       "RUST",
	"swift":      "SWIFT",
	"zig":        "ZIG",
	"typescript": "TS",
	"javascript": "JS",
	"kotlin":     "KT",
	"html":       "HTML",
}

func badgeText(lang string) string {
	if t, ok := langBadgeText[lang]; ok {
		return t
	}
	return strings.ToUpper(lang)
}

// sessionLanguages returns the language names a session drives: the primary
// first, then each secondary language server, lower-case (e.g. ["go", "html"]).
// Empty when the project language is unknown. Shared by the list badges and the
// Details pane so the two never disagree.
func sessionLanguages(s session.Info) []string {
	primary := sessionLangLabel(s)
	if primary == "" {
		return nil
	}
	out := []string{primary}
	for _, a := range secondaryAdapters(s.Adapters) {
		if lang := adapterToLang[a]; lang != "" {
			out = append(out, lang)
		}
	}
	return out
}

// sessionLangs returns the upper-case badge labels for the list view (GO, HTML).
func sessionLangs(s session.Info) []string {
	langs := sessionLanguages(s)
	out := make([]string, 0, len(langs))
	for _, l := range langs {
		out = append(out, badgeText(l))
	}
	return out
}

// secondaryAdapters returns the active adapters after the primary (index 0).
func secondaryAdapters(adapters []string) []string {
	if len(adapters) <= 1 {
		return nil
	}
	return adapters[1:]
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
