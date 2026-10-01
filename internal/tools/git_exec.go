package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/toolerror"
)

const maxGitBytes = 100 * 1024 // 100 KiB

// buildGitArgv assembles the full git argv. add and commit use typed params so
// free-form path args and commit footguns (-F, editor, --no-verify, --amend)
// are unreachable; all other subcommands pass args through. trailer, when
// non-empty, is stamped on a commit via --trailer (the [git] commit_trailer
// session-attribution knob); it is inserted BEFORE the `--` pathspec
// separator so a path-limited commit keeps working.
func buildGitArgv(a gitToolArgs, trailer string) ([]string, error) {
	switch a.Subcommand {
	case "commit":
		if strings.TrimSpace(a.Message) == "" {
			return nil, errors.New("git commit: message is required")
		}
		argv := []string{"commit", "-m", a.Message}
		if trailer != "" {
			argv = append(argv, "--trailer", trailer)
		}
		// Path-limited commit: `git commit -m <msg> -- <files>` commits ONLY the
		// named paths, ignoring unrelated staged changes in the index — the
		// multi-agent / shared-worktree workflow agents asked for repeatedly.
		if len(a.Files) > 0 {
			argv = append(argv, "--")
			argv = append(argv, a.Files...)
		}
		return argv, nil
	case "add":
		if len(a.Files) == 0 {
			return nil, errors.New("git add: at least one path is required (use the files parameter)")
		}
		return append([]string{"add", "-A", "--"}, a.Files...), nil
	default:
		return append([]string{a.Subcommand}, a.Args...), nil
	}
}

// gitReadArgv prefixes a READ-ONLY git argv with --no-optional-locks.
//
// `git status` and `git diff` refresh the index as a side effect of running,
// and that refresh is a WRITE: it takes .git/index.lock and rewrites
// .git/index. Every one of plumb's read-only git queries runs under
// exec.CommandContext, whose default cancellation is SIGKILL — which git
// cannot trap, so it never gets to remove the lock. A daemon shutdown, a
// connection eviction, or any cancelled tool call mid-query therefore strands
// an index.lock, and the next `git add` in that repo fails with "Unable to
// create index.lock: File exists" and the misleading advice that another git
// process is running. (Observed twice in one session, both times with no git
// process alive and a zero-byte lock file.)
//
// --no-optional-locks tells git to skip any operation needing a lock. The
// refresh it skips is purely a stat-cache optimisation — the command's output
// is identical either way — so this is the correct flag for every query plumb
// makes, and it removes the failure mode by construction rather than by
// cleaning up after it.
//
// Mutating commands must NOT use it: they need the lock.
//
// Use the constant directly when the argv is a literal list, and the helper
// when it is a slice built at runtime — gosec cannot see through the call, so
// the literal form keeps the argv inspectable and avoids a //nolint.
const gitNoOptionalLocks = "--no-optional-locks"

func gitReadArgv(argv []string) []string {
	return append([]string{gitNoOptionalLocks}, argv...)
}

// gitArgvForTier applies gitReadArgv to a read-tier argv and leaves every
// other tier's untouched.
func gitArgvForTier(argv []string, tier gitTier) []string {
	if tier == tierRead {
		return gitReadArgv(argv)
	}
	return argv
}

