package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindGitRoot_EmptyPathRefuses is the last line of defence against the
// cross-session leak: an empty path must error, never resolve to the daemon's
// cwd (os.Getwd), which is shared across connections and may belong to an
// unrelated repository.
func TestFindGitRoot_EmptyPathRefuses(t *testing.T) {
	_, err := findGitRoot("")
	if err == nil || !strings.Contains(err.Error(), "no repository path") {
		t.Fatalf("findGitRoot(\"\") must refuse, got %v", err)
	}
}

// TestEnhanceGitError_SubmodulePathspec covers the submodule-aware rewrite: a
// write that names a path inside a nested submodule must be redirected to the
// submodule's own repo root, with the original message preserved.
func TestEnhanceGitError_SubmodulePathspec(t *testing.T) {
	msg := "fatal: Pathspec 'plumb/site/index.html' is in submodule 'plumb'"
	got := enhanceGitError("/work/ops", msg)
	if !strings.Contains(got, msg) {
		t.Fatalf("hint must preserve the original message, got %q", got)
	}
	for _, want := range []string{"separate repository", "/work/ops/plumb", "repo="} {
		if !strings.Contains(got, want) {
			t.Errorf("want hint to contain %q, got %q", want, got)
		}
	}
}

// TestEnhanceGitError_IndexLockUnaffected proves the refactor left the existing
// stale-lock rewrite firing.
func TestEnhanceGitError_IndexLockUnaffected(t *testing.T) {
	msg := "fatal: Unable to create '/r/.git/index.lock': File exists"
	if got := enhanceGitError("/r", msg); !strings.Contains(got, "leftover lock") {
		t.Errorf("index.lock hint should still fire, got %q", got)
	}
}

// TestEnhanceGitError_UntrackedPathspec covers the freshly-created-file case: a
// path-limited commit naming an untracked path must be redirected to staging it
// first, with the original message preserved.
func TestEnhanceGitError_UntrackedPathspec(t *testing.T) {
	msg := "error: pathspec 'docs/PLAN-00041.md' did not match any file(s) known to git"
	got := enhanceGitError("/r", msg)
	if !strings.Contains(got, msg) {
		t.Fatalf("hint must preserve the original message, got %q", got)
	}
	for _, want := range []string{"not yet tracked", "docs/PLAN-00041.md", "add"} {
		if !strings.Contains(got, want) {
			t.Errorf("want hint to contain %q, got %q", want, got)
		}
	}
}

// TestEnhanceGitError_UntrackedNotSubmodule proves the untracked hint does not
// hijack the submodule failure (both are pathspec errors, distinct markers).
func TestEnhanceGitError_UntrackedNotSubmodule(t *testing.T) {
	msg := "fatal: Pathspec 'plumb/site/index.html' is in submodule 'plumb'"
	if got := enhanceGitError("/work/ops", msg); strings.Contains(got, "not yet tracked") {
		t.Errorf("submodule error must not get the untracked hint, got %q", got)
	}
}

// TestEnhanceGitError_Passthrough proves an unrelated error is returned verbatim.
func TestEnhanceGitError_Passthrough(t *testing.T) {
	msg := "fatal: not a git repository"
	if got := enhanceGitError("/r", msg); got != msg {
		t.Errorf("unrelated error must pass through unchanged, got %q", got)
	}
}

