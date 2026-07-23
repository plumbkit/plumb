package tui

import (
	"reflect"
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
