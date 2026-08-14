//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// TestGit_WriteTimeoutIsReportedAsPlumbs is the end-to-end shape of the
// misattribution this change exists to fix, driven through the real tool and a
// real git binary: a pre-commit hook that outlasts [git] write_timeout.
//
// plumb kills the child's process group at the bound, and a signalled child
// yields ExitCode() == -1 — so the reply used to be `git commit: exit code -1`
// under a remediation stating that git declined the operation and that "plumb
// raised no objection, so no plumb setting or flag changes the outcome". Both
// clauses are false here, and in the expensive direction: the reader goes
// looking for a defect in a change that has none.
//
// Bounded with runBounded so a regression that restores the old unbounded or
// mis-sized behaviour fails in seconds rather than hanging CI.
func TestGit_WriteTimeoutIsReportedAsPlumbs(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	// A hook that simply outlives the bound — the multi-agent case in miniature,
	// where the hook is waiting on a peer's golangci-lint rather than sleeping.
	hook := "#!/bin/sh\necho 'pre-commit: waiting on the shared lint cache'\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatalf("writing hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	// 1.5s, not something smaller: git takes roughly a second to reach the hook
	// at all, and a bound that expires first would still prove the attribution
	// while quietly dropping the hook-output half of the assertion below.
	tool := NewGit(WriteDeps{}, func() GitPolicy {
		return GitPolicy{AllowWrites: true, WriteTimeout: 1500 * time.Millisecond}
	})
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"f.txt"}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}

	var err error
	runBounded(t, 25*time.Second, "git commit past the write_timeout", func() {
		raw, _ := json.Marshal(map[string]any{"subcommand": "commit", "message": "slow hook", "repo": dir})
		_, err = tool.Execute(context.Background(), raw)
	})

	if err == nil {
		t.Fatal("want an error naming plumb's own bound")
	}
	msg := err.Error()
	for _, want := range []string{"plumb stopped waiting", "1.5s", "waiting on the shared lint cache"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must name plumb, the bound, and the hook's output; want %q in %q", want, msg)
		}
	}
	if strings.Contains(msg, "exit code -1") {
		t.Errorf("a killed child reported as a git exit code is the defect itself: %q", msg)
	}
	assertClassified(t, err, toolerror.KindClientTimeout, toolerror.ClassRetryAfterWait, true)
	if reason := mustClassify(t, err).Remediation.Reason; !strings.Contains(reason, "write_timeout") {
		t.Errorf("the remediation must name the setting that moves the bound, got %q", reason)
	}
}

// TestGit_CommitWithinTheBoundStillSucceeds is the control. Everything above
// would also pass if plumb had simply started killing every commit, so the
// bound must be shown to LET a hook of realistic length finish — and to report
// that as an ordinary success, not as a timeout.
func TestGit_CommitWithinTheBoundStillSucceeds(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	hook := "#!/bin/sh\nsleep 0.2\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatalf("writing hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	tool := NewGit(WriteDeps{}, func() GitPolicy {
		return GitPolicy{AllowWrites: true, WriteTimeout: 20 * time.Second}
	})
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"f.txt"}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}

	var out string
	var err error
	runBounded(t, 25*time.Second, "git commit inside the write_timeout", func() {
		raw, _ := json.Marshal(map[string]any{"subcommand": "commit", "message": "quick hook", "repo": dir})
		out, err = tool.Execute(context.Background(), raw)
	})
	if err != nil {
		t.Fatalf("a hook well inside the bound must still commit: %v", err)
	}
	if !strings.Contains(out, "quick hook") {
		t.Errorf("expected the commit summary, got %q", out)
	}
}

// TestGit_NetworkTierExpiredContextIsNotAWriteTimeout is the OTHER direction
// from TestGit_WriteTimeoutIsReportedAsPlumbs above: a NETWORK-tier op
// (push/fetch/pull) never gets the write/destructive tiers' cancellation
// decoupling — beginSerialisedGit leaves execCtx == the caller's own ctx for
// tierNetwork — so a caller-cancelled or caller-deadlined context expiring
// mid-fetch is git_exec.go's runGit detecting context.DeadlineExceeded on
// execCtx for a call that was NEVER bounded by plumb's own write_timeout.
// Reporting that as "plumb stopped waiting after Nm0s and killed the git
// child" — the write-tier message — would be exactly the same misattribution
// this PR exists to remove, just pointing the other way: the caller's own
// deadline, dressed up as plumb's bound.
//
// runGit's `mutating := tier == tierWrite || tier == tierDestructive` guard is
// what keeps the two apart (git_exec.go, the `if mutating &&
// errors.Is(execCtx.Err(), context.DeadlineExceeded)` branch). Dropping
// `mutating &&` — verified surviving mutant M19 — makes this test fail: with
// mutating unchecked, a network-tier op whose OWN context happens to be
// DeadlineExceeded (as it always is, right after being killed for exactly
// that reason) would take the write-timeout branch too.
//
// The git child here is `git fetch` against an ssh:// URL with
// GIT_SSH_COMMAND pointed at a stub that sleeps well past the context's
// deadline — deterministic and network-free (nothing is actually dialed;
// GIT_SSH_COMMAND replaces the ssh binary git would otherwise exec).
func TestGit_NetworkTierExpiredContextIsNotAWriteTimeout(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)

	sshStub := filepath.Join(t.TempDir(), "slow-ssh.sh")
	if err := os.WriteFile(sshStub, []byte("#!/bin/sh\nsleep 10\n"), 0o755); err != nil {
		t.Fatalf("writing ssh stub: %v", err)
	}
	child := gitChildSpec{Env: append(os.Environ(), "GIT_SSH_COMMAND="+sshStub), WriteTimeout: 20 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	var err error
	runBounded(t, 25*time.Second, "git fetch past the caller's own deadline", func() {
		_, err = runGit(ctx, dir, "fetch", []string{"fetch", "ssh://127.0.0.1/nonexistent.git"}, tierNetwork, nil, nil, child)
	})

	if err == nil {
		t.Fatal("want an error: the caller's own context expired mid-fetch")
	}
	msg := err.Error()
	if strings.Contains(msg, "plumb stopped waiting") {
		t.Errorf("a network-tier op's own context expiring must not be reported as plumb's write_timeout bound: %q", msg)
	}
	// The real (non-mutant) classification: git's own child failed, reported
	// through gitCommandError — never gitWriteTimeoutError's KindClientTimeout.
	assertClassified(t, err, toolerror.KindGitCommandFailed, toolerror.ClassInspectOutput, false)
}
