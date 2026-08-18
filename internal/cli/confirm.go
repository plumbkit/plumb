package cli

import (
	tea "charm.land/bubbletea/v2"
)

// The shared two-option Yes/No confirmation selector behind `plumb stop`,
// `plumb restart`, and `plumb trust`. The key handling and the default (No)
// are the point of the sharing: every confirmation plumb asks for behaves the
// same, and a new one cannot accidentally default to Yes. Only the prompt
// rendering differs between commands, supplied as a callback over the cursor
// position.

type yesNoModel struct {
	render    func(cursor int) string
	cursor    int // 0 yes, 1 no
	confirmed bool
}

func (m yesNoModel) Init() tea.Cmd { return nil }

func (m yesNoModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k", "left", "h", "down", "j", "right", "l", "tab", "shift+tab":
			if m.cursor == 0 {
				m.cursor = 1
			} else {
				m.cursor = 0
			}
		case "y", "Y":
			m.cursor = 0
			m.confirmed = true
			return m, tea.Quit
		case "n", "N", "q", "esc", "ctrl+c":
			m.cursor = 1
			m.confirmed = false
			return m, tea.Quit
		case "enter", " ":
			m.confirmed = m.cursor == 0
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m yesNoModel) View() tea.View {
	return tea.NewView(m.render(m.cursor))
}

// runYesNoSelector runs the confirmation selector with the given prompt
// renderer and reports the answer. The cursor starts on No.
func runYesNoSelector(render func(cursor int) string) (bool, error) {
	finalModel, err := tea.NewProgram(yesNoModel{cursor: 1, render: render}).Run()
	if err != nil {
		return false, err
	}
	m, ok := finalModel.(yesNoModel)
	if !ok {
		return false, nil
	}
	return m.confirmed, nil
}
