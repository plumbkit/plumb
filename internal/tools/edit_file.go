package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

var editFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Absolute path or file:// URI of the file to edit."
    },
    "edits": {
      "type": "array",
      "description": "Ordered list of str_replace edits to apply sequentially.",
      "items": {
        "type": "object",
        "properties": {
          "old_str": {
            "type": "string",
            "description": "Exact string to find. Must appear EXACTLY ONCE in the current file content — edit is rejected if the string is absent or appears more than once. CRLF / LF differences between old_str and the file are tolerated automatically."
          },
          "new_str": {
            "type": "string",
            "description": "Replacement string. Use empty string to delete old_str."
          }
        },
        "required": ["old_str", "new_str"]
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
    }
  },
  "required": ["path", "edits"]
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
//  2. Uniqueness lock: each old_str must appear EXACTLY ONCE. If the file was
//     modified concurrently (old_str absent or context changed), the edit is
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
// CRLF/LF handling: line endings in old_str are normalised against the file
// before matching, so an old_str with LF can match a file with CRLF.
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
	return "Apply one or more str_replace edits to an existing file. Each edit specifies " +
		"an old_str that must appear EXACTLY ONCE in the file — if it is absent or " +
		"ambiguous the edit is rejected. CRLF differences between old_str and the file " +
		"are tolerated automatically. All edits are applied sequentially in memory, then " +
		"written atomically (temp file + rename). A per-path lock serialises " +
		"concurrent edits to the same file from any session. Optionally pass " +
		"expected_mtime (from a prior read_file header) to guarantee the file hasn't " +
		"changed since you read it. The response includes a line-range summary of what changed."
}