// runGit runs a git subcommand in the repository containing repo. Non-read tiers
// (index/ref-mutating + network) are serialised per repo so concurrent
// plumb-initiated writes queue rather than collide on .git/index.lock; read-tier
// ops never lock — which is true only because they run with
// --no-optional-locks (see gitReadArgv). Without it `git status`/`git diff`
// refresh the index, and that refresh takes the very lock this comment claims
// reads never touch. For the index/ref-mutating tiers the git child also runs under
// a cancellation-decoupled, bounded context (see beginSerialisedGit) so a daemon
// shutdown or connection eviction mid-commit lets git finish and release the
// lock rather than SIGKILLing it and stranding the lock.
//
// guard (may be nil) is the cross-session ref-movement guard
// (git_ref_guard.go): its preExec check runs here, after the per-repo lock is
// held for the mutating tiers, so a peer's in-flight commit cannot slip
// between the check and this operation; postExec records the session's post-op
// HEAD/branch observation after a successful run. Both hooks are nil-safe, so
// the guardless path adds no branching here.
//
// intentWarn (may be nil) is the repo-level peer-intent check
// (git_intent_warn.go): non-nil only for repo-state verbs with the warning
// wired, it runs right after the guard's pre-execution check — a refused op
// never warns — and its advisory block leads the successful response.
//
// child carries how the git child is RUN (git_child.go): its environment, built
// from [git] env, and the [git] write_timeout bound. A nil Env means inherit
// the daemon's environment, which is what an unconfigured knob resolves to and
// what every git child got before it existed — except that startGitCmd may add
// GOWORK=off, never over a GOWORK already set (git_gowork.go). This is the ONE git child plumb
// spawns that runs the repository's hooks or can open an editor, so it is the
// one whose environment is configurable; the auxiliary read queries around it
// (ls-files, log -1, rev-parse, diff --cached) are plumbing whose output plumb
// parses, and are deliberately left inheriting.
//
// writes (may be nil) is the calling session's WriteTracker. When this op can
// rewrite the working tree, the files the session wrote that git then changes
// are re-recorded afterwards (git_own_writes.go), so the next read does not blame a
// peer for plumb's own switch, merge or restore. A child that outlives the call
// (git_background.go) is re-recorded by its finisher when it exits, not when the
// call returns.
//
// sessKey names the calling session for the once-per-session report of a
// background op's outcome (git_background.go).
func runGit(ctx context.Context, repo, sub string, argv []string, tier gitTier, guard *gitRefGuard, intentWarn func(context.Context, string) string, child gitChildSpec, writes *WriteTracker, sessKey string) (string, error) {
	repoRoot, err := findGitRoot(repo)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}
	// A write an earlier call left running in the background (#549,
	// git_background.go) is reported before anything else, and blocks every
	// further non-read op on the repository until it finishes: queued behind the
	// per-repo lock, such an op would outlive its own call exactly as the first
	// one did. Reads carry the note and run.
	notice, ack := gitBackgroundNotice(repoRoot, sessKey)
	if tier != tierRead {
		if err := refuseWhileGitBackground(repoRoot, sub); err != nil {
			return "", err
		}
	}
	out, err := runGitIn(ctx, repoRoot, sub, argv, tier, guard, intentWarn, child, writes)
	if err != nil {
		return "", err
	}
	ack()
	return notice + out, nil
}

// runGitIn is runGit once the repository is resolved and clear of a background
// op: it serialises a non-read tier, runs the pre-execution guards, and hands
// the child to gitChildRun.
//
// For the index/ref-mutating tiers the whole call — the wait for the per-repo
// lock included — is bounded by [git] detach_after, measured from here. The
// lock wait is cut to that bound because a call queued behind a slow holder
// would otherwise outlive its client just as the holder did, and then commit
// after the client had been told it timed out. A wait cut short runs nothing
// and says so; a child still running at the bound is detached, never killed.
func runGitIn(ctx context.Context, repoRoot, sub string, argv []string, tier gitTier, guard *gitRefGuard, intentWarn func(context.Context, string) string, child gitChildSpec, writes *WriteTracker) (string, error) {
	r := &gitChildRun{ctx: ctx, execCtx: ctx, repoRoot: repoRoot, sub: sub, argv: argv, tier: tier, child: child, guard: guard, start: time.Now(), cleanup: func() {}, afterExit: func() {}}
	lockWait := child.writeTimeout()
	if d, ok := child.detachAfter(); ok && r.mutating() {
		r.detachAfter = d
		lockWait = d
	}
	if tier != tierRead {
		var err error
		r.execCtx, r.cleanup, err = beginSerialisedGit(ctx, repoRoot, sub, tier, child.writeTimeout(), lockWait)
		if err != nil {
			return "", err
		}
	}
	// Deferred, not called inline, so a panic below cannot strand the per-repo
	// lock; skipped only once the finisher has taken ownership of it.
	detached := false
	defer func() {
		if !detached {
			r.cleanup()
		}
	}()
	if err := guardRefPreExec(r.execCtx, guard, repoRoot, sub); err != nil {
		return "", err
	}
	// Taken under the per-repo lock and refreshed on EVERY exit below: a merge
	// that stops on conflicts fails and has still rewritten files. A child that
	// is detached instead is refreshed by its finisher once it has exited (the
	// files change while it runs, not when this call returns), still under the
	// lock.
	r.afterExit = trackOwnGitWrites(writes, repoRoot, sub, tier)
	defer func() {
		if !detached {
			r.afterExit()
		}
	}()
	if intentWarn != nil {
		r.warning = intentWarn(r.execCtx, repoRoot)
	}
	argv = gitArgvForTier(argv, tier)
	out, op, err := r.exec(argv)
	if op != nil {
		// Detached: the background finisher owns cleanup, and with it the
		// per-repo lock and the drain token, until the child exits.
		detached = true
		return gitStillRunningMessage(op), nil
	}
	return out, err
}

