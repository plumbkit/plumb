//go:build unix

package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// These tests drive plumbkit/plumb#549 end to end through the real tool and a
// real git binary: a pre-commit hook that outlives the call's foreground
// deadline. Before the fix the call simply blocked for the hook's whole run —
// which, against a real MCP client, meant the client gave up with "Request
// timed out" while the commit went on to land unreported.

// testDetachAfter is the shortened foreground deadline injected through
// GitPolicy.DetachAfter; the hooks below sleep well past it.
const testDetachAfter = time.Second

// slowHookRepo creates a repository whose pre-commit hook runs body, with f.txt
// already staged through the tool, and returns the repo, its resolved root,
// the HEAD it starts from, and a tool whose foreground deadline is
// testDetachAfter.
func slowHookRepo(t *testing.T, hookBody string) (dir, root, head0 string, tool *Git) {
	t.Helper()
	requireGit(t)
	dir = initTestRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\n"+hookBody), 0o755); err != nil {
		t.Fatalf("writing hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	tool = NewGit(WriteDeps{}, func() GitPolicy {
		return GitPolicy{AllowWrites: true, WriteTimeout: 30 * time.Second, DetachAfter: testDetachAfter}
	}).WithSession(func() string { return "sess-a" }, func() string { return "alpha" })
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"f.txt"}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}
	var err error
	if root, err = findGitRoot(dir); err != nil {
		t.Fatalf("findGitRoot: %v", err)
	}
	t.Cleanup(func() {
		// Never leave a detached child running into the next test.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = waitGitBackground(ctx, root)
		gitBackgroundOps.Delete(root)
	})
	return dir, root, gitRevParse(t, dir, "HEAD"), tool
}

func gitRevParse(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", rev).Output()
	if err != nil {
		t.Fatalf("git rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

// commitDetached issues the commit and asserts it came back as "still
// running", promptly, rather than blocking for the hook.
func commitDetached(t *testing.T, tool *Git, dir string) string {
	t.Helper()
	var out string
	var err error
	start := time.Now()
	runBounded(t, 25*time.Second, "git commit past the foreground deadline", func() {
		out, err = callGit(t, tool, map[string]any{"subcommand": "commit", "message": "slow hook commit", "repo": dir})
	})
	elapsed := time.Since(start)
	t.Logf("commit result after %s:\n%s", elapsed, out)
	if err != nil {
		t.Fatalf("a commit that outlives the foreground deadline must not be reported as an error (callers retry errors): %v", err)
	}
	if elapsed > testDetachAfter+3*time.Second {
		t.Errorf("the call took %s — it waited for the hook instead of returning at the %s deadline", elapsed, testDetachAfter)
	}
	for _, want := range []string{"STILL RUNNING", "pid:", "started:", "Do NOT retry", "HEAD:"} {
		if !strings.Contains(out, want) {
			t.Errorf("the still-running result must contain %q, got:\n%s", want, out)
		}
	}
	return out
}

func waitBackground(t *testing.T, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitGitBackground(ctx, root); err != nil {
		t.Fatalf("background commit never finished: %v", err)
	}
}

// TestGit_SlowCommitDetachesAndReportsWhereItLanded is the regression test for
// #549: still-running result, writes refused and reads served while in flight,
// then the landed SHA reported on the next call — and exactly one commit made.
func TestGit_SlowCommitDetachesAndReportsWhereItLanded(t *testing.T) {
	dir, root, head0, tool := slowHookRepo(t, "echo 'pre-commit: linting' >&2\nsleep 4\nexit 0\n")

	out := commitDetached(t, tool, dir)
	if !strings.Contains(out, head0[:7]) {
		t.Errorf("the result must name the HEAD the commit started from (%s), got:\n%s", head0[:7], out)
	}
	if got := gitRevParse(t, dir, "HEAD"); got != head0 {
		t.Fatalf("HEAD moved before the hook finished (%s); the test is not exercising the in-flight window", got)
	}

	// A write while in flight is refused — with the explanation, not a lock
	// collision — and nothing is run.
	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}
	_, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"g.txt"}, "repo": dir})
	if err == nil {
		t.Fatal("a write while a background commit is in flight must be refused")
	}
	t.Logf("refusal: %v", err)
	if !strings.Contains(err.Error(), "still running in the background") || !strings.Contains(err.Error(), "Nothing was run") {
		t.Errorf("the refusal must explain the in-flight op, got: %v", err)
	}
	assertClassified(t, err, toolerror.KindDaemonTransport, toolerror.ClassRetryAfterWait, true)
	if staged := gitStaged(t, dir); strings.Contains(staged, "g.txt") {
		t.Errorf("the refused add staged g.txt anyway: %q", staged)
	}

	// A read works while in flight, and says what is running.
	status, err := callGit(t, tool, map[string]any{"subcommand": "status", "args": []string{"--short"}, "repo": dir})
	if err != nil {
		t.Fatalf("a read must keep working while a background commit is in flight: %v", err)
	}
	t.Logf("status while in flight:\n%s", status)
	if !strings.Contains(status, "still running in the background") || !strings.Contains(status, "f.txt") {
		t.Errorf("the read should carry the in-flight note and its own output, got:\n%s", status)
	}

	waitBackground(t, root)
	landed := gitRevParse(t, dir, "HEAD")
	if landed == head0 {
		t.Fatal("the background commit never landed")
	}

	// The next call reports where it landed.
	logOut, err := callGit(t, tool, map[string]any{"subcommand": "log", "args": []string{"--oneline", "-1"}, "repo": dir})
	if err != nil {
		t.Fatalf("git log after the background commit: %v", err)
	}
	t.Logf("next call:\n%s", logOut)
	if !strings.Contains(logOut, "landed as "+landed) || !strings.Contains(logOut, "slow hook commit") {
		t.Errorf("the next call must report the landed SHA %s, got:\n%s", landed, logOut)
	}
	// Once per session: the same session is not told again.
	again, err := callGit(t, tool, map[string]any{"subcommand": "log", "args": []string{"--oneline", "-1"}, "repo": dir})
	if err != nil {
		t.Fatalf("second git log: %v", err)
	}
	if strings.Contains(again, "landed as") {
		t.Errorf("the outcome must be reported once per session, got it again:\n%s", again)
	}
	// Exactly one commit on top of the initial one: nothing double-committed.
	if n := gitRevParse(t, dir, "HEAD~1"); n != head0 {
		t.Errorf("HEAD~1 = %s, want the starting HEAD %s", n, head0)
	}

	// And writes are accepted again once it has finished.
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"g.txt"}, "repo": dir}); err != nil {
		t.Errorf("a write after the background commit finished must run: %v", err)
	}
}

