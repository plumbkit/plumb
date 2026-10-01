package tools

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

// git_background.go keeps a slow git write HONEST about its outcome when it
// outlives the caller's MCP call (plumbkit/plumb#549).
//
// An index/ref-mutating git child runs decoupled from request cancellation
// (beginSerialisedGit), so that a disconnect or shutdown mid-commit lets git
// finish and release .git/index.lock instead of being SIGKILLed holding it.
// The cost of that decoupling was a lie: when a slow pre-commit hook outlasted
// the MCP client's call timeout, the client reported "Request timed out" while
// the commit carried on inside the daemon and LANDED a minute later. A caller
// that believed the error retried and either collided on index.lock or, once
// the lock cleared, committed the same change twice; a caller that gave up
// assumed nothing was committed while HEAD had moved.
//
// The fix keeps the decoupling (killing git mid-commit is worse) and bounds
// only how long the CALL waits: after [git] detach_after — kept below the
// usual client call timeout — the call returns a "still running in the
// background" result naming the child's pid, start time and the HEAD it
// started from, and the op is registered here. Until it finishes every further
// non-read git call on that repository is refused with that explanation, reads
// keep working (and carry a note), and once it has finished the next git call
// from each session reports the outcome: landed as <sha>, or failed with git's
// output.

// defaultGitDetachAfter is the compiled default for [git] detach_after. It sits
// well below the 60-second request timeout MCP client SDKs default to, leaving
// room for the response to travel back through the proxy, and in line with
// [collab] max_wait_seconds (55s), which is kept under the same ceiling for the
// same reason.
const defaultGitDetachAfter = 45 * time.Second

// gitBackgroundReportTTL is how long a FINISHED background op's outcome stays
// reportable. It is long enough for the session that started it to come back
// after the timeout, and short enough that a session attaching much later is
// not told about an old commit it never cared about.
const gitBackgroundReportTTL = 30 * time.Minute

// maxGitBackgroundOutcomeBytes bounds the stored outcome text; the tail is
// kept, because that is where git and a failing hook put the reason.
const maxGitBackgroundOutcomeBytes = 4 * 1024

// gitBackgroundOp is one git write that outlived its call.
//
// Concurrency: the identifying fields are set before the op is published and
// never written again. mu guards the outcome fields and seen; done is closed
// exactly once, by finish, after the outcome is stored.
type gitBackgroundOp struct {
	repoRoot   string
	sub        string
	pid        int
	started    time.Time
	headBefore string // "abc1234 (main)", or "" when it could not be read
	detachedAt time.Duration

	done chan struct{}

	mu       sync.Mutex
	finished time.Time
	failed   bool
	outcome  string
	seen     map[string]bool
}

// gitBackgroundOps holds the most recent background op per resolved repository
// root. It is process-global for the same reason repoLocks is: the per-repo
// lock it mirrors is daemon-wide, and a peer session writing to the same
// worktree must see the op another session left running.
var gitBackgroundOps sync.Map // map[string]*gitBackgroundOp

func (op *gitBackgroundOp) running() bool {
	select {
	case <-op.done:
		return false
	default:
		return true
	}
}

// finish records the op's outcome and wakes anyone waiting on done.
func (op *gitBackgroundOp) finish(failed bool, outcome string) {
	op.mu.Lock()
	op.finished = time.Now()
	op.failed = failed
	op.outcome = tailBytes(outcome, maxGitBackgroundOutcomeBytes)
	op.mu.Unlock()
	close(op.done)
}

// describe is the one-line identity of the op, shared by every message.
func (op *gitBackgroundOp) describe() string {
	s := fmt.Sprintf("git %s (pid %d, started %s", op.sub, op.pid, op.started.Format(time.RFC3339))
	if op.headBefore != "" {
		s += ", HEAD was " + op.headBefore
	}
	return s + ")"
}

// registerGitBackground publishes op as the latest background op for its
// repository, replacing any finished record there.
func registerGitBackground(op *gitBackgroundOp) {
	gitBackgroundOps.Store(op.repoRoot, op)
}

