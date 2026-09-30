package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git_merge_test.go covers the merge subcommand (#530): an ordinary merge is a
// write-tier operation that runs the merge hooks, its state flags are gated or
// refused, and a merge that stops on conflicts says which files conflict and
// leaves git's merging state for the caller to resolve.

// mergeFixture returns a repository on branch main whose branch "side" has
// diverged from it, so merging side can never fast-forward. Hooks are pinned to
// the repository's own directory: a developer's global core.hooksPath would
// otherwise run somewhere the test's hooks are not.
func mergeFixture(t *testing.T) string {
	t.Helper()
	requireGit(t)
	repo := initTestRepo(t)
	runGitDirect(t, repo, "branch", "-M", "main")
	runGitDirect(t, repo, "config", "commit.gpgsign", "false")
	runGitDirect(t, repo, "config", "core.hooksPath", filepath.Join(repo, ".git", "hooks"))
	commitFileDirect(t, repo, "side", "side.txt", "side\n")
	commitFileDirect(t, repo, "main", "main.txt", "main\n")
	return repo
}

// commitFileDirect commits file with content on branch (created from main when
// it does not exist yet) outside plumb, then leaves main checked out.
func commitFileDirect(t *testing.T, repo, branch, file, content string) {
	t.Helper()
	if branch != "main" {
		exists := exec.Command("git", "-C", repo, "rev-parse", "--verify", "-q", "refs/heads/"+branch).Run() == nil
		if exists {
			runGitDirect(t, repo, "switch", "-q", branch)
		} else {
			runGitDirect(t, repo, "switch", "-q", "-c", branch, "main")
		}
	}
	if err := os.WriteFile(filepath.Join(repo, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDirect(t, repo, "add", file)
	runGitDirect(t, repo, "commit", "-q", "-m", branch+": "+file)
	runGitDirect(t, repo, "switch", "-q", "main")
}

// writesOnlyGit is a git tool whose policy opens the write tier and nothing
// above it — the compiled default. A merge must succeed under it.
func writesOnlyGit(repo string) *Git {
	return NewGit(
		WriteDeps{WorkspaceFn: func(context.Context) string { return repo }},
		func() GitPolicy { return GitPolicy{AllowWrites: true} },
	)
}

// headParents returns how many parents HEAD has, read with plain git.
func headParents(t *testing.T, repo string) int {
	t.Helper()
	cmd := exec.Command("git", "rev-list", "--parents", "-n", "1", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git rev-list: %v", err)
	}
	return len(strings.Fields(string(out))) - 1
}

func installHook(t *testing.T, repo, name, body string) {
	t.Helper()
	path := filepath.Join(repo, ".git", "hooks", name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // G306: a git hook must be executable
		t.Fatal(err)
	}
}

// TestGit_MergeNoFFRunsAtTheWriteTier is the acceptance case from #530: merging
// the base branch into a work branch with --no-ff is the non-rewriting update,
// and it must run with only allow_writes — the tier commit needs.
func TestGit_MergeNoFFRunsAtTheWriteTier(t *testing.T) {
	repo := mergeFixture(t)
	before := gitHeadSHA(t, repo)
	out, err := callGit(t, writesOnlyGit(repo), map[string]any{
		"subcommand": "merge", "args": []string{"--no-ff", "--no-edit", "side"},
	})
	if err != nil {
		t.Fatalf("merge --no-ff under allow_writes: %v", err)
	}
	if gitHeadSHA(t, repo) == before {
		t.Fatalf("HEAD did not move; output:\n%s", out)
	}
	if n := headParents(t, repo); n != 2 {
		t.Errorf("HEAD has %d parents, want a two-parent merge commit", n)
	}
	if _, err := os.Stat(filepath.Join(repo, "side.txt")); err != nil {
		t.Errorf("side.txt was not merged in: %v", err)
	}
}

// TestGit_MergeFFOnly: --ff-only fast-forwards when it can and refuses (git's
// own refusal, surfaced as a failure) when it cannot, leaving HEAD alone.
func TestGit_MergeFFOnly(t *testing.T) {
	repo := mergeFixture(t)
	tool := writesOnlyGit(repo)
	before := gitHeadSHA(t, repo)
	if _, err := callGit(t, tool, map[string]any{"subcommand": "merge", "args": []string{"--ff-only", "side"}}); err == nil {
		t.Fatal("merge --ff-only of a diverged branch succeeded; git must refuse it")
	}
	if gitHeadSHA(t, repo) != before {
		t.Fatal("a refused --ff-only merge moved HEAD")
	}

	runGitDirect(t, repo, "switch", "-q", "-c", "behind", "HEAD~1")
	if _, err := callGit(t, tool, map[string]any{"subcommand": "merge", "args": []string{"--ff-only", "main"}}); err != nil {
		t.Fatalf("merge --ff-only of a descendant: %v", err)
	}
	if gitHeadSHA(t, repo) != before {
		t.Errorf("--ff-only did not fast-forward behind to main's %s", before[:7])
	}
}

// TestGit_MergeStateFlagsAreDestructive: --abort resets the working tree and
// --quit strands a half-done merge, so both need allow_destructive and confirm,
// exactly like cherry-pick's and rebase's state flags.
func TestGit_MergeStateFlagsAreDestructive(t *testing.T) {
	repo := mergeFixture(t)
	for _, flag := range []string{"--abort", "--quit"} {
		_, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": "merge", "args": []string{flag}})
		if err == nil || !strings.Contains(err.Error(), "destructive operations are disabled") {
			t.Errorf("merge %s under allow_writes only: want the destructive-tier refusal, got %v", flag, err)
		}
		_, err = callGit(t, sessionGitTool(repo, "s", "n"), map[string]any{"subcommand": "merge", "args": []string{flag}})
		if err == nil || !strings.Contains(err.Error(), "requires confirm: true") {
			t.Errorf("merge %s without confirm: want the confirm refusal, got %v", flag, err)
		}
	}
}

// TestGit_MergeRefusesFlagsThatEscapeTheToolsContract: --no-verify skips the
// hooks the tool promises always run, --edit / --continue need an editor the
// tool cannot drive, and --file reads a message from an arbitrary path. Each is
// refused before git runs, and --continue's refusal names the working route.
// Abbreviations are refused too, because git accepts any unambiguous prefix of
// a long option.
func TestGit_MergeRefusesFlagsThatEscapeTheToolsContract(t *testing.T) {
	repo := mergeFixture(t)
	before := gitHeadSHA(t, repo)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--no-verify", "side"}, "--no-verify"},
		{[]string{"--no-verif", "side"}, "--no-verify"},
		{[]string{"-e", "side"}, "editor"},
		{[]string{"--edit", "side"}, "editor"},
		{[]string{"--ed", "side"}, "editor"},
		{[]string{"-ne", "side"}, "editor"},
		{[]string{"-F", "/etc/passwd", "side"}, "--file"},
		{[]string{"-F/etc/passwd", "side"}, "--file"},
		{[]string{"--file=/etc/passwd", "side"}, "--file"},
		{[]string{"--fil=/etc/passwd", "side"}, "--file"},
		{[]string{"--continue"}, "subcommand \"commit\""},
		{[]string{"--cont"}, "subcommand \"commit\""},
		// A refused flag hidden behind another option's VALUE (#540 review). git
		// gives a value-taking option the next argument whatever it spells, so the
		// scan must too — otherwise the "value" it skips is really the next flag.
		{[]string{"--message", "-m", "--no-verify", "side"}, "--no-verify"},
		{[]string{"--message", "--", "--no-verify", "side"}, "--no-verify"},
		{[]string{"--mess", "-m", "--no-verify", "side"}, "--no-verify"},
		{[]string{"--message", "-s", "-e", "side"}, "editor"},
		{[]string{"--into-name", "-X", "-F", "/etc/passwd", "side"}, "--file"},
		{[]string{"--strategy", "-m", "--no-verify", "side"}, "--no-verify"},
		{[]string{"--strategy-option", "-m", "--no-verify", "side"}, "--no-verify"},
		{[]string{"--cleanup", "-m", "--no-verify", "side"}, "--no-verify"},
		{[]string{"-nm", "x", "--no-verify", "side"}, "--no-verify"},
		// A genuine `--` does not end the check either.
		{[]string{"side", "--", "--no-verify"}, "--no-verify"},
	}
	for _, c := range cases {
		_, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": "merge", "args": c.args})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("merge %v: want a refusal mentioning %q, got %v", c.args, c.want, err)
		}
	}
	if gitHeadSHA(t, repo) != before {
		t.Fatal("a refused merge moved HEAD")
	}
	// Positive control: -m takes the next token as its value, so a message that
	// happens to spell a refused flag is a message, not a flag.
	if _, err := callGit(t, writesOnlyGit(repo), map[string]any{
		"subcommand": "merge", "args": []string{"--no-ff", "-m", "-e is fine here", "side"},
	}); err != nil {
		t.Fatalf("merge -m <message resembling a flag>: %v", err)
	}
}

// TestCheckMergeArgs_ValuesAreNotFlags is the other direction of the scan: a
// value git reads as a value is never inspected as a flag, so these are all
// accepted. Over-refusal here would be harmless for safety but would make the
// scan's model of git's parser wrong, which is what the bypasses exploited.
func TestCheckMergeArgs_ValuesAreNotFlags(t *testing.T) {
	for _, args := range [][]string{
		{"--message", "--no-verify is just words", "side"},
		{"--message=--no-verify", "side"},
		{"--mes", "-e", "side"},
		{"-m", "--", "side"},
		{"-m--no-verify", "side"},
		{"--into-name", "-e", "side"},
		{"--in", "--edit", "side"},
		{"-X", "-F", "side"},
		{"-s", "-e", "side"},
		{"--cleanup", "--no-verify", "side"},
		{"-Sekey", "side"},
	} {
		if err := checkMergeArgs(args); err != nil {
			t.Errorf("checkMergeArgs(%q) = %v; git reads the flag-like token as a value, so it must be accepted", args, err)
		}
	}
}

// TestGit_MergeRunsItsHooks: the merge commit runs pre-merge-commit and
// commit-msg, as commit runs pre-commit and commit-msg. A failing hook fails the
// merge through the tool and leaves HEAD where it was.
func TestGit_MergeRunsItsHooks(t *testing.T) {
	repo := mergeFixture(t)
	seen := filepath.Join(t.TempDir(), "commit-msg.ran")
	installHook(t, repo, "commit-msg", "touch '"+seen+"'\n")
	installHook(t, repo, "pre-merge-commit", "echo 'pre-merge-commit says no'\nexit 1\n")
	before := gitHeadSHA(t, repo)
	_, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": "merge", "args": []string{"--no-ff", "--no-edit", "side"}})
	if err == nil || !strings.Contains(err.Error(), "pre-merge-commit says no") {
		t.Fatalf("merge with a failing pre-merge-commit hook: want the hook's refusal, got %v", err)
	}
	if gitHeadSHA(t, repo) != before {
		t.Fatal("HEAD moved although pre-merge-commit refused the merge")
	}
	// A refused pre-merge-commit may leave the merge in progress; clear it either way.
	_ = exec.Command("git", "-C", repo, "merge", "--abort").Run()

	installHook(t, repo, "pre-merge-commit", "exit 0\n")
	if _, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": "merge", "args": []string{"--no-ff", "--no-edit", "side"}}); err != nil {
		t.Fatalf("merge with passing hooks: %v", err)
	}
	if _, err := os.Stat(seen); err != nil {
		t.Errorf("commit-msg never ran for the merge commit: %v", err)
	}
}