type strEdit struct {
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

type editFileArgs struct {
	Path          string    `json:"path"`
	Edits         []strEdit `json:"edits"`
	ExpectedMtime string    `json:"expected_mtime"`
	ExpectedSha   string    `json:"expected_sha"`
	DirtyOk       bool      `json:"dirty_ok"`
}

func (t *EditFile) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	if !t.deps.Limiter.Allow() {
		return "", rateLimitError("edit_file", t.deps.Limiter)
	}
	var a editFileArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("edit_file: invalid arguments: %w", err)
	}
	if a.Path == "" {
		return "", fmt.Errorf("edit_file: path is required")
	}
	if len(a.Edits) == 0 {
		return "", fmt.Errorf("edit_file: at least one edit is required")
	}

	path := strings.TrimPrefix(a.Path, "file://")

	// Per-path lock: serialise all concurrent writes to this path.
	unlock := lockPath(path)
	defer unlock()

	if !a.DirtyOk && pathIsDirty(ctx, path) {
		return "", &editLogicErr{fmt.Errorf("edit_file: %q has uncommitted changes; "+
			"review and commit first, or pass dirty_ok: true to proceed", path)}
	}

	// expected_mtime gate (optimistic concurrency).
	if a.ExpectedMtime != "" {
		want, err := time.Parse(time.RFC3339Nano, a.ExpectedMtime)
		if err != nil {
			return "", &editLogicErr{fmt.Errorf("edit_file: expected_mtime is not RFC3339Nano: %w", err)}
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", &editLogicErr{fmt.Errorf("edit_file: stat %q: %w", path, err)}
		}
		if !info.ModTime().Equal(want) {
			return "", &editLogicErr{fmt.Errorf(
				"edit_file: file %q was modified since you read it\n"+
					"  expected_mtime: %s\n"+
					"  current mtime:  %s\n"+
					"  Re-read the file and try again.",
				path, want.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano),
			)}
		}
	}

	// expected_sha gate — content-hash check, stronger than mtime.
	if a.ExpectedSha != "" {
		current, err := fileSHA256(path)
		if err != nil {
			return "", &editLogicErr{fmt.Errorf("edit_file: computing sha256 of %q: %w", path, err)}
		}
		if current != a.ExpectedSha {
			return "", &editLogicErr{fmt.Errorf(
				"edit_file: file %q content has changed since you read it\n"+
					"  expected sha256: %s\n"+
					"  current  sha256: %s\n"+
					"  Re-read the file and try again.",
				path, a.ExpectedSha, current,
			)}
		}
	}

	// Strict mode: every edit requires a prior read with matching mtime.
	// Opt-in via config [edits] strict = true, or PLUMB_STRICT_EDITS=1.
	// expected_mtime above is the more precise signal when an agent threads
	// it through; strict mode catches the case where the agent forgot.
	if t.isStrict() {
		recorded := t.deps.Reads.Mtime(path)
		if recorded.IsZero() {
			return "", &editLogicErr{fmt.Errorf(
				"edit_file: strict mode: %q has not been read in this daemon session — call read_file first",
				path)}
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", &editLogicErr{fmt.Errorf("edit_file: stat %q: %w", path, err)}
		}
		if !info.ModTime().Equal(recorded) {
			return "", &editLogicErr{fmt.Errorf(
				"edit_file: strict mode: %q has changed since you read it\n"+
					"  recorded mtime: %s\n"+
					"  current mtime:  %s\n"+
					"  Re-read the file and try again.",
				path, recorded.Format(time.RFC3339Nano), info.ModTime().Format(time.RFC3339Nano))}
		}
	}

	uri := "file://" + path
	var preDiags []protocol.Diagnostic
	if t.deps.Diag != nil {
		preDiags = t.deps.Diag.Diagnostics(uri)
	}

	var lastErr error
	for attempt := 1; attempt <= maxEditRetries; attempt++ {
		result, before, content, err := t.tryEdit(ctx, path, a.Edits)
		if err != nil {
			if isEditLogicError(err) {
				return "", err
			}
			lastErr = err
			slog.Warn("edit_file: attempt failed", "path", path, "attempt", attempt, "err", err)
			continue
		}

		if concurrentWriteDetected(path, result, t.deps.ConcurrentWriteSkew) {
			slog.Warn("edit_file: concurrent write detected after rename, retrying",
				"path", path, "attempt", attempt)
			lastErr = fmt.Errorf(
				"concurrent write detected after attempt %d: another process modified %q "+
					"while this edit was in progress", attempt, path)
			continue
		}

		if err := notifyLSP(ctx, t.deps.Client, path, protocol.FileChanged); err != nil {
			slog.Warn("edit_file: LSP notification failed", "path", path, "err", err)
		}
		invalidateCache(t.deps.Cache, uri)

		noun := "edit"
		if len(a.Edits) > 1 {
			noun = "edits"
		}
		summary := summariseLineChanges(before, content)
		var sb strings.Builder
		fmt.Fprintf(&sb, "applied %d %s to %s (%d bytes)", len(a.Edits), noun, path, len(content))
		if attempt > 1 {
			fmt.Fprintf(&sb, " (succeeded on attempt %d)", attempt)
		}
		if info, err := os.Stat(path); err == nil {
			fmt.Fprintf(&sb, "\nmtime: %s", info.ModTime().Format(time.RFC3339Nano))
		}
		if summary != "" {
			sb.WriteString("\n")
			sb.WriteString(summary)
		}
		if t.deps.Diag != nil {
			fresh := awaitDiagnosticsRefresh(t.deps.Diag, uri, preDiags, t.deps.PostWriteDiagWindow)
			sb.WriteString(formatPostWriteDiagnostics(fresh))
		}
		return sb.String(), nil
	}

	return "", fmt.Errorf("edit_file: failed after %d attempts: %w", maxEditRetries, lastErr)
}

