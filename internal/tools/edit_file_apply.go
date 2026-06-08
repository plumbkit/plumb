package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// editFileApply runs the retry loop, notifies the LSP on success, and
// delegates response formatting to formatEditFileSuccess.
func (t *EditFile) editFileApply(ctx context.Context, path string, a editFileArgs, uri string) (string, error) {
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
		if concurrentWriteDetected(path, result, t.deps.concurrentWriteSkew()) {
			slog.Warn("edit_file: concurrent write detected after rename, retrying",
				"path", path, "attempt", attempt)
			lastErr = fmt.Errorf(
				"concurrent write detected after attempt %d: another process modified %q "+
					"while this edit was in progress", attempt, path,
			)
			continue
		}
		if err := notifyLSP(ctx, t.deps.Client, path, protocol.FileChanged); err != nil {
			slog.Warn("edit_file: LSP notification failed", "path", path, "err", err)
		}
		if t.deps.PostWriteNotifyFn != nil {
			if err := t.deps.PostWriteNotifyFn(ctx, path); err != nil {
				slog.Warn("edit_file: post-write adapter notification failed", "path", path, "err", err)
			}
		}
		invalidateCache(t.deps.Cache, uri)
		t.deps.Writes.Record(path)
		return t.formatEditFileSuccess(path, attempt, a.Edits, before, content, uri, a.AwaitDiagnostics), nil
	}
	return "", fmt.Errorf("edit_file: failed after %d attempts: %w", maxEditRetries, lastErr)
}

func (t *EditFile) formatEditFileSuccess(path string, attempt int, edits []strEdit, before, content, uri string, awaitFresh bool) string {
	noun := "edit"
	if len(edits) > 1 {
		noun = "edits"
	}
	summary := summariseLineChanges(before, content)
	var sb strings.Builder
	fmt.Fprintf(&sb, "applied %d %s to %s (%d bytes)", len(edits), noun, path, len(content))
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
	if t.deps.showWriteDiff() {
		if d := unifiedDiff(path, before, content); d != "" {
			sb.WriteString("\n")
			sb.WriteString(d)
		}
	}
	sb.WriteString(t.deps.postWriteDiagnostics(uri, content, awaitFresh))
	return sb.String()
}

// tryEdit reads the file, applies all edits in memory, and writes the result.
// Returns (writeResult, originalContent, newContent, error). Errors from edit
// logic (old_string not found, ambiguous) are marked with editLogicErr so the
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
		if edit.StartLine != 0 {
			newStr := matchLineEndings(edit.NewStr, content)
			var rerr error
			content, rerr = applyRangeEdit(content, edit.StartLine, edit.EndLine, newStr)
			if rerr != nil {
				return writeResult{}, "", "", &editLogicErr{
					fmt.Errorf("edit_file: edit[%d]: %w", i, rerr),
				}
			}
			continue
		}
		if edit.OldStr == "" {
			return writeResult{}, "", "", &editLogicErr{
				fmt.Errorf("edit_file: edit[%d]: old_string must not be empty — use write_file to replace the entire file or start_line to replace by line range", i),
			}
		}
		// CRLF tolerance: if the file uses CRLF and old_string doesn't (or
		// vice versa), normalise old_string to match the file's line ending
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
				path, preReadMtime.Format(time.RFC3339Nano), info2.ModTime().Format(time.RFC3339Nano),
			)
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