// TestGit_MergeConflictNamesTheFilesAndKeepsMergingState: a merge that stops on
// conflicts is reported as such, naming every conflicted file and the two ways
// on, and git's merging state (MERGE_HEAD) is left for the caller to resolve.
func TestGit_MergeConflictNamesTheFilesAndKeepsMergingState(t *testing.T) {
	repo := mergeFixture(t)
	commitFileDirect(t, repo, "side", "init.txt", "side's version\n")
	commitFileDirect(t, repo, "side", "clean.txt", "merges cleanly\n")
	commitFileDirect(t, repo, "main", "init.txt", "main's version\n")
	_, err := callGit(t, writesOnlyGit(repo), map[string]any{"subcommand": "merge", "args": []string{"--no-edit", "side"}})
	if err == nil {
		t.Fatal("a conflicting merge reported success")
	}
	msg := err.Error()
	// "in 1 file" also pins that clean.txt, which merged cleanly, is not listed.
	for _, want := range []string{"stopped with conflicts in 1 file", "\n  init.txt", "subcommand \"commit\"", "--abort"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict report does not mention %q:\n%s", want, msg)
		}
	}
	if _, statErr := os.Stat(filepath.Join(repo, ".git", "MERGE_HEAD")); statErr != nil {
		t.Errorf("MERGE_HEAD is gone: the repository must be left in git's merging state: %v", statErr)
	}
}

// TestGit_MergeHonoursExpectedHead: merge is a write-tier op, so expected_head
// refuses it before git runs when HEAD is elsewhere.
func TestGit_MergeHonoursExpectedHead(t *testing.T) {
	repo := mergeFixture(t)
	before := gitHeadSHA(t, repo)
	_, err := callGit(t, writesOnlyGit(repo), map[string]any{
		"subcommand": "merge", "args": []string{"--no-ff", "--no-edit", "side"}, "expected_head": "side",
	})
	if err == nil || !strings.Contains(err.Error(), "expected_head mismatch") {
		t.Fatalf("merge with a stale expected_head: want the mismatch refusal, got %v", err)
	}
	if gitHeadSHA(t, repo) != before {
		t.Fatal("a refused merge moved HEAD")
	}
}
