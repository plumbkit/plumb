package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/golimpio/plumb/internal/session"
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

func (r renameSessionModal) renderModal(bg string, width, height int) string {
	return spliceOverlay(bg, r.View(width, height), width, height)
}

func (r renameSessionModal) View(width, height int) string {
	// Center the modal box
	modalWidth := 55
	modalHeight := 8
	topPad := (height - modalHeight) / 2
	leftPad := (width - modalWidth) / 2

	if topPad < 0 {
		topPad = 0
	}
	if leftPad < 0 {
		leftPad = 0
	}

	// Build modal content
	lines := []string{}

	// Top border
	lines = append(lines, "┌─ Rename Session "+strings.Repeat("─", modalWidth-18)+"┐")

	// Empty line
	lines = append(lines, "│"+strings.Repeat(" ", modalWidth-2)+"│")

	// Current name
	currentLine := fmt.Sprintf("│  Current name: %-37s│", r.currentName)
	lines = append(lines, currentLine)

	// Empty line
	lines = append(lines, "│"+strings.Repeat(" ", modalWidth-2)+"│")

	// Input line
	inputField := r.input
	if len(inputField) > modalWidth-14 {
		inputField = inputField[len(inputField)-(modalWidth-14):]
	}
	inputLine := fmt.Sprintf("│  New name: [%-40s│", inputField+strings.Repeat(" ", modalWidth-14-len(inputField))+"]")
	lines = append(lines, inputLine)

	// Validation feedback
	if r.validationErr != "" {
		errMsg := "  ⚠ " + r.validationErr
		if len(errMsg) > modalWidth-4 {
			errMsg = errMsg[:modalWidth-4]
		}
		lines = append(lines, "│"+errMsg+strings.Repeat(" ", modalWidth-2-len(errMsg))+"│")
	} else if r.validationErr == "" && r.input != "" {
		lines = append(lines, "│  ✓ valid"+strings.Repeat(" ", modalWidth-11)+"│")
	} else {
		lines = append(lines, "│"+strings.Repeat(" ", modalWidth-2)+"│")
	}

	// Help text
	helpLine := "  Press Esc to cancel, Enter to save."
	lines = append(lines, "│"+helpLine+strings.Repeat(" ", modalWidth-2-len(helpLine))+"│")

	// Bottom border
	lines = append(lines, "└"+strings.Repeat("─", modalWidth-2)+"┘")

	// Build output with padding
	var output strings.Builder
	for i := 0; i < topPad; i++ {
		output.WriteString("\n")
	}
	for _, line := range lines {
		output.WriteString(strings.Repeat(" ", leftPad))
		output.WriteString(line)
		output.WriteString("\n")
	}

	return output.String()
}
