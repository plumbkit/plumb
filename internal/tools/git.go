package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var gitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "subcommand": {
      "type": "string",
      "description": "Git subcommand to run. Allowed: diff, log, show, blame, status, branch, tag, shortlog, stash"
    },
    "args": {
      "type": "array",
      "items": {"type": "string"},
      "description": "Arguments passed directly to git, e.g. [\"HEAD~3\", \"--stat\"] or [\"-10\", \"--oneline\", \"--\", \"main.go\"]"
    },
    "repo": {
      "type": "string",
      "description": "Path to any file or directory inside the repository. Omit to use the current working directory."
    }
  },
  "required": ["subcommand"]
}`)

// allowedGitSubcommands is the set of read-only git subcommands this tool will run.
// Destructive subcommands (commit, push, reset, checkout, merge, …) are intentionally absent.
var allowedGitSubcommands = map[string]bool{
	"diff":     true,
	"log":      true,
	"show":     true,
	"blame":    true,
	"status":   true,
	"branch":   true,
	"tag":      true,
	"shortlog": true,
	"stash":    true,
}

const maxGitBytes = 100 * 1024 // 100 KiB

// Git runs read-only git subcommands and returns their output as text.
//
// Concurrency: Execute is safe for concurrent use (no shared mutable state).
type Git struct{}

func NewGit() *Git { return &Git{} }

func (t *Git) Name() string                 { return "git" }
func (t *Git) InputSchema() json.RawMessage { return gitSchema }
func (t *Git) Description() string {
	return "Safe read-only git surface — only inspection subcommands are accepted; destructive operations (commit, push, reset, rebase, checkout, etc.) are rejected, so this is safe to call without confirmation. " +
		"Allowed: diff (any flags/refs/paths), log (any format/range), show, blame, status, branch, tag, shortlog, stash list/show. " +
		"Pass git flags and arguments directly via args, e.g. args:[\"-U5\",\"HEAD~1\",\"--\",\"main.go\"] for diff. " +
		"Essential for clients without shell access (Claude Desktop, Cursor MCP); for hosts that have a shell, this still records the call in stats."
}

type gitToolArgs struct {
	Subcommand string   `json:"subcommand"`
	Args       []string `json:"args"`
	Repo       string   `json:"repo"`
}

func (t *Git) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a gitToolArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("git: invalid arguments: %w", err)
	}
	if !allowedGitSubcommands[a.Subcommand] {
		allowed := "diff, log, show, blame, status, branch, tag, shortlog, stash"
		return "", fmt.Errorf("git: subcommand %q is not permitted; allowed: %s", a.Subcommand, allowed)
	}

	repoRoot, err := findGitRoot(a.Repo)
	if err != nil {
		return "", fmt.Errorf("git: %w", err)
	}

	cmdArgs := append([]string{a.Subcommand}, a.Args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Dir = repoRoot

	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr != "" {
				return "", fmt.Errorf("git %s: %s", a.Subcommand, stderr)
			}
		}
		return "", fmt.Errorf("git %s: %w", a.Subcommand, err)
	}

	result := string(out)

	// For log and blame, apply a line-count cap so large histories or files
	// don't flood the agent context. The byte cap is the final safety net.
	const maxLogLines = 200
	if a.Subcommand == "log" || a.Subcommand == "blame" {
		result = truncateLines(result, maxLogLines,
			fmt.Sprintf("… (showing first %d lines — add --oneline / -n N to narrow, or use args to filter)", maxLogLines))
	}
	if len(result) > maxGitBytes {
		result = result[:maxGitBytes] + "\n… (output truncated at 100 KiB)"
	}
	if strings.TrimSpace(result) == "" {
		return "(no output)", nil
	}
	return result, nil
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
