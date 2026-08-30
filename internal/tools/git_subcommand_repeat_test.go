package tools

import (
	"strings"
	"testing"
)

// TestGit_RepeatedSubcommandInArgsRefused pins the guard against the observed
// caller slip: the subcommand given twice, the duplicate leading args. On the
// remote-leading verbs git then resolves the stray verb as the remote name and
// fails with the cryptic "does not appear to be a git repository" (verified
// against git 2.x), which has driven agents out of the plumb lane to shell git.
func TestGit_RepeatedSubcommandInArgsRefused(t *testing.T) {
	tool := NewGit(WriteDeps{}, nil)
	for _, sub := range []string{"push", "fetch", "pull"} {
		_, err := callGit(t, tool, map[string]any{"subcommand": sub, "args": []string{sub, "origin", "main"}})
		if err == nil || !strings.Contains(err.Error(), "must not repeat the subcommand") {
			t.Fatalf("git %s with duplicated leading arg: want the duplicated-subcommand refusal, got %v", sub, err)
		}
	}
}

// The refusal must not over-trigger: the correct arg shape gets past this
// check (it fails later, on policy — the nil policy refuses the network
// tier), and `git stash push`, whose first argument legitimately repeats a
// verb, is outside the guard entirely.
func TestGit_RepeatedSubcommandGuardDoesNotOverTrigger(t *testing.T) {
	tool := NewGit(WriteDeps{}, nil)
	if _, err := callGit(t, tool, map[string]any{"subcommand": "push", "args": []string{"origin", "main"}}); err != nil &&
		strings.Contains(err.Error(), "must not repeat the subcommand") {
		t.Fatalf("correct arg shape flagged as a duplicated subcommand: %v", err)
	}
	if _, err := callGit(t, tool, map[string]any{"subcommand": "stash", "args": []string{"push"}}); err != nil &&
		strings.Contains(err.Error(), "must not repeat the subcommand") {
		t.Fatalf("stash push wrongly caught by the duplicated-subcommand guard: %v", err)
	}
}
