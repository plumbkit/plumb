package cli

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestPluralSessionCount(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{1, "1 active session"},
		{2, "2 active sessions"},
		{3, "3 active sessions"},
	}
	for _, tt := range tests {
		if got := pluralSessionCount(tt.n); got != tt.want {
			t.Fatalf("pluralSessionCount(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestRenderStopConfirmationPromptUsesPluralSessionCount(t *testing.T) {
	got := ansiStripForCLITest(renderStopConfirmationPrompt(stopActionPrompt.consequence, 1, 1))
	if !strings.Contains(got, "You have 1 active session.") {
		t.Fatalf("singular prompt missing session count:\n%s", got)
	}
	if strings.Contains(got, "choose") || strings.Contains(got, "enter confirm") {
		t.Fatalf("prompt should not include shortcut help text:\n%s", got)
	}
	if !strings.Contains(got, "Stopping the daemon will terminate all active sessions.\n    Confirm?") {
		t.Fatalf("prompt should keep confirmation on the next line:\n%s", got)
	}
	if strings.Contains(got, "Stop the daemon?") {
		t.Fatalf("prompt should use the shorter confirmation copy:\n%s", got)
	}
	if !strings.Contains(got, "┊   Yes") || !strings.Contains(got, "┊ ❯ No") {
		t.Fatalf("prompt missing yes/no options:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("prompt should end with a newline so later output does not overwrite the final option:\n%s", got)
	}

	got = ansiStripForCLITest(renderStopConfirmationPrompt(stopActionPrompt.consequence, 3, 0))
	if !strings.Contains(got, "You have 3 active sessions.") {
		t.Fatalf("plural prompt missing session count:\n%s", got)
	}
	if !strings.Contains(got, "┊ ❯ Yes") || !strings.Contains(got, "┊   No") {
		t.Fatalf("prompt should show selected Yes when cursor is 0:\n%s", got)
	}
}

func TestStopConfirmationModelDefaultNo(t *testing.T) {
	m := newStopConfirmationModel(2, stopActionPrompt)
	if m.cursor != 1 {
		t.Fatalf("default cursor = %d, want No index 1", m.cursor)
	}
	got := ansiStripForCLITest(m.View().Content)
	if !strings.Contains(got, "┊ ❯ No") {
		t.Fatalf("default view should select No:\n%s", got)
	}
}

func TestStopConfirmationModelKeyboardFlow(t *testing.T) {
	m := newStopConfirmationModel(2, stopActionPrompt)
	updated, cmd := m.Update(keyPress("up"))
	if cmd != nil {
		t.Fatal("navigation should not quit")
	}
	m = updated.(stopConfirmationModel)
	if m.cursor != 0 {
		t.Fatalf("after up cursor = %d, want Yes index 0", m.cursor)
	}

	updated, cmd = m.Update(keyPress("enter"))
	if cmd == nil {
		t.Fatal("enter should quit")
	}
	m = updated.(stopConfirmationModel)
	if !m.confirmed {
		t.Fatal("enter on Yes should confirm")
	}

	m = newStopConfirmationModel(2, stopActionPrompt)
	updated, cmd = m.Update(keyPress("enter"))
	if cmd == nil {
		t.Fatal("enter should quit")
	}
	m = updated.(stopConfirmationModel)
	if m.confirmed {
		t.Fatal("enter on default No should cancel")
	}

	m = newStopConfirmationModel(2, stopActionPrompt)
	updated, cmd = m.Update(keyPress("y"))
	if cmd == nil {
		t.Fatal("y should quit")
	}
	m = updated.(stopConfirmationModel)
	if !m.confirmed || m.cursor != 0 {
		t.Fatalf("y should select and confirm Yes, got cursor=%d confirmed=%v", m.cursor, m.confirmed)
	}
}

func ansiStripForCLITest(s string) string {
	return ansi.Strip(s)
}

func keyPress(s string) tea.KeyPressMsg {
	if s == "enter" {
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyEnter})
	}
	if s == "up" {
		return tea.KeyPressMsg(tea.Key{Code: tea.KeyUp})
	}
	return tea.KeyPressMsg(tea.Key{Text: s, Code: []rune(s)[0]})
}
