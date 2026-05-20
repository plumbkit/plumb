package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

var gitCommitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "message": {
      "type": "string",
      "description": "Commit message."
    },
    "repo": {
      "type": "string",
      "description": "Path to any file or directory inside the repository. Omit to use the current working directory."
    }
  },
  "required": ["message"]
}`)

// GitCommit stages files and creates a git commit.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).
type GitCommit struct {
	deps WriteDeps
}

func NewGitCommit(deps WriteDeps) *GitCommit { return &GitCommit{deps: deps} }

func (t *GitCommit) Name() string                 { return "git_commit" }
func (t *GitCommit) InputSchema() json.RawMessage { return gitCommitSchema }
func (t *GitCommit) Description() string {
	return "Create a git commit from whatever is currently staged in the index. " +
		"Call git_add first to stage the files you want to include. " +
		"Pre-commit hooks always run; there is no --no-verify escape hatch. " +
		"Returns the new short commit hash and subject on success."
}

type gitCommitArgs struct {
	Message string `json:"message"`
	Repo    string `json:"repo"`
}

func (a gitCommitArgs) validate() error {
	if strings.TrimSpace(a.Message) == "" {
		return fmt.Errorf("git_commit: message is required")
	}
	return nil
}

type gitCommitResult struct {
	Hash    string // full SHA-1
	Subject string // first line of commit message
}

func (t *GitCommit) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("git_commit", t.deps.Limiter)
	}
	a, err := parseGitCommitArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	res, err := t.run(ctx, a)
	if err != nil {
		return "", err
	}
	return formatGitCommitResult(res), nil
}

func parseGitCommitArgs(raw json.RawMessage) (gitCommitArgs, error) {
	var a gitCommitArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("git_commit: invalid arguments: %w", err)
	}
	return a, nil
}

func (t *GitCommit) run(ctx context.Context, a gitCommitArgs) (gitCommitResult, error) {
	repoRoot, err := findGitRoot(a.Repo)
	if err != nil {
		return gitCommitResult{}, fmt.Errorf("git_commit: %w", err)
	}
	return createCommit(ctx, repoRoot, a.Message)
}

func createCommit(ctx context.Context, repoRoot, message string) (gitCommitResult, error) {
	cmd := exec.CommandContext(ctx, "git", "commit", "-m", message)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return gitCommitResult{}, fmt.Errorf("git_commit: %s", strings.TrimSpace(string(out)))
	}
	return resolveCommitInfo(ctx, repoRoot)
}

func resolveCommitInfo(ctx context.Context, repoRoot string) (gitCommitResult, error) {
	cmd := exec.CommandContext(ctx, "git", "log", "-1", "--format=%H\t%s")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return gitCommitResult{}, fmt.Errorf("git_commit: reading commit info: %w", err)
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
