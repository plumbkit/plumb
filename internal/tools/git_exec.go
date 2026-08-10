package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
func runGit(ctx context.Context, repo, sub string, argv []string, tier gitTier, guard *gitRefGuard, intentWarn func(context.Context, string) string) (string, error) {
	repoRoot, err := findGitRoot(repo)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}
	execCtx := ctx
	if tier != tierRead {
		var cleanup func()
		execCtx, cleanup, err = beginSerialisedGit(ctx, repoRoot, sub, tier)
		if err != nil {
			return "", err
		}
		defer cleanup()
	}
	if err := guardRefPreExec(execCtx, guard, repoRoot, sub); err != nil {
		return "", err
	}
	warning := ""
	if intentWarn != nil {
		warning = intentWarn(execCtx, repoRoot)
	}
	if tier == tierRead {
		argv = gitReadArgv(argv)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(execCtx, "git", argv...)
	cmd.Dir = repoRoot
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	mutating := tier == tierWrite || tier == tierDestructive
	if err := execGitCmd(cmd, mutating, repoRoot); err != nil {
		// git check-ignore exits 1 when NONE of the listed paths are ignored —
		// a normal "no match" result, not a failure.
		if sub == "check-ignore" && isExitCode(err, 1) && strings.TrimSpace(stderr.String()) == "" {
			return postProcessGit(ctx, repoRoot, sub, stdout.String())
		}
		// warning is attached here too, not just on the success path below: a
		// failure is exactly when a peer's claim ("rebasing ops main") is most
		// likely to be the explanation, and the query cost was already paid.
		return "", gitCommandError(repoRoot, sub, argv, err, stdout.String(), stderr.String(), warning)
	}
	guard.postExec(execCtx)
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String() // switch/push and friends report on stderr
	}
	processed, err := postProcessGit(ctx, repoRoot, sub, out)
	return warning + processed, err
}

const (
	// maxGitErrStreamBytes bounds each stream quoted in a failure response.
	maxGitErrStreamBytes = 16 * 1024
	// maxGitErrStdoutLines bounds the trailing stdout lines quoted on failure.
	maxGitErrStdoutLines = 40
)

// gitCommandError builds the tool-facing error for a failed git subprocess.
// warning (may be "") is the repo-intent advisory block computed before the
// git child ran (git_intent_warn.go) — it leads the message on a failure just
// as it leads the response on success, so a peer's live claim explaining the
// failure (e.g. a rebase collision) is not silently dropped by the one path
// where it matters most. After that, the exit code leads, then stderr and
// stdout are each quoted under their own label (bounded). Previously whichever
// stream was non-empty became the error string itself — so a failing
// pre-commit hook that wrote only to stdout surfaced as `git commit: 0
// issues. file-size: OK`, the hook's chatter standing in for the real cause.
// Success-path output is unaffected; this runs only on a non-zero exit.
func gitCommandError(repoRoot, sub string, argv []string, runErr error, stdout, stderr, warning string) error {
	var b strings.Builder
	b.WriteString(warning)
	fmt.Fprintf(&b, "git %s: %s", sub, gitExitDescription(runErr))
	truncated := false
	if msg, cut := boundGitErrStream(stderr); msg != "" {
		// The hint rewrites (index.lock, pathspec, submodule) all match git's
		// own diagnostics, which git writes to stderr — enhancing this stream
		// preserves their behaviour exactly.
		fmt.Fprintf(&b, "\nstderr:\n%s", enhanceGitError(repoRoot, msg))
		truncated = truncated || cut
	}
	if out, cut := tailGitErrStdout(stdout); out != "" {
		fmt.Fprintf(&b, "\nstdout (last %d lines):\n%s", maxGitErrStdoutLines, out)
		truncated = truncated || cut
	}
	if truncated {
		b.WriteString("\n" + gitRerunNote(argv, repoRoot))
	}
	return errors.New(b.String())
}

