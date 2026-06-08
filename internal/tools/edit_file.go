package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// edit_file is split across files by concern: the all-or-nothing apply path
// lives in edit_file_apply.go; the apply_partial path in edit_file_partial.go;
// error construction and the line-ending / line-change helpers in
// edit_file_errors.go. This file holds the Tool surface and the precondition
// gates (dirty / optimistic-concurrency / strict mode).

var editFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file to edit."
    },
    "edits": {
      "type": "array",
      "description": "Ordered list of str_replace edits to apply sequentially.",
      "items": {
        "type": "object",
        "properties": {
          "old_string": {
            "type": "string",
            "description": "Exact string to find. Required when start_line is not set. Must appear EXACTLY ONCE in the current file content — edit is rejected if absent or ambiguous. CRLF / LF differences are tolerated automatically."
          },
          "new_string": {
            "type": "string",
            "description": "Replacement text. Use empty string to delete. When start_line is set, replaces the specified line range (or appends at end of file when start_line is -1)."
          },
          "start_line": {
            "type": "integer",
            "description": "First line to replace (1-based, inclusive). When set, old_string is not used. Use -1 to append new_string at end of file. Use end_line: -1 to extend the range to the last line."
          },
          "end_line": {
            "type": "integer",
            "description": "Last line to replace (1-based, inclusive). Defaults to start_line when absent (single-line operation). Use -1 for end of file. Only used when start_line is set."
          }
        },
        "required": ["new_string"],
        "additionalProperties": false
      },
      "minItems": 1
    },
    "expected_mtime": {
      "type": "string",
      "description": "Optional. RFC3339Nano mtime previously returned by read_file. If provided, the edit is rejected if the file's current mtime differs — fast optimistic-concurrency check."
    },
    "expected_sha": {
      "type": "string",
      "description": "Optional. Hex-encoded SHA-256 previously returned by read_file. If provided, the edit is rejected if the file's current content hash differs — stronger than expected_mtime, survives mtime aliasing."
    },
    "dirty_ok": {
      "type": "boolean",
      "description": "Allow editing a file that has uncommitted changes in its git repository. Default false — the edit is refused if the target file is dirty. Pass true to proceed anyway."
    },
    "apply_partial": {
      "type": "boolean",
      "description": "When true, apply each edit independently and continue on failure instead of rolling back the entire batch. Returns a per-edit result list showing which edits succeeded and which failed. Incompatible with strict mode — not safe when concurrent agents share the file."
    },
    "await_diagnostics": {
      "type": "boolean",
      "description": "When true, block up to a few seconds for the language server to finish re-analysing this file and report an authoritative post-write result — a clean fresh pass is stated explicitly. Use it for a trustworthy \"did my change compile?\" answer instead of shelling out to a build. Default false (fast adaptive window; the result may predate the write)."
    }
  },
  "required": ["file_path", "edits"],
  "additionalProperties": false
}`)

// maxEditRetries is the maximum number of times edit_file will retry when it
// detects a concurrent write between its read and rename.
const maxEditRetries = 3

// EditFile applies one or more str_replace edits to a file.
//
// Safety model (five layers):
//
//  1. Per-path lock: a process-global lock serialises concurrent edit_file /
//     write_file calls to the same path. Two parallel sessions cannot interleave
//     read/write operations on the same file.
//
//  2. Uniqueness lock: each old_string must appear EXACTLY ONCE. If the file was
//     modified concurrently (old_string absent or context changed), the edit is
//     rejected with a clear error — no silent corruption possible.
//
//  3. Optional expected_mtime: when supplied, the file's current mtime must
//     match. Rejects edits to a file that changed since the agent's read.
//
//  4. In-memory application: all edits are applied in memory to produce the
//     final content before any write occurs. If any edit fails, the file is
//     not touched.
//
//  5. Atomic write + retry: content is staged in os.TempDir() and renamed.
//     A pre-rename mtime check rejects writes if the file changed between
//     our read and the rename. A post-rename mtime check triggers a retry
//     (up to maxEditRetries=3) if a third party wrote after our rename.
//
// CRLF/LF handling: line endings in old_string are normalised against the file
// before matching, so an old_string with LF can match a file with CRLF.
//
// Concurrency: Execute is safe for concurrent use.
type EditFile struct{ deps WriteDeps }

func NewEditFile(deps WriteDeps) *EditFile { return &EditFile{deps: deps} }

// isStrict reports whether strict mode applies to this call. Prefers the
// configured StrictModeFn (per-workspace + env merged by daemon); falls
// back to env-only check when no closure is wired.
func (t *EditFile) isStrict() bool {
	if t.deps.Strict != nil {
		return t.deps.Strict()
	}
	return strictModeEnabled()
}

func (*EditFile) Name() string                 { return "edit_file" }
func (*EditFile) InputSchema() json.RawMessage { return editFileSchema }
func (*EditFile) Description() string {
	return "Apply one or more edits to an existing file. " +
		"Use this tool — not Claude Code's native Edit/Write — for every in-workspace file change: " +
		"plumb and the Claude Code harness track read-state separately, so a native Edit after a plumb " +
		"read_file fails with \"File has not been read yet\". Pair read_file with edit_file and stay in one lane. " +
		"Two edit modes per item: " +
		"(1) str_replace mode (default): set old_string to the text that must appear EXACTLY ONCE — " +
		"rejected if absent or ambiguous. " +
		"(2) range mode: set start_line (1-based) to replace lines start_line..end_line with new_string; " +
		"use start_line: -1 to append at end of file; end_line: -1 to delete or replace through the last line. " +
		"Range mode is the clean solution for deleting a block of lines (no unique anchor needed) and for " +
		"appending to a file. " +
		"CRLF differences between old_string and the file are tolerated automatically. " +
		"All edits are applied sequentially in memory then written atomically (temp file + rename). " +
		"A per-path lock serialises concurrent edits. Optionally pass expected_mtime (from a prior " +
		"read_file header) to guarantee the file hasn't changed since you read it."
}

type strEdit struct {
	OldStr    string `json:"old_string"`
	NewStr    string `json:"new_string"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type editFileArgs struct {
	Path             string    `json:"file_path"`
	Edits            []strEdit `json:"edits"`
	ExpectedMtime    string    `json:"expected_mtime"`
	ExpectedSha      string    `json:"expected_sha"`
	DirtyOk          bool      `json:"dirty_ok"`
	ApplyPartial     bool      `json:"apply_partial"`
	AwaitDiagnostics bool      `json:"await_diagnostics"`
}

