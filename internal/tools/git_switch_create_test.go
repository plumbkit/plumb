package tools

import (
	"reflect"
	"strings"
	"testing"
)

// TestNormaliseSwitchCreate covers the pure rewrite: -c/-C become their long
// forms only for the switch subcommand, and only as bare flag tokens.
func TestNormaliseSwitchCreate(t *testing.T) {
	cases := []struct {
		name     string
		sub      string
		args     []string
		wantArgs []string
		wantNote bool
	}{
		{"switch -c rewrites", "switch", []string{"-c", "feature"}, []string{"--create", "feature"}, true},
		{"switch -C rewrites", "switch", []string{"-C", "feature"}, []string{"--force-create", "feature"}, true},
		{"switch without create untouched", "switch", []string{"main"}, []string{"main"}, false},
		{"non-switch left alone", "log", []string{"-c", "x=y"}, []string{"-c", "x=y"}, false},
		{"branch -c left alone (its own copy flag)", "branch", []string{"-c", "a", "b"}, []string{"-c", "a", "b"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, note := normaliseSwitchCreate(tc.sub, tc.args)
			if !reflect.DeepEqual(got, tc.wantArgs) {
				t.Errorf("args = %v, want %v", got, tc.wantArgs)
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note presence = %v, want %v (note=%q)", note != "", tc.wantNote, note)
			}
		})
	}
}

// TestClassifySwitchCreate confirms the rewritten long forms classify as writes
// (so the global-flag denylist no longer pre-empts the tier), and that adding a
// discard flag still escalates to destructive.
func TestClassifySwitchCreate(t *testing.T) {
	if got := classifyGit("switch", []string{"--create", "feature"}); got != tierWrite {
		t.Errorf("switch --create = %v, want tierWrite", got)
	}
	if got := classifyGit("switch", []string{"--force-create", "feature"}); got != tierWrite {
		t.Errorf("switch --force-create = %v, want tierWrite", got)
	}
	if got := classifyGit("switch", []string{"--create", "feature", "--discard-changes"}); got != tierDestructive {
		t.Errorf("switch --create --discard-changes = %v, want tierDestructive", got)
	}
}

// TestGit_SwitchCreateFlag is the end-to-end proof: `git switch -c <branch>` now
// succeeds against a real repo (previously refused by the -c denylist), actually
// creates and switches to the branch, and the result carries the rewrite note.
func TestGit_SwitchCreateFlag(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })

	out, err := callGit(t, tool, map[string]any{"subcommand": "switch", "args": []string{"-c", "feature"}, "repo": dir})
	if err != nil {
		t.Fatalf("git switch -c should succeed, got: %v", err)
	}
	if !strings.Contains(out, "rewrote `git switch -c") {
		t.Errorf("expected the rewrite note in output, got: %q", out)
	}

	// Confirm the branch was actually created and is current.
	status, err := callGit(t, tool, map[string]any{"subcommand": "status", "repo": dir})
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if !strings.Contains(status, "feature") {
		t.Errorf("expected to be on branch 'feature', status: %q", status)
	}
}

// TestGit_DashCStillDeniedElsewhere proves the rewrite is scoped to switch: a
// non-switch subcommand still has -c (the config-injection vector) rejected.
func TestGit_DashCStillDeniedElsewhere(t *testing.T) {
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	_, err := callGit(t, tool, map[string]any{
		"subcommand": "log", "args": []string{"-c", "core.pager=touch /tmp/pwned", "--oneline"}, "repo": t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("non-switch -c must stay denied, got: %v", err)
	}
}
