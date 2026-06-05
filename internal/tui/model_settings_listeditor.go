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
	title   string
	entries []string
	cursor  int    // 0..len(entries)-1 selects an entry
	adding  bool   // true while typing a new entry
	input   string // text buffer while adding
}

func newListEditor(key settingKey, title string, entries []string) *listEditor {
	return &listEditor{
		key:     key,
		title:   title,
		entries: append([]string(nil), entries...),
	}
}

// Update handles one key. done is true when the editor should close; save is
// true only when it should close AND persist e.entries (enter), false on cancel
// (esc, the conventional "discard" action).
func (e *listEditor) Update(msg tea.KeyPressMsg) (done, save bool) {
	if e.adding {
		e.updateAdding(msg)
		return false, false
	}
	switch msg.String() {
	case "esc":
		return true, false // cancel — discard changes
	case "enter", "ctrl+s":
		return true, true // save & close
	case "up", "k":
		if e.cursor > 0 {
			e.cursor--
		}
	case "down", "j":
		if e.cursor < len(e.entries)-1 {
			e.cursor++
		}
	case "a", "+":
		e.adding = true
		e.input = ""
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
		e.adding = false
		e.input = ""
	case "enter":
		if v := strings.TrimSpace(e.input); v != "" {
			e.entries = append(e.entries, v)
			e.cursor = len(e.entries) - 1 // park on the new entry
		}
		e.adding = false
		e.input = ""
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

const listFieldWidth = 46

func (e *listEditor) box() string {
	const pad = 2
	rows := e.contentRows()
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

// contentRows builds the inner rows: each entry, the add row (or the input field
// while adding), and a help line.
func (e *listEditor) contentRows() []string {
	rows := []string{""}
	if len(e.entries) == 0 && !e.adding {
		rows = append(rows, MutedStyle.Render("(inherits — no entries; press a to add)"))
	}
	for i, entry := range e.entries {
		cursor := "  "
		style := ItemStyle
		if !e.adding && i == e.cursor {
			cursor = "❯ "
			style = SelectedStyle
		}
		rows = append(rows, cursor+style.Render(truncList(entry)))
	}
	rows = append(rows, e.addRow())
	var help string
	if e.adding {
		help = MutedStyle.Render("enter") + StatusStyle.Render(" add    ") + MutedStyle.Render("esc") + StatusStyle.Render(" cancel")
	} else {
		help = MutedStyle.Render("a") + StatusStyle.Render(" add  ") + MutedStyle.Render("d") + StatusStyle.Render(" remove  ") +
			MutedStyle.Render("enter") + StatusStyle.Render(" save  ") + MutedStyle.Render("esc") + StatusStyle.Render(" cancel")
	}
	return append(rows, "", help)
}

// addRow renders the editable input field while adding, or a static "+ add"
// affordance otherwise (adding is triggered by `a`, not by selecting a row).
func (e *listEditor) addRow() string {
	if !e.adding {
		return "  " + MutedStyle.Render("+ add (a)")
	}
	shown := e.input
	if r := []rune(shown); len(r) > listFieldWidth {
		shown = string(r[len(r)-listFieldWidth:])
	}
	shown += strings.Repeat(" ", max(listFieldWidth-lipgloss.Width(shown), 0))
	return "  " + SepStyle.Render("[") + DetailStyle.Render(shown) + SepStyle.Render("]")
}

func truncList(s string) string {
	if r := []rune(s); len(r) > listFieldWidth {
		return string(r[:listFieldWidth-1]) + "…"
	}
	return s
}