func (t *EditFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("edit_file", t.deps.Limiter)
	}
	a, err := parseEditFileArgs(raw)
	if err != nil {
		return "", err
	}

	path := strings.TrimPrefix(a.Path, "file://")
	if err := t.deps.checkBoundary(path); err != nil {
		return "", fmt.Errorf("edit_file: %w", err)
	}

	// Per-path lock: serialise all concurrent writes to this path.
	unlock := lockPath(path)
	defer unlock()

	if err := t.editFilePreconditions(ctx, path, a); err != nil {
		return "", err
	}

	uri := "file://" + path

	if a.ApplyPartial {
		t.deps.notifyTopology(path)
		return t.executePartial(ctx, path, a.Edits, uri, a.AwaitDiagnostics) + t.deps.reportQuality(ctx, path), nil
	}
	result, err := t.editFileApply(ctx, path, a, uri)
	if err != nil {
		return "", err
	}
	t.deps.notifyTopology(path)
	return result + t.deps.reportQuality(ctx, path), nil
}

func parseEditFileArgs(raw json.RawMessage) (editFileArgs, error) {
	var a editFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("edit_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return a, fmt.Errorf("edit_file: file_path is required")
	}
	if len(a.Edits) == 0 {
		return a, fmt.Errorf("edit_file: at least one edit is required")
	}
	return a, nil
}

// editFilePreconditions runs the dirty-check, optimistic-concurrency, and
// strict-mode gates before any read or write.
func (t *EditFile) editFilePreconditions(ctx context.Context, path string, a editFileArgs) error {
	if !a.DirtyOk && dirtyBlocksWrite(ctx, t.deps.Writes, path) {
		return &editLogicErr{fmt.Errorf("edit_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", path)}
	}
	if a.ExpectedMtime != "" {
		want, err := time.Parse(time.RFC3339Nano, a.ExpectedMtime)
		if err != nil {
			return &editLogicErr{fmt.Errorf("edit_file: expected_mtime is not RFC3339Nano: %w", err)}
		}
		info, err := os.Stat(path)
		if err != nil {
			return &editLogicErr{fmt.Errorf("edit_file: stat %q: %w", path, err)}
		}
		if !info.ModTime().Equal(want) {
			return &editLogicErr{fmt.Errorf(
				"edit_file: file %q was modified since you read it\n"+
					"  expected_mtime: %s\n"+
					"  current mtime:  %s\n"+
					"  Re-read the file and try again",
				path, want.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano),
			)}
		}
	}
	if a.ExpectedSha != "" {
		current, err := fileSHA256(path)
		if err != nil {
			return &editLogicErr{fmt.Errorf("edit_file: computing sha256 of %q: %w", path, err)}
		}
		if current != a.ExpectedSha {
			return &editLogicErr{fmt.Errorf(
				"edit_file: file %q content has changed since you read it\n"+
					"  expected sha256: %s\n"+
					"  current  sha256: %s\n"+
					"  Re-read the file and try again",
				path, a.ExpectedSha, current,
			)}
		}
	}
	if !t.isStrict() {
		return nil
	}
	recorded := t.deps.Reads.Mtime(path)
	if recorded.IsZero() {
		return &editLogicErr{fmt.Errorf(
			"edit_file: strict mode: %q has not been read in this daemon session — call read_file first",
			path,
		)}
	}
	info, err := os.Stat(path)
	if err != nil {
		return &editLogicErr{fmt.Errorf("edit_file: stat %q: %w", path, err)}
	}
	if !info.ModTime().Equal(recorded) {
		return &editLogicErr{fmt.Errorf(
			"edit_file: strict mode: %q has changed since you read it\n"+
				"  recorded mtime: %s\n"+
				"  current mtime:  %s\n"+
				"  Re-read the file and try again",
			path, recorded.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano),
		)}
	}
	return nil
}