// TestGit_FailedBackgroundCommitIsReported: a hook that fails AFTER the call
// detached is reported as a failure, with the hook's own output, on the next
// call — and HEAD has not moved.
func TestGit_FailedBackgroundCommitIsReported(t *testing.T) {
	dir, root, head0, tool := slowHookRepo(t, "sleep 3\necho 'lint: 3 issues found' >&2\nexit 1\n")

	commitDetached(t, tool, dir)
	waitBackground(t, root)
	if got := gitRevParse(t, dir, "HEAD"); got != head0 {
		t.Fatalf("a failed hook must not move HEAD, got %s", got)
	}

	out, err := callGit(t, tool, map[string]any{"subcommand": "status", "args": []string{"--short"}, "repo": dir})
	if err != nil {
		t.Fatalf("git status after the failed background commit: %v", err)
	}
	t.Logf("next call:\n%s", out)
	for _, want := range []string{"FAILED", "lint: 3 issues found", "git commit"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure report must contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "landed as") {
		t.Errorf("a failed commit must not be reported as landed:\n%s", out)
	}
}

// TestGit_FinishedBackgroundReportedToEachSession: a peer session's call does
// not consume the report the originating session is waiting for.
func TestGit_FinishedBackgroundReportedToEachSession(t *testing.T) {
	dir, root, _, tool := slowHookRepo(t, "sleep 3\nexit 0\n")
	peer := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true} }).
		WithSession(func() string { return "sess-b" }, func() string { return "beta" })

	commitDetached(t, tool, dir)
	waitBackground(t, root)

	for _, g := range []*Git{peer, tool} {
		out, err := callGit(t, g, map[string]any{"subcommand": "log", "args": []string{"-1", "--format=%s"}, "repo": dir})
		if err != nil {
			t.Fatalf("git log: %v", err)
		}
		if !strings.Contains(out, "landed as") {
			t.Errorf("session %q was not told where the background commit landed:\n%s", g.sessionKey(), out)
		}
	}
}

// TestGit_QueuedWriteDoesNotOutliveItsCall covers the write that was already
// queued on the per-repo lock when the slow commit started: it passed the
// in-flight check (nothing was detached yet), so only the lock wait's bound
// stops it outliving its own call and then running unreported. It must give up
// at its deadline and run nothing.
func TestGit_QueuedWriteDoesNotOutliveItsCall(t *testing.T) {
	dir, root, _, tool := slowHookRepo(t, "sleep 4\nexit 0\n")
	if err := os.WriteFile(filepath.Join(dir, "g.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	commitDone := make(chan struct{})
	go func() {
		defer close(commitDone)
		_, _ = callGit(t, tool, map[string]any{"subcommand": "commit", "message": "slow", "repo": dir})
	}()
	// The owner sidecar is stamped once the commit's child has started, i.e.
	// with the per-repo lock held.
	owner := gitOwnerPath(root)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(owner); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow commit never started")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var err error
	start := time.Now()
	runBounded(t, 25*time.Second, "queued git add", func() {
		_, err = callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"g.txt"}, "repo": dir})
	})
	elapsed := time.Since(start)
	<-commitDone
	if err == nil || !strings.Contains(err.Error(), "nothing was run") {
		t.Errorf("a write queued past its deadline must give up and say nothing ran, got err=%v", err)
	}
	if elapsed > testDetachAfter+2*time.Second {
		t.Errorf("the queued write waited %s — it outlived its %s deadline behind the lock", elapsed, testDetachAfter)
	}
	waitBackground(t, root)
	if staged := gitStaged(t, dir); strings.Contains(staged, "g.txt") {
		t.Errorf("the queued add ran after its call had given up: %q", staged)
	}
}

