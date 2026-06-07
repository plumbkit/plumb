package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// textEditor is a single-line text-input popup for string settings — currently
// the per-language [lsp.<lang>] command. enter saves, esc cancels (discards),
// mirroring the list editor's conventions.
//
// Concurrency: TUI-thread only, like every other model field.
type textEditor struct {
	key     settingKey
	lspLang string
	title   string
	input   string
}

func newTextEditor(key settingKey, lspLang, title, current string) *textEditor {
	return &textEditor{key: key, lspLang: lspLang, title: title, input: current}
}

// Update handles one key. done closes the editor; save (enter) means persist,
// esc cancels and discards.
func (e *textEditor) Update(msg tea.KeyPressMsg) (done, save bool) {
	switch msg.String() {
	case "esc":
		return true, false
	case "enter":
		return true, true
	case "backspace":
		if len(e.input) > 0 {
			e.input = e.input[:len(e.input)-1]
		}
	case "ctrl+u":
		e.input = ""
	default:
		s := msg.String()
		if len(s) == 1 && s[0] >= 32 && s[0] < 127 {
			e.input += s
		}
	}
	return false, false
}

func (e *textEditor) renderModal(bg string, width, height int) string {
	return spliceOverlay(bg, e.box(), width, height)
}

func (e *textEditor) box() string {
	const pad = 2
	field := e.input
	if r := []rune(field); len(r) > listFieldWidth {
		field = string(r[len(r)-listFieldWidth:])
	}
	field += strings.Repeat(" ", max(listFieldWidth-lipgloss.Width(field), 0))
	rows := []string{
		"",
		"  " + SepStyle.Render("[") + DetailStyle.Render(field) + SepStyle.Render("]"),
		"",
		MutedStyle.Render("enter") + StatusStyle.Render(" save  ") + MutedStyle.Render("esc") + StatusStyle.Render(" cancel"),
	}
	title := " " + e.title + " "
	contentW := lipgloss.Width(title)
	for _, row := range rows {
		if w := lipgloss.Width(row); w > contentW {
			contentW = w
		}
	}
	innerW := contentW + pad*2

	var b strings.Builder
	dashes := max(innerW-1-lipgloss.Width(title), 0)
	b.WriteString(SepStyle.Render("╭─") + PanelHeaderStyle.Render(title) + SepStyle.Render(strings.Repeat("─", dashes)+"╮") + "\n")
	for _, row := range rows {
		rpad := max(innerW-pad-lipgloss.Width(row), 0)
		b.WriteString(SepStyle.Render("│") + strings.Repeat(" ", pad) + row + strings.Repeat(" ", rpad) + SepStyle.Render("│") + "\n")
	}
	b.WriteString(SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯"))
	return b.String()
}