// beginSerialisedGit prepares a non-read git op: it refuses new work while the
// daemon is draining for shutdown, registers the op as in-flight, takes the
// per-repo lock, and — for index/ref-mutating tiers — reaps any attributable
// stale lock left by a dead daemon and returns a cancellation-decoupled, bounded
// exec context so a shutdown mid-commit lets git finish. (The owner sidecar is
// stamped by startGitCmd once the child pid is known.) The returned cleanup
// closure (which the caller runs once the child is done) reverses all of it.
// Network tiers serialise and drain-gate but keep request-context cancellation
// (a push can hang on auth — it must stay interruptible) and write no owner
// sidecar (they do not create index.lock).
//
// writeTimeout is the resolved [git] write_timeout and bounds the exec context
// for a mutating tier. lockWait bounds how long this call queues for the
// per-repository lock. It is write_timeout unless the call has a shorter
// foreground deadline ([git] detach_after, see runGitIn). Deriving it from
// write_timeout rather than a shorter constant of its own is deliberate — when
// the lock wait was its own shorter constant, a legitimate holder taking longer
// than that constant made every queued peer fail with "another git operation is
// in progress" for a wait that was always going to be satisfiable. A peer should
// be willing to wait exactly as long as a holder is permitted to run, or as long
// as its own caller will wait, whichever is shorter.
func beginSerialisedGit(ctx context.Context, repoRoot, sub string, tier gitTier, writeTimeout, lockWait time.Duration) (context.Context, func(), error) {
	if gitWriteDrainActive() {
		return nil, nil, toolerror.Wrap(fmt.Errorf("git %s: %w", sub, errGitDraining),
			toolerror.KindDaemonTransport, toolerror.ClassRetryAfterWait)
	}
	gitWriteInflight.Add(1)
	release, err := lockRepo(ctx, repoRoot, lockWait)
	if err != nil {
		gitWriteInflight.Done()
		// Classified on the same terms as the drain refusal above, and for the
		// same reason: the queue in front of this call is plumb's own, git was
		// never reached, and the remedy is to wait. Left bare this arrived as an
		// unclassified internal error — no kind, no remediation — so "another git
		// operation is in progress … timed out" reached the caller with no hint
		// that retrying is exactly the right move.
		return nil, nil, toolerror.Wrap(fmt.Errorf("git %s: %w; nothing was run", sub, err),
			toolerror.KindDaemonTransport, toolerror.ClassRetryAfterWait)
	}
	execCtx := ctx
	cancel := func() {}
	if tier == tierWrite || tier == tierDestructive {
		reapStaleGitLock(repoRoot)
		execCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	}
	cleanup := func() {
		cancel()
		release()
		gitWriteInflight.Done()
	}
	return execCtx, cleanup, nil
}

// startGitCmd applies the child-wait bound (boundGitChildWait — see the
// unbounded-Wait hazard documented there), starts cmd, and stamps the owner
// sidecar with the git child's pid for a mutating op (so a stranded index.lock
// is attributable to the actual lock holder, not the daemon). It returns the
// wait half separately so a caller can stop waiting without killing the child
// (awaitOrDetach); wait clears the sidecar once the child has exited.
//
// The hygiene is applied here rather than at the callsite so every git child
// that goes through this chokepoint gets it, and so a test can exercise the
// real function rather than a reconstruction of it.
//
// The child's GOWORK is decided here for the same reason (applyAutoGoWork): a
// worktree under a go.work that lists another checkout of its module would fail
// every go command its hooks run, and this is the one place every hook-running
// git child — commit, rebase, cherry-pick, push — passes through. goWorkOff is
// the go.work that was switched off ("" when none was), so a failure can say so.
func startGitCmd(cmd *exec.Cmd, mutating bool, repoRoot string) (goWorkOff string, wait func() error, err error) {
	boundGitChildWait(cmd)
	goWorkOff = applyAutoGoWork(cmd)
	pinChildPWD(cmd)
	if err := cmd.Start(); err != nil {
		return goWorkOff, nil, err
	}
	if mutating {
		recordGitLockOwner(repoRoot, cmd.Process.Pid)
	}
	return goWorkOff, func() error {
		if mutating {
			defer clearGitLockOwner(repoRoot)
		}
		return cmd.Wait()
	}, nil
}

