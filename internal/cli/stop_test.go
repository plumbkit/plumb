package cli

import (
	"path/filepath"
	"slices"
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
	m := yesNoModel{cursor: 1, render: func(cursor int) string {
		return renderStopConfirmationPrompt(stopActionPrompt.consequence, 2, cursor)
	}}
	if m.cursor != 1 {
		t.Fatalf("default cursor = %d, want No index 1", m.cursor)
	}
	got := ansiStripForCLITest(m.View().Content)
	if !strings.Contains(got, "┊ ❯ No") {
		t.Fatalf("default view should select No:\n%s", got)
	}
}

func TestStopConfirmationModelKeyboardFlow(t *testing.T) {
	newModel := func() yesNoModel {
		return yesNoModel{cursor: 1, render: func(cursor int) string {
			return renderStopConfirmationPrompt(stopActionPrompt.consequence, 2, cursor)
		}}
	}
	m := newModel()
	updated, cmd := m.Update(keyPress("up"))
	if cmd != nil {
		t.Fatal("navigation should not quit")
	}
	m = updated.(yesNoModel)
	if m.cursor != 0 {
		t.Fatalf("after up cursor = %d, want Yes index 0", m.cursor)
	}

	updated, cmd = m.Update(keyPress("enter"))
	if cmd == nil {
		t.Fatal("enter should quit")
	}
	m = updated.(yesNoModel)
	if !m.confirmed {
		t.Fatal("enter on Yes should confirm")
	}

	updated, cmd = newModel().Update(keyPress("enter"))
	if cmd == nil {
		t.Fatal("enter should quit")
	}
	if updated.(yesNoModel).confirmed {
		t.Fatal("enter on default No should cancel")
	}

	updated, cmd = newModel().Update(keyPress("y"))
	if cmd == nil {
		t.Fatal("y should quit")
	}
	m = updated.(yesNoModel)
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

// TestOpenFilesUnderDir_DecidesDaemonOwnership pins the rule that keeps
// `plumb stop` inside the environment it was invoked in.
//
// The sweep that reaches this rule is `pgrep -f "plumb daemon"`, which matches
// every plumb daemon on the machine regardless of which XDG tree it belongs to.
// Unscoped, that made `plumb stop` in an isolated tree — a test harness, a second
// checkout, a container mount — SIGTERM the developer's live daemon. It is not
// hypothetical: an integration harness did exactly that, restarting an operator's
// daemon three times mid-session while it served eleven other connections, and
// stretching one test from 15s to 347s as the two fought.
//
// So this is the whole blast-radius decision, and it is unit-tested rather than
// left to an integration test that can only observe the damage after the fact.
func TestOpenFilesUnderDir_DecidesDaemonOwnership(t *testing.T) {
	t.Parallel()
	const runtimeDir = "/Users/dev/Library/Caches/plumb"
	ours := "n" + runtimeDir + "/plumb.sock\nn/dev/null\n"

	cases := []struct {
		name   string
		output string
		dir    string
		want   bool
	}{
		{"a daemon holding our socket is ours", ours, runtimeDir, true},
		{"a trailing separator on the dir is tolerated", ours, runtimeDir + "/", true},
		{
			// The case the whole fix exists for: an isolated harness daemon.
			name:   "a daemon in another tree is not ours",
			output: "n/tmp/plsmk123/Library/Caches/plumb/plumb.sock\n",
			dir:    runtimeDir,
			want:   false,
		},
		{
			// A prefix match without the separator would claim this one, and it
			// belongs to a different environment entirely.
			name:   "a sibling directory sharing a prefix is not ours",
			output: "n" + runtimeDir + "-other/plumb.sock\n",
			dir:    runtimeDir,
			want:   false,
		},
		{
			// Fail closed: lsof that told us nothing is not evidence of ownership.
			name: "no open files proves nothing", output: "", dir: runtimeDir, want: false,
		},
		{
			// A degenerate dir would claim half the filesystem, and with it every
			// daemon on the machine — the exact failure, arrived at differently.
			name:   "the filesystem root is never an ownership claim",
			output: "n/anything\n", dir: "/", want: false,
		},
		{"an empty dir is never an ownership claim", "n/anything\n", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := openFilesUnderDir(c.output, c.dir); got != c.want {
				t.Errorf("openFilesUnderDir(%q, %q) = %v, want %v", c.output, c.dir, got, c.want)
			}
		})
	}
}

// TestOwnedDirsFrom_ClaimsTheLegacyLocationToo guards against the mirror-image
// of the over-broad sweep.
//
// The legacy cache-dir runtime location is a documented, expected state, not an
// edge case: `plumb doctor` detects a daemon there and prints "run `plumb stop`,
// then reconnect", and `plumb serve` warns about it after an upgrade or when
// launched somewhere $XDG_RUNTIME_DIR is unset (cron, systemd, docker exec,
// ssh). Scoping the sweep to the current directory alone would make `plumb stop`
// report "Daemon is not running." while one is alive, and `plumb restart` would
// then spawn a duplicate beside it. Fixing an over-broad sweep must not produce
// an under-broad one.
//
// The RULE is tested rather than the environment, because the two locations
// coincide on some platforms — and there the environment-driven version of this
// test asserted nothing at all while the legacy arm could be deleted unnoticed.
func TestOwnedDirsFrom_ClaimsTheLegacyLocationToo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		current, legacy string
		want            []string
	}{
		{
			"both locations are claimed when they differ", "/run/user/1000/plumb", "/home/u/.cache/plumb",
			[]string{"/run/user/1000/plumb", "/home/u/.cache/plumb"},
		},
		{
			"a coinciding legacy location is not duplicated", "/home/u/.cache/plumb", "/home/u/.cache/plumb",
			[]string{"/home/u/.cache/plumb"},
		},
		{
			"no legacy location leaves just the current one", "/run/user/1000/plumb", "",
			[]string{"/run/user/1000/plumb"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := ownedDirsFrom(c.current, c.legacy); !slices.Equal(got, c.want) {
				t.Errorf("ownedDirsFrom(%q, %q) = %v, want %v", c.current, c.legacy, got, c.want)
			}
		})
	}
}

// TestOwnedRuntimeDirs_AlwaysClaimsTheCurrentLocation is the thin wiring check
// the rule test cannot make: whatever the environment resolves to, the directory
// this process would actually use must be claimed, or every daemon becomes
// unstoppable.
func TestOwnedRuntimeDirs_AlwaysClaimsTheCurrentLocation(t *testing.T) {
	dirs := ownedRuntimeDirs()
	current := filepath.Dir(daemonSocketPath())
	if !slices.Contains(dirs, current) {
		t.Fatalf("the current runtime dir %q is not claimed; dirs=%v", current, dirs)
	}
	for _, d := range dirs {
		if openFilesUnderDir("n/some/unrelated/path\n", d) {
			t.Errorf("claimed dir %q matches an unrelated path", d)
		}
	}
}