// gitBackgroundFor returns the repository's background op, or nil. A finished
// op past its report TTL is evicted on the way.
func gitBackgroundFor(repoRoot string) *gitBackgroundOp {
	v, ok := gitBackgroundOps.Load(repoRoot)
	if !ok {
		return nil
	}
	op := v.(*gitBackgroundOp)
	if op.running() {
		return op
	}
	op.mu.Lock()
	expired := time.Since(op.finished) > gitBackgroundReportTTL
	op.mu.Unlock()
	if expired {
		gitBackgroundOps.CompareAndDelete(repoRoot, op)
		return nil
	}
	return op
}

// sweepGitBackgroundOps evicts finished records past their report TTL; called
// from StartRepoLockSweep's ticker so a repository nobody touches again does
// not keep its record for the daemon's lifetime.
func sweepGitBackgroundOps() {
	gitBackgroundOps.Range(func(key, _ any) bool {
		_ = gitBackgroundFor(key.(string))
		return true
	})
}

// gitBackgroundNotice returns the note to lead this call's successful output
// with, and an ack to call once that output is actually delivered. A running op
// is reported on every call (it is live state); a finished op is reported once
// per session, so the session that started it learns the outcome even when a
// peer's call happened to come first.
func gitBackgroundNotice(repoRoot, sessKey string) (string, func()) {
	op := gitBackgroundFor(repoRoot)
	if op == nil {
		return "", func() {}
	}
	if op.running() {
		return fmt.Sprintf("note: %s is still running in the background after %s; its outcome is not known yet.\n\n",
			op.describe(), time.Since(op.started).Round(time.Second)), func() {}
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.seen[sessKey] {
		return "", func() {}
	}
	verdict := "finished"
	if op.failed {
		verdict = "FAILED"
	}
	note := fmt.Sprintf("note: background %s %s %s ago (it ran for %s):\n%s\n\n",
		op.describe(), verdict, time.Since(op.finished).Round(time.Second),
		op.finished.Sub(op.started).Round(time.Second), indentLines(op.outcome, "  "))
	return note, func() {
		op.mu.Lock()
		defer op.mu.Unlock()
		if op.seen == nil {
			op.seen = make(map[string]bool)
		}
		op.seen[sessKey] = true
	}
}

// refuseWhileGitBackground refuses a non-read git op on a repository whose
// previous write is still running in the background. Queueing it instead would
// wait on the per-repo lock for as long as the background child runs, which
// outlives this call too and reproduces the very lie #549 is about.
func refuseWhileGitBackground(repoRoot, sub string) error {
	op := gitBackgroundFor(repoRoot)
	if op == nil || !op.running() {
		return nil
	}
	return toolerror.Wrap(fmt.Errorf(
		"git %s: refused — %s is still running in the background on this repository (%s so far), "+
			"so this write would only queue behind it. Nothing was run. "+
			"Reads (status, log, diff) still work; call git log -1 or status to follow it, and the next git call here reports whether it landed or failed",
		sub, op.describe(), time.Since(op.started).Round(time.Second)),
		toolerror.KindDaemonTransport, toolerror.ClassRetryAfterWait)
}

// gitStillRunningMessage is the result of a call whose git child outlived the
// foreground deadline. It is a success result, not an error, on purpose: the
// operation has not failed, and an error is exactly what makes a caller retry.
func gitStillRunningMessage(op *gitBackgroundOp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "git %s is STILL RUNNING in the background — it has not failed, and it has not finished.\n", op.sub)
	fmt.Fprintf(&b, "It outlived plumb's %s foreground deadline ([git] detach_after, kept below the MCP client's call timeout); "+
		"the usual cause is a slow hook.\n", op.detachedAt.Round(time.Millisecond))
	fmt.Fprintf(&b, "  pid:     %d\n  started: %s\n", op.pid, op.started.Format(time.RFC3339))
	if op.headBefore != "" {
		fmt.Fprintf(&b, "  HEAD:    %s when it started", op.headBefore)
		if op.sub == "commit" {
			b.WriteString(" — HEAD moves to the new commit when it lands")
		}
		b.WriteString("\n")
	}
	b.WriteString("Do NOT retry: a retry would collide on .git/index.lock or, once the lock clears, repeat the operation " +
		"(a second commit of the same change). Further git writes on this repository are refused until it finishes; " +
		"reads (status, log, diff) still work. The next git call here reports the outcome — landed as <sha>, or failed with git's output.")
	return b.String()
}

