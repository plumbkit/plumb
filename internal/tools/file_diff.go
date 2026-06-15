package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

var fileDiffSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_a": {
      "type": "string",
      "description": "Path to the first file (the 'before' side). Absolute path, file:// URI, or workspace-relative path."
    },
    "file_b": {
      "type": "string",
      "description": "Path to the second file (the 'after' side). Absolute path, file:// URI, or workspace-relative path."
    },
    "context_lines": {
      "type": "integer",
      "description": "Lines of context shown around each change (default 3)"
    },
    "ignore_whitespace": {
      "type": "boolean",
      "description": "Ignore whitespace-only differences"
    }
  },
  "required": ["file_a", "file_b"],
  "additionalProperties": false
}`)

const maxFileDiffBytes = 100 * 1024 // 100 KiB

// FileDiff produces a unified diff between two arbitrary files.
// No git repository is required.
//
// Concurrency: Execute is safe for concurrent use.
type FileDiff struct {
	guard BoundaryGuard
	ws    WorkspaceFn // may be nil; anchors workspace-relative file_a/file_b to the pinned root
}

func NewFileDiff() *FileDiff { return &FileDiff{} }

func (t *FileDiff) WithBoundary(guard BoundaryGuard) *FileDiff {
	t.guard = guard
	return t
}

// WithWorkspace wires the pinned-workspace accessor so relative file_a/file_b
// resolve against the workspace root rather than the daemon's working
// directory. Nil-safe.
func (t *FileDiff) WithWorkspace(ws WorkspaceFn) *FileDiff {
	t.ws = ws
	return t
}

func (t *FileDiff) Name() string                 { return "file_diff" }
func (t *FileDiff) InputSchema() json.RawMessage { return fileDiffSchema }
func (t *FileDiff) Description() string {
	return "Returns a unified diff between two arbitrary files. Works outside git — for tracked files use the git tool's diff subcommand instead, which understands refs and the index. " +
		"Use context_lines to control surrounding context and ignore_whitespace to skip formatting-only changes. " +
		"Essential for clients without shell access (Claude Desktop, Cursor MCP)."
}

type fileDiffArgs struct {
	FileA            string `json:"file_a"`
	FileB            string `json:"file_b"`
	ContextLines     *int   `json:"context_lines"`
	IgnoreWhitespace bool   `json:"ignore_whitespace"`
}

func (t *FileDiff) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a fileDiffArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("file_diff: invalid arguments: %w", err)
	}
	if a.FileA == "" || a.FileB == "" {
		return "", fmt.Errorf("file_diff: file_a and file_b are required")
	}
	a.FileA = resolvePath(a.FileA, t.ws)
	a.FileB = resolvePath(a.FileB, t.ws)
	if err := t.guard.check(a.FileA); err != nil {
		return "", fmt.Errorf("file_diff: %w", err)
	}
	if err := t.guard.check(a.FileB); err != nil {
		return "", fmt.Errorf("file_diff: %w", err)
	}

	contextLines := 3
	if a.ContextLines != nil {
		contextLines = *a.ContextLines
	}

	args := []string{fmt.Sprintf("-U%d", contextLines)}
	if a.IgnoreWhitespace {
		args = append(args, "-w")
	}
	args = append(args, a.FileA, a.FileB)

	cmd := exec.CommandContext(ctx, "diff", args...)
	out, err := cmd.Output()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			return "", fmt.Errorf("file_diff: running diff: %w", err)
		}
		switch exitErr.ExitCode() {
		case 1:
			// Exit code 1 means the files differ — this is the normal case.
		case 2:
			stderr := strings.TrimSpace(string(exitErr.Stderr))
			if stderr == "" {
				stderr = err.Error()
			}
			return "", fmt.Errorf("file_diff: %s", stderr)
		default:
			return "", fmt.Errorf("file_diff: diff exited with code %d", exitErr.ExitCode())
		}
	}

	if len(out) == 0 {
		return "(files are identical)", nil
	}

	result := string(out)
	if len(result) > maxFileDiffBytes {
		result = result[:maxFileDiffBytes] + "\n… (output truncated at 100 KiB)"
	}
	return result, nil
}
