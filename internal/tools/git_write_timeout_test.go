//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
// It asserts what plumb does, not how fast: the bound is reported as plumb's,
// git's whole process group is stopped, and the repository is left as it was —
// no lock, no moved ref, the staged change still staged. The only time it reads
// is that the call took AT LEAST the bound, which load cannot falsify. It once
// bounded the wait at 1.5s and needed git to reach the hook inside it, which a
// loaded machine does not do (#575).
//
// The hook signals once it is running, and the bound is doubled until it has.
// That, rather than a bound large enough to be safe on any machine, is what keeps
// the half of the assertion that quotes the hook's output deterministic: the
// bound has to expire after the hook started for there to be output to quote.
func TestGit_WriteTimeoutIsReportedAsPlumbs(t *testing.T) {
	t.Parallel()
	requireGit(t)
	dir := initTestRepo(t)
	head0 := gitRevParse(t, dir, "HEAD")

	// The hook is the multi-agent case in miniature, where it is waiting on a
	// peer's golangci-lint rather than sleeping. It prints, starts a sleeper, and
	// only then publishes both pids — atomically, by rename, from outside the
	// repository — so a pid file that exists proves the output was already
	// written, and gives the test the processes to look for afterwards.
	pids := filepath.Join(t.TempDir(), "hook.pids")
	hook := "#!/bin/sh\n" +
		"echo 'pre-commit: waiting on the shared lint cache'\n" +
		"sleep 60 &\n" +
		"echo \"$$ $!\" > " + shellQuote(pids+".tmp") + " && mv " + shellQuote(pids+".tmp") + " " + shellQuote(pids) + "\n" +
		"wait\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil { //nolint:gosec // G306: a hook must be executable
		t.Fatalf("writing hook: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	bound := 1500 * time.Millisecond
	tool := NewGit(WriteDeps{}, func() GitPolicy { return GitPolicy{AllowWrites: true, WriteTimeout: bound} })
	if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{"f.txt"}, "repo": dir}); err != nil {
		t.Fatalf("git add: %v", err)
	}

	const attempts = 5 // 1.5s doubling to 24s
	var (
		err     error
		elapsed time.Duration
		hookPID []int
	)
	for attempt := 1; ; attempt++ {
		_ = os.Remove(pids)
		begin := time.Now()
		runBounded(t, bound+25*time.Second, "git commit past the write_timeout", func() {
			raw, _ := json.Marshal(map[string]any{"subcommand": "commit", "message": "slow hook", "repo": dir})
			_, err = tool.Execute(context.Background(), raw)
		})
		elapsed = time.Since(begin)
		if hookPID = readHookPIDs(pids); hookPID != nil {
			break
		}
		// The bound expired before git reached the hook, which says something about
		// the machine and nothing about plumb. Wait longer; never assert on it.
		if attempt == attempts {
			t.Fatalf("git never reached the pre-commit hook within %s: the machine is too slow to run this test", bound)
		}
		t.Logf("attempt %d: the hook had not started within %s; doubling the bound", attempt, bound)
		bound *= 2
	}

	if err == nil {
		t.Fatal("want an error naming plumb's own bound")
	}
	msg := err.Error()
	for _, want := range []string{"plumb stopped waiting after " + bound.String(), "waiting on the shared lint cache"} {
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
	// A floor, not a ceiling: the call cannot report plumb's bound before the bound.
	if elapsed < bound {
		t.Errorf("the call returned after %s, before the %s bound it reports", elapsed, bound)
	}

	// Git is stopped: the hook and what it started, not just git itself.
	for _, pid := range hookPID {
		waitProcessStopped(t, pid)
	}

	// The repository is as it was: nothing half-written.
	if got := gitRevParse(t, dir, "HEAD"); got != head0 {
		t.Errorf("HEAD moved to %s although the hook was killed before the commit", got)
	}
	if got := gwGit(t, dir, "diff", "--cached", "--name-only"); got != "f.txt" {
		t.Errorf("the staged change must survive the killed commit untouched, got %q", got)
	}
	if locks := lockFilesUnder(t, filepath.Join(dir, ".git")); len(locks) != 0 {
		t.Errorf("a killed commit left lock files behind: %v", locks)
	}
}

// readHookPIDs returns the pids the hook published, or nil while it has not.
func readHookPIDs(path string) []int {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitProcessStopped fails unless pid has gone. Death after SIGKILL is
// asynchronous, so it polls, with far more slack than a prompt kill needs: a
// process that is still alive at the end is a failure on any machine, and one
// that is merely slow to go is not. The deadline must stay well short of how long
// the hook would live unkilled (60s), or a plumb that never killed it would pass by
// waiting for it to finish. A zombie is a stopped process whose parent
// has not collected it yet, so it counts as gone.
func waitProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if stat := strings.TrimSpace(string(out)); stat == "" || strings.HasPrefix(stat, "Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("process %d is still running: plumb reported its bound but did not stop git's process group", pid)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lockFilesUnder lists every *.lock file below dir: an index.lock, a HEAD.lock, a
// ref's lock — whatever a git killed mid-write could strand.
func lockFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var locks []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".lock") {
			locks = append(locks, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return locks
}

// TestGit_CommitWithinTheBoundStillSucceeds is the control. Everything above
// would also pass if plumb had simply started killing every commit, so the
// bound must be shown to LET a hook of realistic length finish — and to report
// that as an ordinary success, not as a timeout.
func TestGit_CommitWithinTheBoundStillSucceeds(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		_, err = runGit(ctx, dir, "fetch", []string{"fetch", "ssh://127.0.0.1/nonexistent.git"}, tierNetwork, nil, nil, child, nil, "")
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