// tryEdit reads the file, applies all edits in memory, and writes the result.
// Returns (writeResult, originalContent, newContent, error). Errors from edit
// logic (old_str not found, ambiguous) are marked with editLogicErr so the
// caller knows not to retry them.
//
// Pre-rename mtime check: between reading the file and writing the result,
// we re-stat and confirm the mtime hasn't changed. If it has, another
// process wrote during our edit and we surface a retryable error.
func (t *EditFile) tryEdit(ctx context.Context, path string, edits []strEdit) (writeResult, string, string, error) {
	_ = ctx

	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return writeResult{}, "", "", &editLogicErr{
				fmt.Errorf("edit_file: file not found: %q — use write_file to create new files", path),
			}
		}
		return writeResult{}, "", "", fmt.Errorf("edit_file: stat %q: %w", path, statErr)
	}
	preReadMtime := info.ModTime()

	data, err := os.ReadFile(path)
	if err != nil {
		return writeResult{}, "", "", fmt.Errorf("edit_file: reading %q: %w", path, err)
	}
	original := string(data)
	content := original

	for i, edit := range edits {
		if edit.OldStr == "" {
			return writeResult{}, "", "", &editLogicErr{
				fmt.Errorf("edit_file: edit[%d]: old_str must not be empty — use write_file to replace the entire file", i),
			}
		}
		// CRLF tolerance: if the file uses CRLF and old_str doesn't (or
		// vice versa), normalise old_str to match the file's line ending
		// style before comparison.
		oldStr := matchLineEndings(edit.OldStr, content)
		newStr := matchLineEndings(edit.NewStr, content)

		count := strings.Count(content, oldStr)
		switch count {
		case 0:
			return writeResult{}, "", "", &editLogicErr{
				t.notFoundError(i, path, edit.OldStr, oldStr, preReadMtime),
			}
		case 1:
			content = strings.Replace(content, oldStr, newStr, 1)
		default:
			return writeResult{}, "", "", &editLogicErr{
				ambiguousError(i, count, path, edit.OldStr, oldStr),
			}
		}
	}

	// Pre-rename mtime check: did anything change between our read and now?
	if info2, err := os.Stat(path); err == nil {
		if !info2.ModTime().Equal(preReadMtime) {
			return writeResult{}, "", "", fmt.Errorf(
				"edit_file: file %q changed between read and write (mtime moved from %s to %s) — retry will re-read",
				path, preReadMtime.Format(time.RFC3339Nano), info2.ModTime().Format(time.RFC3339Nano))
		}
	}

	perm := info.Mode().Perm()
	if perm == 0 {
		perm = 0o644
	}

	res, err := safeWrite(path, []byte(content), perm)
	if err != nil {
		return writeResult{}, "", "", fmt.Errorf("edit_file: write failed: %w", err)
	}

	return res, original, content, nil
}

// notFoundError builds the "old_str not found" error. It tiers its message on
// what the daemon knows about the agent's prior read of this path: when a
// read_file mtime is recorded and differs from the current mtime, the file
// definitely changed since the agent read it (re-read needed); when the
// recorded mtime equals the current mtime, the file is unchanged and the
// snippet itself is wrong (snippet needs verification); when no read is
// recorded, we fall back to the generic message.
//
// If matchLineEndings transformed old_str, both the sent and the searched
// forms are surfaced so the agent can see what plumb actually looked for.
func (t *EditFile) notFoundError(i int, path, sent, searched string, preReadMtime time.Time) error {
	recorded := t.deps.Reads.Mtime(path)

	sentSnippet := truncateSnippet(sent)
	searchedSnippet := truncateSnippet(searched)

	var b strings.Builder
	fmt.Fprintf(&b, "edit_file: edit[%d]: old_str not found in %q", i, path)

	switch {
	case !recorded.IsZero() && !recorded.Equal(preReadMtime):
		fmt.Fprintf(&b, " — file has been modified since you read it")
	case !recorded.IsZero():
		fmt.Fprintf(&b, " — file unchanged since your read; the snippet is incorrect")
	}

	fmt.Fprintf(&b, "\n  old_str: %q", sentSnippet)
	if searched != sent {
		fmt.Fprintf(&b, "\n  searched (after newline normalisation): %q", searchedSnippet)
	}

	switch {
	case !recorded.IsZero() && !recorded.Equal(preReadMtime):
		fmt.Fprintf(&b, "\n  your read mtime: %s", recorded.Format(time.RFC3339Nano))
		fmt.Fprintf(&b, "\n  current mtime:  %s", preReadMtime.Format(time.RFC3339Nano))
		b.WriteString("\n  Re-read the file with read_file, then retry with the updated content.")
	case !recorded.IsZero():
		fmt.Fprintf(&b, "\n  This file has not been modified since your read at %s.", recorded.Format(time.RFC3339Nano))
		b.WriteString("\n  Verify the snippet character-by-character — whitespace, line endings, and stray punctuation are the usual culprits.")
	default:
		b.WriteString("\n  The file may have been modified since you last read it, or the string is incorrect.")
		b.WriteString("\n  Use read_file to check the current content.")
	}
	return errors.New(b.String())
}

