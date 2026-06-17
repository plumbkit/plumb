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
// are unreachable; all other subcommands pass args through.
func buildGitArgv(a gitToolArgs) ([]string, error) {
	switch a.Subcommand {
	case "commit":
		if strings.TrimSpace(a.Message) == "" {
			return nil, fmt.Errorf("git commit: message is required")
		}
		argv := []string{"commit", "-m", a.Message}
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
			return nil, fmt.Errorf("git add: at least one path is required (use the files parameter)")
		}
		return append([]string{"add", "-A", "--"}, a.Files...), nil
	default:
		return append([]string{a.Subcommand}, a.Args...), nil
	}
}

// runGit runs a git subcommand in the repository containing repo. Non-read tiers
// (index/ref-mutating + network) are serialised per repo so concurrent
// plumb-initiated writes queue rather than collide on .git/index.lock; read-tier
// ops never lock. For the index/ref-mutating tiers the git child also runs under
// a cancellation-decoupled, bounded context (see beginSerialisedGit) so a daemon
// shutdown or connection eviction mid-commit lets git finish and release the
// lock rather than SIGKILLing it and stranding the lock.
func runGit(ctx context.Context, repo, sub string, argv []string, tier gitTier) (string, error) {
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
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", sub, enhanceGitError(repoRoot, msg))
	}
	out := stdout.String()
	if strings.TrimSpace(out) == "" {
		out = stderr.String() // switch/push and friends report on stderr
	}
	return postProcessGit(ctx, repoRoot, sub, out)
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
	cmd := exec.CommandContext(ctx, "git", "diff", "--cached", "--name-status")
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
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%H\t%s")
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
		return "", fmt.Errorf("no repository path")
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

	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not a git repository: %s", dir)
	}
	return strings.TrimSpace(string(out)), nil
}
