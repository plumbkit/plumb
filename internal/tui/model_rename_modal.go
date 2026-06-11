package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/session"
)

// renameSessionModal is the modal for renaming the current session.
type renameSessionModal struct {
	currentName   string
	input         string
	validationErr string
}

func newRenameSessionModal(currentName string) renameSessionModal {
	return renameSessionModal{
		currentName: currentName,
		input:       "",
	}
}

func (r *renameSessionModal) validateInput() error {
	if strings.TrimSpace(r.input) == "" {
		return fmt.Errorf("name is required")
	}
	_, err := session.NormaliseName(r.input)
	return err
}

func (r *renameSessionModal) Update(msg tea.Msg) (renameSessionModal, bool) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "esc":
			return *r, false // cancel
		case "enter":
			if err := r.validateInput(); err != nil {
				r.validationErr = err.Error()
				return *r, false
			}
			return *r, true // confirmed
		case "ctrl+u":
			r.input = ""
			r.validationErr = ""
			return *r, false
		case "backspace":
			if len(r.input) > 0 {
				r.input = r.input[:len(r.input)-1]
				r.validationErr = ""
			}
			return *r, false
		default:
			// Only accept printable ASCII characters
			str := msg.String()
			if len(str) == 1 && str[0] >= 32 && str[0] < 127 {
				r.input += str
				if err := r.validateInput(); err != nil {
					r.validationErr = err.Error()
				} else {
					r.validationErr = ""
				}
			}
			return *r, false
		}
	}
	return *r, false
}

// paste appends pasted text to the name input and re-validates.
func (r *renameSessionModal) paste(text string) {
	r.input += text
	if err := r.validateInput(); err != nil {
		r.validationErr = err.Error()
	} else {
		r.validationErr = ""
	}
}

// renderModal composites the modal box, centred, over the (already dimmed)
// background. spliceOverlay handles the centring — the box positions itself.
func (r renameSessionModal) renderModal(bg string, width, height int) string {
	return spliceOverlay(bg, r.box(), width, height)
}

// fieldWidth is the visible width of the editable name field.
const fieldWidth = 34

// box renders the rename-session modal as a self-contained, themed box using
// the same rounded-border idiom as the help and theme-picker overlays. It does
// not position itself; spliceOverlay centres it.
func (r renameSessionModal) box() string {
	const pad = 2 // blank columns each side of the content

	rows := r.contentRows()
	contentW := lipgloss.Width(" Rename Session ")
	for _, row := range rows {
		if w := lipgloss.Width(row); w > contentW {
			contentW = w
		}
	}
	innerW := contentW + pad*2

	var b strings.Builder
	title := " Rename Session "
	dashes := max(innerW-1-lipgloss.Width(title), 0)
	b.WriteString(SepStyle.Render("╭─") + PanelHeaderStyle.Render(title) + SepStyle.Render(strings.Repeat("─", dashes)+"╮") + "\n")
	for _, row := range rows {
		rpad := max(innerW-pad-lipgloss.Width(row), 0)
		b.WriteString(SepStyle.Render("│") + strings.Repeat(" ", pad) + row + strings.Repeat(" ", rpad) + SepStyle.Render("│") + "\n")
	}
	b.WriteString(SepStyle.Render("╰" + strings.Repeat("─", innerW) + "╯"))
	return b.String()
}

// contentRows builds the inner, border-free rows of the modal. Each returned
// string is a styled cell whose visible width box() pads to the inner width.
func (r renameSessionModal) contentRows() []string {
	current := MutedStyle.Render("Current   ") + DetailStyle.Render(r.currentName)

	shown := r.input
	if runes := []rune(shown); len(runes) > fieldWidth {
		shown = string(runes[len(runes)-fieldWidth:])
	}
	shown += strings.Repeat(" ", max(fieldWidth-lipgloss.Width(shown), 0))
	input := MutedStyle.Render("New name  ") + SepStyle.Render("[") + DetailStyle.Render(shown) + SepStyle.Render("]")

	var status string
	switch {
	case r.validationErr != "":
		msg := r.validationErr
		if len(msg) > fieldWidth {
			msg = msg[:fieldWidth-1] + "…"
		}
		status = WarnStyle.Render("⚠ " + msg)
	case r.input != "":
		status = OkStyle.Render("✓ valid")
	}

	help := MutedStyle.Render("esc") + StatusStyle.Render(" cancel    ") + MutedStyle.Render("enter") + StatusStyle.Render(" save")

	return []string{"", current, "", input, "", status, "", help}
}