// ambiguousError builds the "old_str appears N times" error. Re-reading
// doesn't help when the snippet is non-unique, so we don't consult
// ReadTracker here — we only surface the post-normalisation form when it
// differs from what the agent sent.
func ambiguousError(i, count int, path, sent, searched string) error {
	sentSnippet := truncateSnippet(sent)
	searchedSnippet := truncateSnippet(searched)

	var b strings.Builder
	fmt.Fprintf(&b, "edit_file: edit[%d]: old_str appears %d times in %q — must be unique", i, count, path)
	fmt.Fprintf(&b, "\n  old_str: %q", sentSnippet)
	if searched != sent {
		fmt.Fprintf(&b, "\n  searched (after newline normalisation): %q", searchedSnippet)
	}
	b.WriteString("\n  Add more surrounding context to old_str to make it unambiguous.")
	return errors.New(b.String())
}

// truncateSnippet caps s at 60 characters with an ellipsis. Used to keep
// edit-error messages compact for callers that surface them in chat.
func truncateSnippet(s string) string {
	if len(s) <= 60 {
		return s
	}
	return s[:60] + "…"
}

// matchLineEndings normalises s so its newline style matches that of ref.
// If ref contains CRLF and s only LF, all LF in s are upgraded to CRLF (and
// pre-existing CRLF in s left alone). If ref is pure LF, s is normalised to LF.
func matchLineEndings(s, ref string) string {
	refHasCRLF := strings.Contains(ref, "\r\n")
	sHasCRLF := strings.Contains(s, "\r\n")
	if refHasCRLF && !sHasCRLF {
		return strings.ReplaceAll(s, "\n", "\r\n")
	}
	if !refHasCRLF && sHasCRLF {
		return strings.ReplaceAll(s, "\r\n", "\n")
	}
	return s
}

// summariseLineChanges returns a compact human-readable description of which
// line numbers changed between before and after. Best-effort: shows up to 5
// ranges; collapses adjacent differing lines into a single range.
func summariseLineChanges(before, after string) string {
	if before == after {
		return ""
	}
	bl := strings.Split(before, "\n")
	al := strings.Split(after, "\n")

	type rng struct{ start, end int }
	var ranges []rng

	// Walk both line arrays in parallel, treating the shorter as padded.
	max := len(bl)
	if len(al) > max {
		max = len(al)
	}
	inRun := false
	var runStart int
	for i := 0; i < max; i++ {
		var b, a string
		if i < len(bl) {
			b = bl[i]
		}
		if i < len(al) {
			a = al[i]
		}
		if b != a {
			if !inRun {
				runStart = i + 1
				inRun = true
			}
		} else if inRun {
			ranges = append(ranges, rng{runStart, i})
			inRun = false
		}
	}
	if inRun {
		ranges = append(ranges, rng{runStart, max})
	}
	if len(ranges) == 0 {
		return ""
	}
	var parts []string
	limit := 5
	for i, r := range ranges {
		if i >= limit {
			parts = append(parts, fmt.Sprintf("…(+%d more)", len(ranges)-limit))
			break
		}
		if r.start == r.end {
			parts = append(parts, fmt.Sprintf("L%d", r.start))
		} else {
			parts = append(parts, fmt.Sprintf("L%d-%d", r.start, r.end))
		}
	}
	return "lines changed: " + strings.Join(parts, ", ")
}

// editLogicErr wraps errors caused by bad edit logic (wrong old_str, empty
// old_str, ambiguous match, expected_mtime mismatch). These are distinct from
// I/O or concurrency errors — retrying won't fix them.
type editLogicErr struct{ err error }

func (e *editLogicErr) Error() string { return e.err.Error() }
func (e *editLogicErr) Unwrap() error { return e.err }

// isEditLogicError reports whether err is an edit logic error that should not
// be retried.
func isEditLogicError(err error) bool {
	var le *editLogicErr
	return errors.As(err, &le)
}
