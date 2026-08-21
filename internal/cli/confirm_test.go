package cli

import (
	"errors"
	"strings"
	"testing"
)

// TestRunYesNoSelector_RefusesWithoutTerminal is the regression for a headless
// failure that was worse than it looked. `plumb stop` and `plumb restart` ran
// the bubbletea selector unconditionally, so with no TTY they died inside the
// TUI with "could not open TTY: open /dev/tty: device not configured" — an
// error naming neither the command that failed nor any way to proceed.
//
// `plumb trust`, the third caller of this same selector, had always checked for
// a terminal first. The guard therefore belongs in the selector, not in each
// caller, which is what this test pins: the check runs before tea.NewProgram.
//
// Under `go test` stdin is not a terminal, so this exercises the real path. If
// the guard is removed the test does not merely fail, it fails by trying to open
// a TTY — which is the bug.
func TestRunYesNoSelector_RefusesWithoutTerminal(t *testing.T) {
	if stdinIsTerminal() {
		t.Skip("stdin is a terminal; this test covers the headless path")
	}
	rendered := false
	ok, err := runYesNoSelector(func(int) string {
		rendered = true
		return "should not be reached"
	})
	if !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("expected ErrNoTerminal, got %v", err)
	}
	if ok {
		t.Error("a confirmation that could not be asked must never report Yes")
	}
	if rendered {
		t.Error("the prompt was rendered; the terminal check must precede the TUI")
	}
}

// TestNonInteractiveDaemonActionError_NamesTheRemedy pins the three things the
// raw TTY error did not say: which command refused, what it would have done, and
// how to proceed without a terminal. An agent reading only the message has to be
// able to act on it.
func TestNonInteractiveDaemonActionError_NamesTheRemedy(t *testing.T) {
	for _, p := range []daemonActionPrompt{stopActionPrompt, restartActionPrompt} {
		err := nonInteractiveDaemonActionError(p, 3)
		if err == nil {
			t.Fatalf("%s: expected an error", p.verb)
		}
		msg := err.Error()
		for _, want := range []string{p.verb, "--force", "not a terminal", p.consequence, "3 active session(s)"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s refusal missing %q:\n%s", p.verb, want, msg)
			}
		}
	}
}

// TestNonInteractiveDaemonActionError_OmitsSessionCountWhenZero keeps the
// message honest: claiming "0 active session(s) would be interrupted" invites a
// reader to discount a warning that is accurate in every other case.
func TestNonInteractiveDaemonActionError_OmitsSessionCountWhenZero(t *testing.T) {
	msg := nonInteractiveDaemonActionError(stopActionPrompt, 0).Error()
	if strings.Contains(msg, "0 active session") {
		t.Errorf("zero sessions should not be announced as interrupted:\n%s", msg)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("the remedy must survive the zero-session path:\n%s", msg)
	}
}