// execGitCmd is startGitCmd followed by the wait: the run-to-completion form.
func execGitCmd(cmd *exec.Cmd, mutating bool, repoRoot string) error {
	_, wait, err := startGitCmd(cmd, mutating, repoRoot)
	if err != nil {
		return err
	}
	return wait()
}

// postProcessGit replaces the raw output of add/commit with the concise
// feedback the dedicated tools used to provide.
func postProcessGit(ctx context.Context, repoRoot, sub, out string) (string, error) {
	switch sub {
	case "add":
		return stagedSummary(ctx, repoRoot)
	case "commit":
		if res, err := resolveCommitInfo(ctx, repoRoot); err == nil {
			return formatGitCommitResult(res), nil
		}
	case "check-ignore":
		if strings.TrimSpace(out) == "" {
			return "none of the listed paths are git-ignored", nil
		}
	}
	return formatGitOutput(sub, out), nil
}

// isExitCode reports whether err is an *exec.ExitError with the given exit code.
func isExitCode(err error, code int) bool {
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode() == code
	}
	return false
}

// enhanceGitError rewrites a few cryptic git failures into actionable guidance.
// Each case is a self-contained hint helper returning "" when it does not apply,
// so adding a rewrite never disturbs the others.
func enhanceGitError(repoRoot, msg string) string {
	if hint := submodulePathspecHint(repoRoot, msg); hint != "" {
		return msg + hint
	}
	if hint := untrackedPathspecHint(msg); hint != "" {
		return msg + hint
	}
	if hint := indexLockHint(repoRoot, msg); hint != "" {
		return msg + hint
	}
	return msg
}

// indexLockHint addresses a stale `.git/index.lock` (left by a crashed git
// process) that blocks add/commit with "Unable to create '.../index.lock': File
// exists". We surface the exact remedy rather than auto-removing the lock — in a
// shared worktree another live git/plumb process may legitimately hold it, so
// silent removal is unsafe. Returns "" when msg is not this failure.
func indexLockHint(repoRoot, msg string) string {
	if !strings.Contains(msg, "index.lock") || !strings.Contains(msg, "File exists") {
		return ""
	}
	lock := filepath.Join(repoRoot, ".git", "index.lock")
	return fmt.Sprintf(
		"\n  This is a leftover lock from a git process that did not exit cleanly. "+
			"First confirm no git is running (e.g. `pgrep -fl git`); if none is, remove the stale lock with `rm -f %s`, then retry. "+
			"plumb does not remove it automatically because another session may hold it in a shared worktree.",
		lock,
	)
}

// submodulePathspecHint addresses git's "Pathspec '<path>' is in submodule
// '<name>'" failure — emitted when a write (e.g. add, or commit -- <path>) names
// a path that lives inside a nested submodule while git runs in the
// superproject. A submodule is a separate repository, so the superproject can
// only record its commit pointer, never stage its file contents; the operation
// must target the submodule directly. Returns "" when msg is not this failure.
func submodulePathspecHint(repoRoot, msg string) string {
	const marker = "is in submodule"
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return ""
	}
	name := firstQuoted(msg[idx:])
	if name == "" {
		return ""
	}
	sub := filepath.Join(repoRoot, name)
	return fmt.Sprintf(
		"\n  %q is a git submodule — a separate repository nested in this one. "+
			"A git command run in the superproject cannot stage or commit files inside it (the superproject tracks only the submodule's commit pointer). "+
			"Re-run the git tool with repo=%q (a path inside the submodule) and give files relative to that root. "+
			"After committing inside the submodule, record the moved pointer with a separate add+commit in the superproject.",
		name, sub,
	)
}