// awaitOrDetach waits for the started child until the call's foreground
// deadline. A child that finishes in time returns its run error and a nil op.
// A child still running at the deadline is NOT killed — killing git mid-commit
// strands index.lock and a half-written index — but registered as a background
// op and left to a finisher goroutine, and the op is returned.
func (r *gitChildRun) awaitOrDetach(cmd *exec.Cmd, wait func() error) (*gitBackgroundOp, error) {
	if r.detachAfter <= 0 {
		return nil, wait()
	}
	done := make(chan error, 1)
	go func() { done <- wait() }()
	timer := time.NewTimer(time.Until(r.start.Add(r.detachAfter)))
	defer timer.Stop()
	select {
	case err := <-done:
		return nil, err
	case <-timer.C:
	}
	select {
	case err := <-done: // finished at the very deadline: report it in the foreground
		return nil, err
	default:
	}
	op := &gitBackgroundOp{
		repoRoot:   r.repoRoot,
		sub:        r.sub,
		pid:        cmd.Process.Pid,
		started:    r.start,
		headBefore: gitHeadSummary(r.execCtx, r.repoRoot),
		detachedAt: r.detachAfter,
		done:       make(chan struct{}),
	}
	registerGitBackground(op)
	go r.finishInBackground(op, done)
	return op, nil
}

// finishInBackground waits out a detached child, records its outcome on op the
// same way a foreground call would have reported it, and only then releases
// the per-repo lock and drain token (cleanup): the op is marked finished first,
// so a write arriving in between queues briefly rather than being refused.
//
// The own-writes refresh (afterExit) runs here, on a failure too, because the
// call that started the child returned before git changed any file: re-recording
// at that point would be a no-op, and the next read_file would blame a peer for
// this operation. It runs before the op is marked finished, so a caller told the
// outcome never reads a file that is still unrecorded.
func (r *gitChildRun) finishInBackground(op *gitBackgroundOp, done <-chan error) {
	defer r.cleanup()
	err := <-done
	r.afterExit()
	if err != nil {
		op.finish(true, r.failure(err).Error())
		return
	}
	r.guard.postExec(r.execCtx)
	op.finish(false, gitBackgroundOutcome(r.execCtx, r.repoRoot, r.sub, r.output()))
}

// gitHeadSummary returns "abc1234 (branch)" for repoRoot's HEAD, or "" on any
// failure. Read-only and lock-free (--no-optional-locks).
func gitHeadSummary(ctx context.Context, repoRoot string) string {
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "rev-parse", "HEAD", "--abbrev-ref", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return ""
	}
	sha := fields[0]
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return fmt.Sprintf("%s (%s)", sha, fields[1])
}

// gitBackgroundOutcome renders a SUCCESSFUL background op's outcome from the
// same material a foreground call would have returned; a failure is rendered by
// gitChildRun.failure, exactly as in the foreground.
func gitBackgroundOutcome(ctx context.Context, repoRoot, sub, out string) string {
	if sub == "commit" {
		if res, err := resolveCommitInfo(ctx, repoRoot); err == nil {
			return "landed as " + res.Hash + " " + res.Subject
		}
	}
	processed, err := postProcessGit(ctx, repoRoot, sub, out)
	if err != nil {
		return "completed"
	}
	return "completed: " + processed
}

// waitGitBackground blocks until repoRoot's background op (if any) finishes
// or ctx ends. For tests and for callers that must not race a detached child.
func waitGitBackground(ctx context.Context, repoRoot string) error {
	v, ok := gitBackgroundOps.Load(repoRoot)
	if !ok {
		return nil
	}
	select {
	case <-v.(*gitBackgroundOp).done:
		return nil
	case <-ctx.Done():
		return errors.New("background git op still running: " + ctx.Err().Error())
	}
}

func tailBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return "…" + s[len(s)-limit:]
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
