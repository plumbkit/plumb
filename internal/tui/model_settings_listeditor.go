package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// listEditor is the popup for editing a list-valued setting (a []string such as
// workspace.read_roots or git.protected_branches). It edits an in-memory copy;
// the caller persists the entries to the active scope on save (enter); esc
// cancels and discards the in-memory edits.
//
// Concurrency: TUI-thread only, like every other model field.
type listEditor struct {
	key     settingKey
	lspLang string // non-empty when editing a per-language [lsp.<lang>] list field
	title   string
	entries []string
	cursor  int    // 0..len(entries)-1 selects an entry
	adding  bool   // true while typing in the input field (adding or editing)
	editing bool   // true while the input replaces the entry at cursor (vs appends a new one)
	input   string // text buffer while adding/editing
}

func newListEditor(key settingKey, title string, entries []string) *listEditor {
	return &listEditor{
		key:     key,
		title:   title,
		entries: append([]string(nil), entries...),
	}
}

// Update handles one key. done is true when the editor should close; entries are
// auto-saved, so closing (esc) always persists. enter edits the selected entry
// in place; a adds a new one.
func (e *listEditor) Update(msg tea.KeyPressMsg) (done, save bool) {
	if e.adding {
		e.updateAdding(msg)
		return false, false
	}
	switch msg.String() {
	case "esc":
		return true, true // close — entries are auto-saved
	case "up", "k":
		if e.cursor > 0 {
			e.cursor--
		}
	case "down", "j":
		if e.cursor < len(e.entries)-1 {
			e.cursor++
		}
	case "a", "+":
		e.adding, e.editing, e.input = true, false, ""
	case "enter":
		if e.cursor < len(e.entries) { // edit the selected entry in place
			e.adding, e.editing, e.input = true, true, e.entries[e.cursor]
		}
	case "d", "delete", "backspace":
		if e.cursor < len(e.entries) {
			e.entries = append(e.entries[:e.cursor], e.entries[e.cursor+1:]...)
			if e.cursor >= len(e.entries) && e.cursor > 0 {
				e.cursor = len(e.entries) - 1
			}
		}
	}
	return false, false
}

