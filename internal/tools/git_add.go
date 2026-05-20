package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

var gitAddSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "files": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Absolute paths of files or directories to stage. Required — no glob expansion is performed, so each entry must be an explicit path."
    },
    "repo": {
      "type": "string",
      "description": "Path to any file or directory inside the repository. Omit to derive from the first entry in files."
    }
  },
  "required": ["files"]
}`)

// GitAdd stages specific paths for the next commit.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).
type GitAdd struct {
	deps WriteDeps
}

func NewGitAdd(deps WriteDeps) *GitAdd { return &GitAdd{deps: deps} }

func (t *GitAdd) Name() string                 { return "git_add" }
func (t *GitAdd) InputSchema() json.RawMessage { return gitAddSchema }
func (t *GitAdd) Description() string {
	return "Stage specific files for the next git commit (git add -- <files>). " +
		"Requires an explicit list of absolute paths — no glob expansion is performed, " +
		"so only the named paths are staged. " +
		"If repo is omitted, the repository root is derived from the first entry in files. " +
		"Returns a summary of what is currently staged after the operation."
}

type gitAddArgs struct {
	Files []string `json:"files"`
	Repo  string   `json:"repo"`
}

func (a gitAddArgs) validate() error {
	if len(a.Files) == 0 {
		return fmt.Errorf("git_add: at least one file path is required")
	}
	return nil
}

func (t *GitAdd) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("git_add", t.deps.Limiter)
	}
	a, err := parseGitAddArgs(raw)
	if err != nil {
		return "", err
	}
	if err := a.validate(); err != nil {
		return "", err
	}
	return t.run(ctx, a)
}

func parseGitAddArgs(raw json.RawMessage) (gitAddArgs, error) {
	var a gitAddArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("git_add: invalid arguments: %w", err)
	}
	return a, nil
}

func (t *GitAdd) run(ctx context.Context, a gitAddArgs) (string, error) {
	repoSeed := a.Repo
	if repoSeed == "" {
		repoSeed = a.Files[0]
	}
	repoRoot, err := findGitRoot(repoSeed)
	if err != nil {
		return "", fmt.Errorf("git_add: %w", err)
	}
	//nolint:gosec // G204: a.Files are absolute paths supplied by the MCP caller; "--" separates them from flags so no injection is possible via exec (no shell involved).
	cmd := exec.CommandContext(ctx, "git", append([]string{"add", "--"}, a.Files...)...)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git_add: %s", strings.TrimSpace(string(out)))
	}
	return stagedSummary(ctx, repoRoot)
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