// TestGit_FastCommitIsNotDetached is the control: a hook inside the deadline
// commits in the foreground with the ordinary result, and registers nothing.
func TestGit_FastCommitIsNotDetached(t *testing.T) {
	dir, root, _, tool := slowHookRepo(t, "exit 0\n")
	out, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": "fast hook commit", "repo": dir})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if strings.Contains(out, "STILL RUNNING") || !strings.Contains(out, "fast hook commit") {
		t.Errorf("a commit inside the deadline must report normally, got:\n%s", out)
	}
	if op := gitBackgroundFor(root); op != nil {
		t.Errorf("a foreground commit registered a background op: %+v", op)
	}
}

func TestGitChildSpec_DetachAfter(t *testing.T) {
	tests := []struct {
		name         string
		detach, bond time.Duration
		want         time.Duration
		wantOn       bool
	}{
		{"unset uses the default", 0, 0, defaultGitDetachAfter, true},
		{"negative uses the default", -time.Second, 0, defaultGitDetachAfter, true},
		{"explicit", 5 * time.Second, time.Minute, 5 * time.Second, true},
		{"at the write bound never detaches", time.Minute, time.Minute, time.Minute, false},
		{"beyond the write bound never detaches", 2 * time.Minute, time.Minute, 2 * time.Minute, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, on := (gitChildSpec{DetachAfter: tt.detach, WriteTimeout: tt.bond}).detachAfter()
			if got != tt.want || on != tt.wantOn {
				t.Errorf("detachAfter() = (%s, %v), want (%s, %v)", got, on, tt.want, tt.wantOn)
			}
		})
	}
	if defaultGitDetachAfter >= 60*time.Second {
		t.Errorf("the default %s must sit below the 60s MCP client call timeout it exists to beat", defaultGitDetachAfter)
	}
}

func gitStaged(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", gitNoOptionalLocks, "-C", dir, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("git diff --cached: %v", err)
	}
	return string(out)
}

// TestGit_DetachedOpReRecordsOwnWritesWhenItFinishes: plumb's own working-tree
// rewrite must not be blamed on a peer (#529) even when the op outlives its call
// (#549). A slow pre-rebase hook holds the call past its deadline BEFORE git
// touches init.txt, so a refresh taken when the call returns would find nothing
// changed, and the rewrite that follows would be reported by the next read as
// "a peer or external process" edit. The finisher has to do the re-recording,
// once git has exited and before the outcome is visible.
func TestGit_DetachedOpReRecordsOwnWritesWhenItFinishes(t *testing.T) {
	repo, tracker, _ := ownWritesFixture(t)
	installHook(t, repo, "pre-rebase", "sleep 3\nexit 0\n")
	tool := NewGit(
		WriteDeps{WorkspaceFn: func(context.Context) string { return repo }, Writes: tracker},
		func() GitPolicy {
			return GitPolicy{AllowWrites: true, AllowDestructive: true, WriteTimeout: 30 * time.Second, DetachAfter: testDetachAfter}
		},
	)
	root, err := findGitRoot(repo)
	if err != nil {
		t.Fatalf("findGitRoot: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = waitGitBackground(ctx, root)
		gitBackgroundOps.Delete(root)
	})
	path := filepath.Join(repo, "init.txt")

	var out string
	runBounded(t, 25*time.Second, "git rebase past the foreground deadline", func() {
		out, err = callGit(t, tool, map[string]any{"subcommand": "rebase", "args": []string{"other"}, "confirm": true})
	})
	if err != nil || !strings.Contains(out, "STILL RUNNING") {
		t.Fatalf("the rebase should have detached (err=%v):\n%s", err, out)
	}
	if data, _ := os.ReadFile(path); string(data) == "other's version\n" {
		t.Fatal("git had already rewritten init.txt when the call returned, so this test is not exercising a rewrite after the call")
	}

	waitBackground(t, root)
	if data, _ := os.ReadFile(path); string(data) != "other's version\n" {
		t.Fatalf("the background rebase did not rewrite init.txt (got %q), so this test proves nothing", data)
	}
	if readWarns(t, tracker, path) {
		t.Error("read after plumb's own detached rebase warned of a peer edit")
	}

	// Positive control: a peer's edit after the op is still reported.
	if err := os.WriteFile(path, []byte("peer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	backdate(t, path, time.Hour)
	if !readWarns(t, tracker, path) {
		t.Error("a peer edit after the detached rebase was not reported")
	}
}
