package cli

import (
	"errors"

	tea "charm.land/bubbletea/v2"
)

// The shared two-option Yes/No confirmation selector behind `plumb stop`,
// `plumb restart`, and `plumb trust`. The key handling and the default (No)
// are the point of the sharing: every confirmation plumb asks for behaves the
// same, and a new one cannot accidentally default to Yes, or forget that there
// may be no terminal to ask on. Only the prompt rendering differs between
// commands, supplied as a callback over the cursor position.

// ErrNoTerminal reports that a confirmation was needed but there is no terminal
// to ask on. Callers translate it into a message naming their own skip flag.
//
// This guard lives in the shared selector rather than in each caller because
// leaving it to callers is precisely what went wrong: `plumb trust` checked for
// a terminal, and `stop` and `restart` — the other two users of this same
// selector — did not. Headless, they died inside bubbletea with
//
//	tea.NewProgram(...).Run(): could not open TTY: open /dev/tty: device not configured
//
// which names no remedy and does not say which command failed. Worse, it is a
// failure an automated caller is most likely to hit and least likely to notice:
// an agent that ran `plumb restart` after rebuilding got that error, kept going,
// and measured a stale daemon for two hours believing it had restarted.
var ErrNoTerminal = errors.New("no terminal available for confirmation")

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
		case "enter", "space": // v2 names the space key "space", not " "
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
//
// Returns ErrNoTerminal without starting the TUI when stdin is not a terminal.
// It refuses rather than assuming Yes: these gates guard stopping a daemon out
// from under live sessions and granting command-execution trust, and a pipeline
// must not acquire either by side effect. Refusing with a remedy still leaves
// the command usable non-interactively — via its own skip flag — which is the
// part that was missing.
func runYesNoSelector(render func(cursor int) string) (bool, error) {
	if !stdinIsTerminal() {
		return false, ErrNoTerminal
	}
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