func TestFirstQuoted(t *testing.T) {
	cases := map[string]string{
		"is in submodule 'plumb'": "plumb",
		"no quotes here":          "",
		"unterminated 'open":      "",
	}
	for in, want := range cases {
		if got := firstQuoted(in); got != want {
			t.Errorf("firstQuoted(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- failure reporting: exit code + labelled, bounded streams ---

// exitError runs a real failing child so the returned error is a genuine
// *exec.ExitError, the shape gitCommandError expects from a non-zero git exit.
func exitError(t *testing.T) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit 1").Run()
	if err == nil {
		t.Fatal("expected a non-zero exit")
	}
	return err
}

// TestGitCommandError_LabelsStreams pins the failure shape: the exit code
// leads, then stderr and stdout each appear under their own label — stdout is
// context, never the error message itself.
func TestGitCommandError_LabelsStreams(t *testing.T) {
	err := gitCommandError("/r", "commit", []string{"commit", "-m", "x"}, exitError(t),
		"0 issues. file-size: OK\n", "hook: lint failed\n", "")
	msg := err.Error()
	firstLine, _, _ := strings.Cut(msg, "\n")
	if !strings.Contains(firstLine, "git commit: exit code 1") {
		t.Errorf("first line must lead with the exit code, got %q", firstLine)
	}
	if strings.Contains(firstLine, "0 issues") {
		t.Errorf("stdout must not be presented as the error, got %q", firstLine)
	}
	for _, want := range []string{"stderr:\nhook: lint failed", "stdout (last 40 lines):\n0 issues. file-size: OK"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want %q in error, got %q", want, msg)
		}
	}
}

// TestGitCommandError_StdoutOnlyFailure is the exact reported bug: a hook
// failing with output on stdout alone. Previously that stdout became the bare
// error string; now it is quoted under its own label beneath the exit code.
func TestGitCommandError_StdoutOnlyFailure(t *testing.T) {
	err := gitCommandError("/r", "commit", []string{"commit", "-m", "x"}, exitError(t), "0 issues. file-size: OK\n", "", "")
	msg := err.Error()
	firstLine, _, _ := strings.Cut(msg, "\n")
	if !strings.Contains(firstLine, "exit code 1") || strings.Contains(firstLine, "0 issues") {
		t.Errorf("first line must be the exit code, not the hook stdout, got %q", firstLine)
	}
	if !strings.Contains(msg, "stdout (last 40 lines):\n0 issues. file-size: OK") {
		t.Errorf("stdout must appear labelled, got %q", msg)
	}
	if strings.Contains(msg, "stderr:") {
		t.Errorf("an empty stderr must not get a label, got %q", msg)
	}
}

// TestGitCommandError_NoOutput covers a failure with nothing on either stream
// (e.g. a rejected push): the error is just the exit code, no empty labels.
func TestGitCommandError_NoOutput(t *testing.T) {
	err := gitCommandError("/r", "push", []string{"push"}, exitError(t), "", "", "")
	if got, want := err.Error(), "git push: exit code 1"; got != want {
		t.Errorf("got %q, want exactly %q", got, want)
	}
}

// TestGitCommandError_NonExitError covers a child that never produced an exit
// code (start failure, cancellation): the raw error text stands in.
func TestGitCommandError_NonExitError(t *testing.T) {
	err := gitCommandError("/r", "status", []string{"status"}, context.Canceled, "", "", "")
	if got, want := err.Error(), "git status: context canceled"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestGitCommandError_TruncationNote covers an oversized stream: the quote is
// bounded and the response says so, pointing at how to re-run directly.
func TestGitCommandError_TruncationNote(t *testing.T) {
	big := strings.Repeat("noise line\n", 3000) // ~33 KB, over the 16 KiB cap
	err := gitCommandError("/r", "commit", []string{"commit", "-m", "my message"}, exitError(t), "", big, "")
	msg := err.Error()
	for _, want := range []string{"truncated", "re-run `git commit -m 'my message'` in /r"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want %q in truncated error, got %.200q…", want, msg)
		}
	}
	if len(msg) > maxGitErrStreamBytes+1024 {
		t.Errorf("error should stay bounded, got %d bytes", len(msg))
	}
}

// TestGitCommandError_WarningLeadsMessage covers the Finding-3 fix: a non-empty
// warning (the repo-intent advisory computed before the git child ran) leads
// the failure message, ahead of the exit code line — the one path where
// dropping it would be worst, since the warning may explain the failure.
func TestGitCommandError_WarningLeadsMessage(t *testing.T) {
	warning := "# plumb-warning: peer intent claims cover this repository (advisory, unverified — not a lock):\n" +
		"#   peer blue-heron claimed: \"rebasing ops main\" (expires in 40 min)\n"
	err := gitCommandError("/r", "commit", []string{"commit", "-m", "x"}, exitError(t), "", "", warning)
	msg := err.Error()
	if !strings.HasPrefix(msg, warning) {
		t.Errorf("warning must lead the failure message, got:\n%s", msg)
	}
	if !strings.Contains(msg, "git commit: exit code 1") {
		t.Errorf("exit code line must still follow the warning, got:\n%s", msg)
	}
}

// TestTailGitErrStdout covers the trailing-lines cap: over-long stdout keeps
// its LAST lines and reports truncation.
func TestTailGitErrStdout(t *testing.T) {
	var sb strings.Builder
	for i := range 60 {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	out, truncated := tailGitErrStdout(sb.String())
	if !truncated {
		t.Error("60 lines must report truncation")
	}
	if !strings.HasPrefix(out, "line 20\n") || !strings.HasSuffix(out, "line 59") {
		t.Errorf("want the last 40 lines, got %.80q…", out)
	}
}

// TestGit_CommitFailingHookReportsStreams is the end-to-end regression: a
// pre-commit hook that prints to both streams and exits non-zero must surface
// the exit code and the hook's stderr — never the hook's chatter as the bare
// error message. (git redirects a pre-commit hook's stdout onto its own
// stderr, so both hook lines arrive on the stderr stream here; the separate
// stdout label is pinned by the unit tests above.)
func TestGit_CommitFailingHookReportsStreams(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	hook := "#!/bin/sh\necho 'hook stdout: 0 issues. file-size: OK'\necho 'hook stderr: lint failed' >&2\nexit 1\n"
	hookPath := filepath.Join(dir, ".git", "hooks", "pre-commit")
	if err := os.WriteFile(hookPath, []byte(hook), 0o755); err != nil {
		t.Fatalf("writing hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"f.txt"}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}
	_, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "test commit", "repo": dir})
	if err == nil {
		t.Fatal("a commit blocked by a failing pre-commit hook must error")
	}
	msg := err.Error()
	for _, want := range []string{"exit code 1", "stderr:\n", "hook stderr: lint failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("want %q in error, got %q", want, msg)
		}
	}
	firstLine, _, _ := strings.Cut(msg, "\n")
	if strings.Contains(firstLine, "hook stdout") || strings.Contains(firstLine, "0 issues") {
		t.Errorf("hook output must not be presented as the error, got %q", firstLine)
	}
}
