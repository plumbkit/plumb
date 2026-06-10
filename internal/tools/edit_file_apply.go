package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// editFileApply runs the retry loop, notifies the LSP on success, and
// delegates response formatting to formatEditFileSuccess.
func (t *EditFile) editFileApply(ctx context.Context, path string, a editFileArgs, uri string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= maxEditRetries; attempt++ {
		result, before, content, notes, err := t.tryEdit(ctx, path, a.Edits)
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
		t.deps.recordWritten(path)
		return t.formatEditFileSuccess(path, attempt, a.Edits, before, content, uri, a.AwaitDiagnostics, notes), nil
	}
	return "", fmt.Errorf("edit_file: failed after %d attempts: %w", maxEditRetries, lastErr)
}

func (t *EditFile) formatEditFileSuccess(path string, attempt int, edits []strEdit, before, content, uri string, awaitFresh bool, notes []string) string {
	noun := "edit"
	if len(edits) > 1 {
		noun = "edits"
	}
	summary := summariseLineChanges(before, content)
	var sb strings.Builder
	fmt.Fprintf(&sb, "applied %d %s to %s %s", len(edits), noun, path, sizeSummary(content))
	if attempt > 1 {
		fmt.Fprintf(&sb, " (succeeded on attempt %d)", attempt)
	}
	if info, err := os.Stat(path); err == nil {
		fmt.Fprintf(&sb, "\nmtime: %s", info.ModTime().Format(time.RFC3339Nano))
	}
	for _, n := range notes {
		fmt.Fprintf(&sb, "\n%s", n)
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

// applyStrEdit applies one str_replace edit to content: CRLF normalisation +
// gutter forgiveness via resolveStrMatch (literal match always first), the
// exactly-once / replace_all contract, and a non-empty advisory note when the
// display-only read_file line-number gutter was stripped before matching.
func (t *EditFile) applyStrEdit(content string, edit strEdit, i int, path string, preReadMtime time.Time) (string, string, error) {
	if edit.OldStr == "" {
		return "", "", &editLogicErr{
			fmt.Errorf("edit_file: edit[%d]: old_string must not be empty — use write_file to replace the entire file or start_line to replace by line range", i),
		}
	}
	oldStr, newStr, count, stripped := resolveStrMatch(content, edit)
	var note string
	if stripped {
		note = fmt.Sprintf("note: edit[%d] %s", i, gutterStrippedNote)
	}
	if count == 0 {
		return "", "", &editLogicErr{t.notFoundError(i, path, edit.OldStr, oldStr, preReadMtime)}
	}
	if edit.ReplaceAll {
		return strings.ReplaceAll(content, oldStr, newStr), note, nil
	}
	if count > 1 {
		return "", "", &editLogicErr{ambiguousError(i, count, path, edit.OldStr, oldStr)}
	}
	return strings.Replace(content, oldStr, newStr, 1), note, nil
}

// tryEdit reads the file, applies all edits in memory, and writes the result.
// Returns (writeResult, originalContent, newContent, notes, error); notes
// carry per-edit advisories (e.g. gutter forgiveness fired) for the success
// response. Errors from edit logic (old_string not found, ambiguous) are
// marked with editLogicErr so the caller knows not to retry them.
//
// Pre-rename mtime check: between reading the file and writing the result,
// we re-stat and confirm the mtime hasn't changed. If it has, another
// process wrote during our edit and we surface a retryable error.
func (t *EditFile) tryEdit(ctx context.Context, path string, edits []strEdit) (writeResult, string, string, []string, error) {
	_ = ctx

	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return writeResult{}, "", "", nil, &editLogicErr{
				fmt.Errorf("edit_file: file not found: %q — use write_file to create new files", path),
			}
		}
		return writeResult{}, "", "", nil, fmt.Errorf("edit_file: stat %q: %w", path, statErr)
	}
	preReadMtime := info.ModTime()

	data, err := os.ReadFile(path)
	if err != nil {
		return writeResult{}, "", "", nil, fmt.Errorf("edit_file: reading %q: %w", path, err)
	}
	original := string(data)
	content := original

	var notes []string
	for i, edit := range edits {
		if edit.StartLine != 0 {
			newStr := matchLineEndings(edit.NewStr, content)
			var rerr error
			content, rerr = applyRangeEdit(content, edit.StartLine, edit.EndLine, newStr)
			if rerr != nil {
				return writeResult{}, "", "", nil, &editLogicErr{
					fmt.Errorf("edit_file: edit[%d]: %w", i, rerr),
				}
			}
			continue
		}
		updated, note, serr := t.applyStrEdit(content, edit, i, path, preReadMtime)
		if serr != nil {
			return writeResult{}, "", "", nil, serr
		}
		if note != "" {
			notes = append(notes, note)
		}
		content = updated
	}

	// Pre-rename mtime check: did anything change between our read and now?
	if info2, err := os.Stat(path); err == nil {
		if !info2.ModTime().Equal(preReadMtime) {
			return writeResult{}, "", "", nil, fmt.Errorf(
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
		return writeResult{}, "", "", nil, fmt.Errorf("edit_file: write failed: %w", err)
	}

	return res, original, content, notes, nil
}
