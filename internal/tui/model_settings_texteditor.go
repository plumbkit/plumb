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
	// cmdField, when non-empty, marks this editor as editing a Commands-tab
	// field rather than a settingKey-backed row (see model_settings_commands.go).
	// The commit routes to the commands model, not stringField, and the target
	// command is the one under the list cursor.
	cmdField string

	fieldWidth int // per-render field width from the screen size; 0 falls back to listFieldWidth
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

// paste appends pasted text to the input field.
func (e *textEditor) paste(text string) {
	e.input += text
}

func (e *textEditor) renderModal(bg string, width, height int) string {
	e.fieldWidth = editorFieldWidth(width, len([]rune(e.input))+1)
	return spliceOverlay(bg, e.box(), width, height)
}

func (e *textEditor) box() string {
	width := e.fieldWidth
	if width == 0 {
		width = listFieldWidth
	}
	content := []string{editorInputField(e.input, width)}
	status := editorHints([2]string{"enter", "save"}, [2]string{"esc", "cancel"})
	return editorModalBox(e.title, content, status)
}