// untrackedPathspecHint addresses git's "pathspec '<path>' did not match any
// file(s) known to git" failure on a path-limited commit (`commit -- <path>`).
// The usual cause is a freshly-created, still-untracked file: a path-limited
// commit only commits already-tracked paths, so git cannot match one git has
// never seen. The remedy is to stage it first. Returns "" when msg is not this
// failure — the submodule variant ("is in submodule") is handled separately.
func untrackedPathspecHint(msg string) string {
	if !strings.Contains(msg, "did not match any file") {
		return ""
	}
	path := firstQuoted(msg)
	if path == "" {
		return ""
	}
	return fmt.Sprintf(
		"\n  %q is not yet tracked by git, so a path-limited commit cannot match it "+
			"(commit -- <path> only commits already-tracked paths). "+
			"Stage it first with the git tool — subcommand \"add\", files [%q] — then commit.",
		path, path,
	)
}

// firstQuoted returns the text inside the first pair of single quotes in s, or
// "" when there is no such pair. git quotes pathspec and submodule names this way.
func firstQuoted(s string) string {
	_, after, ok := strings.Cut(s, "'")
	if !ok {
		return ""
	}
	rest := after
	before0, _, ok0 := strings.Cut(rest, "'")
	if !ok0 {
		return ""
	}
	return before0
}

func formatGitOutput(sub, result string) string {
	const maxLogLines = 200
	if sub == "log" || sub == "blame" {
		result = truncateLines(result, maxLogLines,
			fmt.Sprintf("… (showing first %d lines — add --oneline / -n N to narrow, or use args to filter)", maxLogLines))
	}
	if len(result) > maxGitBytes {
		result = result[:maxGitBytes] + "\n… (output truncated at 100 KiB)"
	}
	if strings.TrimSpace(result) == "" {
		return "(no output)"
	}
	return result
}

// stagedSummary returns a description of what is currently in the index.
func stagedSummary(ctx context.Context, repoRoot string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "diff", "--cached", "--name-status")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "staged (could not read index summary)", nil
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return "nothing staged", nil
	}
	lines := strings.Split(trimmed, "\n")
	return fmt.Sprintf("staged %d file(s):\n%s", len(lines), trimmed), nil
}

type gitCommitResult struct {
	Hash    string // full SHA-1
	Subject string // first line of commit message
}

func resolveCommitInfo(ctx context.Context, repoRoot string) (gitCommitResult, error) {
	cmd := exec.CommandContext(ctx, "git", gitNoOptionalLocks, "log", "-1", "--format=%H\t%s")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return gitCommitResult{}, fmt.Errorf("git commit: reading commit info: %w", err)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "\t", 2)
	if len(parts) < 2 {
		return gitCommitResult{Hash: strings.TrimSpace(string(out))}, nil
	}
	return gitCommitResult{Hash: parts[0], Subject: parts[1]}, nil
}

func formatGitCommitResult(r gitCommitResult) string {
	short := r.Hash
	if len(short) > 7 {
		short = short[:7]
	}
	if short == "" {
		return r.Subject
	}
	return fmt.Sprintf("%s %s", short, r.Subject)
}

// truncateLines caps s at maxLines lines. If the output is longer, the suffix
// is appended on a new line after the last included line.
func truncateLines(s string, maxLines int, suffix string) string {
	lines := strings.SplitN(s, "\n", maxLines+2)
	if len(lines) <= maxLines+1 {
		return s // fits within limit
	}
	return strings.Join(lines[:maxLines], "\n") + "\n" + suffix
}

// findGitRoot returns the root of the git repository that contains path. An
// empty path is an error, never the daemon's cwd: the daemon is a singleton
// shared across connections, so falling back to its working directory would run
// git against an unrelated repository (a cross-session isolation leak). Callers
// must resolve and boundary-check the repo before reaching here.
func findGitRoot(path string) (string, error) {
	if path == "" {
		return "", errors.New("no repository path")
	}
	start := path

	info, err := os.Stat(start)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", start, err)
	}
	dir := start
	if !info.IsDir() {
		dir = filepath.Dir(start)
	}

	out, err := exec.Command("git", gitNoOptionalLocks, "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", dir)
	}
	return strings.TrimSpace(string(out)), nil
}