// gitExitDescription describes how the git child ended: its exit code when it
// ran to a non-zero exit, or the raw error when there is no exit code (a
// start failure or context cancellation).
func gitExitDescription(err error) string {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return fmt.Sprintf("exit code %d", ee.ExitCode())
	}
	return err.Error()
}

// boundGitErrStream trims a captured stream and caps it at
// maxGitErrStreamBytes, keeping the tail — the most recent output, where the
// diagnosis lives. The second return reports whether truncation cut anything.
func boundGitErrStream(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) <= maxGitErrStreamBytes {
		return s, false
	}
	s = s[len(s)-maxGitErrStreamBytes:]
	// Drop the partial first line so the quote starts on a line boundary.
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return s, true
}

// tailGitErrStdout keeps the trailing lines of stdout for a failure report,
// capped at maxGitErrStdoutLines lines on top of the shared byte cap.
func tailGitErrStdout(s string) (string, bool) {
	s, truncated := boundGitErrStream(s)
	lines := strings.Split(s, "\n")
	if len(lines) > maxGitErrStdoutLines {
		lines = lines[len(lines)-maxGitErrStdoutLines:]
		truncated = true
	}
	return strings.Join(lines, "\n"), truncated
}

// gitRerunNote points at the way to see the complete output of a failed
// command whose quoted streams were truncated: run the same git invocation
// directly in the repository.
func gitRerunNote(argv []string, repoRoot string) string {
	return fmt.Sprintf("… (output truncated — re-run `git %s` in %s for the complete output)",
		quoteGitArgv(argv), repoRoot)
}

// quoteGitArgv renders an argv as a copy-pasteable command line, quoting any
// argument that contains whitespace or a single quote.
func quoteGitArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		if strings.ContainsAny(a, " \t'") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		quoted[i] = a
	}
	return strings.Join(quoted, " ")
}

// beginSerialisedGit prepares a non-read git op: it refuses new work while the
// daemon is draining for shutdown, registers the op as in-flight, takes the
// per-repo lock, and — for index/ref-mutating tiers — reaps any attributable
// stale lock left by a dead daemon and returns a cancellation-decoupled, bounded
// exec context so a shutdown mid-commit lets git finish. (The owner sidecar is
// stamped by execGitCmd once the child pid is known.) The returned cleanup
// closure (which the caller defers) reverses all of it. Network tiers serialise
// and drain-gate but keep request-context cancellation (a push can hang on auth
// — it must stay interruptible) and write no owner sidecar (they do not create
// index.lock).
func beginSerialisedGit(ctx context.Context, repoRoot, sub string, tier gitTier) (context.Context, func(), error) {
	if gitWriteDrainActive() {
		return nil, nil, fmt.Errorf("git %s: %w", sub, errGitDraining)
	}
	gitWriteInflight.Add(1)
	release, err := lockRepo(ctx, repoRoot)
	if err != nil {
		gitWriteInflight.Done()
		return nil, nil, fmt.Errorf("git %s: %w", sub, err)
	}
	execCtx := ctx
	cancel := func() {}
	if tier == tierWrite || tier == tierDestructive {
		reapStaleGitLock(repoRoot)
		execCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), gitWriteGrace)
	}
	cleanup := func() {
		cancel()
		release()
		gitWriteInflight.Done()
	}
	return execCtx, cleanup, nil
}

// execGitCmd starts cmd, stamps the owner sidecar with the git child's pid for a
// mutating op (so a stranded index.lock is attributable to the actual lock
// holder, not the daemon), and waits. Returns the child's run error.
func execGitCmd(cmd *exec.Cmd, mutating bool, repoRoot string) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if mutating {
		recordGitLockOwner(repoRoot, cmd.Process.Pid)
		defer clearGitLockOwner(repoRoot)
	}
	return cmd.Wait()
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
	var ee *exec.ExitError
	if errors.As(err, &ee) {
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
	i := strings.IndexByte(s, '\'')
	if i < 0 {
		return ""
	}
	rest := s[i+1:]
	j := strings.IndexByte(rest, '\'')
	if j < 0 {
		return ""
	}
	return rest[:j]
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
