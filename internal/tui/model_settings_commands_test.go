package tui

import (
	"reflect"
	"strings"
	"testing"
)

func TestShellSplit(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"go build ./...", []string{"go", "build", "./..."}},
		{"go test -run 'Test Foo' ./...", []string{"go", "test", "-run", "Test Foo", "./..."}},
		{`echo "a b" c`, []string{"echo", "a b", "c"}},
		{"  spaced   out  ", []string{"spaced", "out"}},
		{"", nil},
	}
	for _, tc := range cases {
		if got := shellSplit(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("shellSplit(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// TestShellSplitJoinRoundTrip proves the exec editor round-trips an argv through
// its display/parse pair — the whole point of adding quoting is that an argument
// with a space survives an edit.
func TestShellSplitJoinRoundTrip(t *testing.T) {
	argvs := [][]string{
		{"go", "test", "-run", "Test Foo", "./..."},
		{"golangci-lint", "run"},
		{"echo", "a'b"},
		{"echo", "a b c"},
		{"echo", `a"b`},
		{"echo", "a 'quoted b"},
	}
	for _, argv := range argvs {
		joined := shellJoin(argv)
		if got := shellSplit(joined); !reflect.DeepEqual(got, argv) {
			t.Errorf("round-trip failed: %#v → %q → %#v", argv, joined, got)
		}
	}
}

// TestCommandsAddShortcut_UsesRawKeyNotNormalised is a regression test for the
// keymap cross-context coupling between the rebindable "refresh" action
// (default key "a") and the Commands tab's fixed, non-rebindable
// "add command" shortcut ("a"/"+"). With refresh rebound to "g", the fixed
// shortcut must still fire on a raw "a" press and must NOT fire on a raw "g"
// press — even though "g" normalises to "a" (refresh's canonical key) and "a"
// normalises to "" (its default having been displaced).
func TestCommandsAddShortcut_UsesRawKeyNotNormalised(t *testing.T) {
	km, warnings := resolveKeymap(map[string]string{"refresh": "g"})
	if len(warnings) != 0 {
		t.Fatalf("resolveKeymap(refresh=g) warnings = %v, want none", warnings)
	}
	newModel := func() Model {
		m := newSettingsModel()
		m.currentSection = 4 // Settings
		m.settingsTab = settingsTabCommands
		m.commandsFocus = cmdFocusList
		m.keys = km
		return m
	}

	// A raw "a" still opens the add-command editor, even though normalise()
	// rewrites a pressed "a" to "" (refresh's default key, now displaced).
	m := newModel()
	m, _ = m.handleSettingsSectionKey(keyPress("a"))
	if m.settingsTextEditor == nil || m.settingsTextEditor.cmdField != cmdEditAdd {
		t.Fatalf("raw \"a\" should open the add-command editor; settingsTextEditor = %+v", m.settingsTextEditor)
	}

	// A raw "g" (the rebound refresh key, which normalises to "a") must NOT
	// open the add-command editor — it is not this tab's shortcut.
	m = newModel()
	m, _ = m.handleSettingsSectionKey(keyPress("g"))
	if m.settingsTextEditor != nil {
		t.Fatalf("raw \"g\" (rebound refresh key) should not open the add-command editor; got %+v", m.settingsTextEditor)
	}
	if m.commandsFocus != cmdFocusList {
		t.Fatalf("raw \"g\" should leave the Commands tab focus unchanged, got %v", m.commandsFocus)
	}
}

// TestCommandsPolicyToggles_OnlyRequireSandboxIsRendered pins the Commands tab's
// policy pane to the keys that still do something.
//
// [commands] allow_shell and deny_network were rendered here as live switches
// after execute_shell_command — the only thing that read them — was retired, so
// flipping one reported success and changed nothing, while still rewriting the
// project config and so potentially invalidating that workspace's `plumb trust`
// grant. This tab had no test coverage at all when that happened, which is why
// it went unnoticed through a round of review.
func TestCommandsPolicyToggles_OnlyRequireSandboxIsRendered(t *testing.T) {
	toggles := renderedPolicyToggles(t, newCommandsModel())
	if len(toggles) != commandsToggleCount {
		t.Errorf("policy pane renders %d toggles but commandsToggleCount is %d — the cursor bounds "+
			"in handleCommandsTogglesKey are derived from the constant, so the two must agree.\ngot:\n%s",
			len(toggles), commandsToggleCount, strings.Join(toggles, "\n"))
	}
	joined := strings.Join(toggles, "\n")
	if !strings.Contains(joined, "require_sandbox") {
		t.Errorf("policy pane does not render require_sandbox, the one [commands] key that is still read:\n%s", joined)
	}
	for _, retired := range []string{"allow_shell", "deny_network"} {
		if strings.Contains(joined, retired) {
			t.Errorf("policy pane still renders a %q toggle. Nothing reads that key any more, so the "+
				"switch is a no-op that reports success — and writing it can cost the workspace its "+
				"`plumb trust` grant.\ngot:\n%s", retired, joined)
		}
	}
}

// TestCommandsToggleCursor_StaysWithinTheRenderedToggles is the off-by-one guard
// for collapsing three toggles to one: every index the navigation can park the
// cursor on must be an index the renderer actually draws, in both directions of
// travel (down off the last toggle descends into the panes; up out of the list
// pane comes back to the last toggle).
//
// Bounds are checked against what the RENDERER produces, not against
// commandsToggleCount, so raising the constant without adding a row — the exact
// shape of this defect — is caught rather than assumed away.
func TestCommandsToggleCursor_StaysWithinTheRenderedToggles(t *testing.T) {
	m := newCommandsModel()
	m.commandsFocus = cmdFocusToggles
	drawn := len(renderedPolicyToggles(t, m))

	// Walking down must never leave the cursor on an index past the last
	// rendered toggle; once past it, focus moves to the panes instead.
	for range drawn + 3 {
		m, _ = m.handleCommandsTogglesKey("down")
		if m.commandsToggleCursor >= drawn {
			t.Fatalf("commandsToggleCursor = %d, past the last of %d rendered toggles",
				m.commandsToggleCursor, drawn)
		}
	}
	if m.commandsFocus != cmdFocusList {
		t.Errorf("walking down past the last toggle should descend into the panes, got focus %v", m.commandsFocus)
	}

	// And coming back up out of the list pane must land on a real toggle.
	m, _ = m.handleCommandsListKey("up", "up")
	if m.commandsFocus != cmdFocusToggles {
		t.Fatalf("up from the top of the list pane should return to the toggles, got focus %v", m.commandsFocus)
	}
	if m.commandsToggleCursor < 0 || m.commandsToggleCursor >= drawn {
		t.Errorf("up from the list pane parked the cursor at %d, outside the %d rendered toggles",
			m.commandsToggleCursor, drawn)
	}
}

// newCommandsModel is a Settings model parked on the Commands tab.
func newCommandsModel() Model {
	m := newSettingsModel()
	m.settingsTab = settingsTabCommands
	return m
}

// renderedPolicyToggles returns the toggle rows the Commands tab actually draws
// between its "Policy" and "Allow-list" headers — the ground truth the cursor
// bounds have to agree with.
func renderedPolicyToggles(t *testing.T, m Model) []string {
	t.Helper()
	lines := m.renderCommandsLines(80)
	start, end := -1, -1
	for i, l := range lines {
		switch {
		case start < 0 && strings.Contains(l, "Policy ([commands])"):
			start = i
		case start >= 0 && end < 0 && strings.Contains(l, "Allow-list"):
			end = i
		}
	}
	if start < 0 || end < 0 {
		t.Fatalf("Commands tab did not render both section headers (policy=%d, allow-list=%d):\n%s",
			start, end, strings.Join(lines, "\n"))
	}
	var out []string
	for _, l := range lines[start+1 : end] {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
