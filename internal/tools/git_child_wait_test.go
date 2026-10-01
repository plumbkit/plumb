//go:build unix

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runBounded runs fn and fails the test if it has not returned within limit.
// Every test in this file exists because the code under test could block
// forever; without an explicit bound a regression would hang CI until the
// harness timeout instead of failing fast with a useful message.
func runBounded(t *testing.T, limit time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s did not return within %s — cmd.Wait is parked on output pipes a descendant still holds", what, limit)
	}
}

// TestExecGitCmd_BoundsWaitWhenDescendantHoldsPipes is the regression for the
// unbounded-Wait hazard. cmd.Stdout/Stderr are bytes.Buffers, so os/exec gives
// the child an os.Pipe and copies from it in a goroutine; cmd.Wait waits for
// that copy to hit EOF, which never comes while ANY descendant holds the write
// end. Here the child exits immediately and a backgrounded grandchild holds the
// pipe for 30s.
//
// Without boundGitChildWait's WaitDelay, execGitCmd parks for the full 30s —
// and in production it parks holding the per-repo lock and a gitWriteInflight
// token, wedging every later non-read git op on that repository from every
// session. The context deadline does not help: it kills the direct child, which
// is not what holds the pipe.
func TestExecGitCmd_BoundsWaitWhenDescendantHoldsPipes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	// The child exits at once; the grandchild inherits stdout and lives 30s.
	cmd := exec.CommandContext(context.Background(), "sh", "-c", "sleep 30 & echo started; exit 0")
	cmd.Dir = t.TempDir()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	var err error
	start := time.Now()
	runBounded(t, 12*time.Second, "execGitCmd", func() { err = execGitCmd(cmd, false, cmd.Dir) })
	elapsed := time.Since(start)

	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Errorf("want exec.ErrWaitDelay once the delay expires, got %v", err)
	}
	if elapsed < gitChildWaitDelay {
		t.Errorf("returned after %s, before the %s delay — the bound is not what released it", elapsed, gitChildWaitDelay)
	}
	// The output copied before the pipes were closed is still captured.
	if !strings.Contains(stdout.String(), "started") {
		t.Errorf("output captured before the delay expired must survive, got %q", stdout.String())
	}
}

// TestExecGitCmd_KillsProcessGroupOnCancel proves cancellation reaches the git
// child's DESCENDANTS, not just the child. Mirrors
// TestRunArgv_KillsProcessGroupOnTimeout, because git_exec.go now uses the same
// hygiene helpers as cmdexec.go: the shell backgrounds a grandchild that would
// create a marker after 1s, then blocks. With the process group in place the
// whole group dies on cancel and the marker never appears; with only the direct
// child killed the grandchild survives to write it.
func TestExecGitCmd_KillsProcessGroupOnCancel(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-ran")
	script := "( sleep 1; touch " + marker + " ) & sleep 30"

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Dir = dir
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runBounded(t, 20*time.Second, "execGitCmd", func() { _ = execGitCmd(cmd, false, dir) })

	// Wait past the grandchild's 1s mark; if the group was killed it never runs.
	time.Sleep(2 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a descendant of the git child survived cancellation — the process group was not killed")
	}
}

// TestGit_CommitWithBackgroundingHookStaysBounded is the end-to-end half,
// through the real tool and a real git binary, in the shape that actually
// reaches users: a pre-commit hook that backgrounds a process without
// redirecting its output. git itself finishes and exits 0, but the backgrounded
// process holds git's stdout, so before this fix the call never returned — and
// the commit tier holds the per-repo lock for the whole of it.
//
// The reply is an error (the operation is reported as unfinished rather than
// silently succeeding), and it must explain WHAT happened: exec's bare
// "WaitDelay expired before I/O complete" tells a caller nothing about the hook.
func TestGit_CommitWithBackgroundingHookStaysBounded(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	hook := "#!/bin/sh\nsleep 30 &\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatalf("writing hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} })
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"f.txt"}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}

	var err error
	runBounded(t, 15*time.Second, "git commit with a backgrounding hook", func() {
		raw, _ := json.Marshal(map[string]any{"subcommand": "commit", "message": "bg hook", "repo": dir})
		_, err = tool.Execute(context.Background(), raw)
	})

	if err == nil {
		t.Fatal("want an error naming the abandoned wait")
	}
	for _, want := range []string{"holding its output pipes", "check the repository state"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must explain the abandoned wait; want %q in %q", want, err.Error())
		}
	}
	if strings.Contains(err.Error(), "WaitDelay expired") {
		t.Errorf("exec's raw message is not actionable for a caller, got %q", err.Error())
	}
}
