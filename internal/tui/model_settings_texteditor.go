package tui

import (
	tea "charm.land/bubbletea/v2"
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
	content := []string{editorInputField(e.input, listFieldWidth)}
	status := editorHints([2]string{"enter", "save"}, [2]string{"esc", "cancel"})
	return editorModalBox(e.title, content, status)
}
