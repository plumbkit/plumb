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

func runGit(ctx context.Context, repo, sub string, argv []string) (string, error) {
	repoRoot, err := findGitRoot(repo)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = repoRoot
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
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
// Currently: a stale `.git/index.lock` (left by a crashed git process) blocks
// add/commit with "Unable to create '.../index.lock': File exists" and no
// in-plumb remedy. We surface the exact remedy rather than auto-removing the
// lock — in a shared worktree another live git/plumb process may legitimately
// hold it, so silent removal is unsafe.
func enhanceGitError(repoRoot, msg string) string {
	if !strings.Contains(msg, "index.lock") || !strings.Contains(msg, "File exists") {
		return msg
	}
	lock := filepath.Join(repoRoot, ".git", "index.lock")
	hint := fmt.Sprintf(
		"\n  This is a leftover lock from a git process that did not exit cleanly. "+
			"First confirm no git is running (e.g. `pgrep -fl git`); if none is, remove the stale lock with `rm -f %s`, then retry. "+
			"plumb does not remove it automatically because another session may hold it in a shared worktree.",
		lock,
	)
	return msg + hint
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

// findGitRoot returns the root of the git repository that contains path.
// If path is empty, the current working directory is used.
func findGitRoot(path string) (string, error) {
	start := path
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("getting working directory: %w", err)
		}
	}

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