// updateAdding handles keys while typing a new entry.
func (e *listEditor) updateAdding(msg tea.KeyPressMsg) {
	switch msg.String() {
	case "esc":
		e.adding, e.editing, e.input = false, false, ""
	case "enter":
		if v := strings.TrimSpace(e.input); v != "" {
			if e.editing && e.cursor < len(e.entries) {
				e.entries[e.cursor] = v // replace the edited entry
			} else {
				e.entries = append(e.entries, v)
				e.cursor = len(e.entries) - 1 // park on the new entry
			}
		}
		e.adding, e.editing, e.input = false, false, ""
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
}

// renderModal composites the editor box centred over the dimmed background.
func (e *listEditor) renderModal(bg string, width, height int) string {
	return spliceOverlay(bg, e.box(), width, height)
}

const (
	listFieldWidth   = 32
	editorContentPad = 3 // gap between the border and content rows
	editorFramePad   = 1 // gap between the border and the status bar
)

func (e *listEditor) box() string {
	rows, status := e.content()
	return editorModalBox(e.title, rows, status)
}

// editorModalBox draws a centred popup: a bold frame title, a blank line, the
// content rows (indented editorContentPad on both sides), a blank line, then the
// status bar (indented editorFramePad). Shared by the list and text editors. A
// blank title falls back to "edit" so the frame is never an empty gap.
func editorModalBox(title string, content []string, status string) string {
	if strings.TrimSpace(title) == "" {
		title = "edit"
	}
	titleSeg := " " + title + " "
	innerW := lipgloss.Width(titleSeg)
	for _, row := range content {
		if w := editorContentPad*2 + lipgloss.Width(row); w > innerW {
			innerW = w
		}
	}
	if w := editorFramePad*2 + lipgloss.Width(status); w > innerW {
		innerW = w
	}

	var b strings.Builder
	dashes := max(innerW-1-lipgloss.Width(titleSeg), 0)
	b.WriteString(SepStyle.Render("╭─") + PanelHeaderStyle.Render(titleSeg) + SepStyle.Render(strings.Repeat("─", dashes)+"╮") + "\n")
	b.WriteString(editorBoxRow("", editorContentPad, innerW)) // blank line above content
	for _, row := range content {
		b.WriteString(editorBoxRow(row, editorContentPad, innerW))
	}
	b.WriteString(editorBoxRow("", editorContentPad, innerW)) // blank line below content
	b.WriteString(editorBoxRow(status, editorFramePad, innerW))
	b.WriteString(SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯"))
	return b.String()
}

// editorBoxRow renders one interior row indented by pad and padded to innerW.
func editorBoxRow(row string, pad, innerW int) string {
	rpad := max(innerW-pad-lipgloss.Width(row), 0)
	return SepStyle.Render("│") + strings.Repeat(" ", pad) + row + strings.Repeat(" ", rpad) + SepStyle.Render("│") + "\n"
}

// editorHints renders a panel status-bar shortcut legend in the standard form
// "key label  ·  key label …". Shared by every settings editor panel.
func editorHints(pairs ...[2]string) string {
	var b strings.Builder
	for i, p := range pairs {
		if i > 0 {
			b.WriteString(StatusStyle.Render("  ·  "))
		}
		b.WriteString(MutedStyle.Render(p[0]) + StatusStyle.Render(" "+p[1]))
	}
	return b.String()
}

// editorInputField renders the bracketed single-line input with a reverse-video
// block cursor after the typed text, padded to a fixed width. Shared by the add
// row here and the text editor.
func editorInputField(input string, width int) string {
	shown := input
	if r := []rune(shown); len(r) > width-1 { // keep the tail visible, leave room for the cursor
		shown = string(r[len(r)-(width-1):])
	}
	cursor := lipgloss.NewStyle().Reverse(true).Render(" ")
	fill := strings.Repeat(" ", max(width-lipgloss.Width(shown)-1, 0))
	// "[ … ]" — the space after [ aligns the input text with the "∙ entry" rows above.
	return SepStyle.Render("[ ") + DetailStyle.Render(shown) + cursor + fill + SepStyle.Render(" ]")
}

// content builds the interior content rows (entries, or the inline input while
// adding/editing) and the status-bar string. editorModalBox positions the blank
// lines and the status row.
func (e *listEditor) content() (rows []string, status string) {
	if len(e.entries) == 0 && !e.adding {
		rows = append(rows, MutedStyle.Render("(inherits — no entries)"))
	}
	for i, entry := range e.entries {
		if e.adding && e.editing && i == e.cursor { // inline edit field replaces the entry
			rows = append(rows, e.addRow())
			continue
		}
		if !e.adding && i == e.cursor { // selected: cursor + entry in the selection colour
			rows = append(rows, SelectedStyle.Render("❯ "+truncList(entry)))
		} else { // others: a muted bullet marks each entry
			rows = append(rows, MutedStyle.Render("∙ ")+ItemStyle.Render(truncList(entry)))
		}
	}
	if e.adding && !e.editing { // a new entry is appended at the end
		rows = append(rows, e.addRow())
	}
	if e.adding {
		status = editorHints([2]string{"enter", "ok"}, [2]string{"esc", "cancel"})
	} else {
		status = editorHints([2]string{"a", "add"}, [2]string{"d", "remove"}, [2]string{"enter", "edit"}, [2]string{"esc", "close"})
	}
	return rows, status
}

// addRow renders the editable input field; only shown while adding (triggered by
// `a`, not by selecting a row).
func (e *listEditor) addRow() string {
	return editorInputField(e.input, listFieldWidth)
}

func truncList(s string) string {
	if r := []rune(s); len(r) > listFieldWidth {
		return string(r[:listFieldWidth-1]) + "…"
	}
	return s
}
