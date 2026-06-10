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

// partialEditResult records the outcome of one edit in an apply_partial call.
type partialEditResult struct {
	index     int
	applied   bool
	lineRange string
	note      string // advisory shown on success (e.g. gutter forgiveness fired)
	err       error
}

// executePartial applies each edit independently, recording per-edit outcomes.
// Successful edits accumulate into the final content; failed edits are skipped.
// The file is written once at the end if any edit succeeded.
func (t *EditFile) executePartial(
	ctx context.Context,
	path string,
	edits []strEdit,
	uri string,
	awaitFresh bool,
) string {
	results, res, original, content, writeErr := t.tryEditPartial(ctx, path, edits)
	applied := countApplied(results)
	var sb strings.Builder
	sb.WriteString(t.formatPartialHeader(path, original, content, applied, len(edits), writeErr))
	sb.WriteString(formatPartialEditsResults(results))
	if writeErr == nil && applied > 0 {
		_ = res
		t.executePartialPostWrite(ctx, path, uri, content, awaitFresh, &sb)
	}
	return sb.String()
}

func countApplied(results []partialEditResult) int {
	n := 0
	for _, r := range results {
		if r.applied {
			n++
		}
	}
	return n
}

func (t *EditFile) formatPartialHeader(path, original, content string, applied, total int, writeErr error) string {
	var sb strings.Builder
	if writeErr != nil {
		fmt.Fprintf(&sb, "partial apply: write failed after %d successful edit(s): %v\n\n", applied, writeErr)
	} else if applied == 0 {
		sb.WriteString("partial apply: all edits failed — file not modified\n\n")
	} else {
		fmt.Fprintf(&sb, "partial apply: applied %d of %d edit(s) to %s (%d bytes)\n",
			applied, total, path, len(content))
		if info, err := os.Stat(path); err == nil {
			fmt.Fprintf(&sb, "mtime: %s\n", info.ModTime().Format(time.RFC3339Nano))
		}
		if s := summariseLineChanges(original, content); s != "" {
			fmt.Fprintf(&sb, "%s\n", s)
		}
		if t.deps.showWriteDiff() {
			if d := unifiedDiff(path, original, content); d != "" {
				sb.WriteString(d)
			}
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func formatPartialEditsResults(results []partialEditResult) string {
	var sb strings.Builder
	sb.WriteString("edit results:\n")
	for _, r := range results {
		if r.applied {
			if r.lineRange != "" {
				fmt.Fprintf(&sb, "  [%d] applied: %s\n", r.index, r.lineRange)
			} else {
				fmt.Fprintf(&sb, "  [%d] applied (no line change)\n", r.index)
			}
			if r.note != "" {
				fmt.Fprintf(&sb, "      note: %s\n", r.note)
			}
		} else {
			fmt.Fprintf(&sb, "  [%d] FAILED: %v\n", r.index, r.err)
		}
	}
	return sb.String()
}

func (t *EditFile) executePartialPostWrite(ctx context.Context, path, uri, content string, awaitFresh bool, sb *strings.Builder) {
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
	sb.WriteString(t.deps.postWriteDiagnostics(uri, content, awaitFresh))
}

// applyPartialEdit applies a single edit to content and returns the (possibly
// unchanged) content plus the per-edit outcome. It never mutates shared state —
// the caller advances content only when res.applied is true.
func (t *EditFile) applyPartialEdit(content string, edit strEdit, i int, path string, preReadMtime time.Time) (string, partialEditResult) {
	if edit.StartLine != 0 {
		newStr := matchLineEndings(edit.NewStr, content)
		updated, rerr := applyRangeEdit(content, edit.StartLine, edit.EndLine, newStr)
		if rerr != nil {
			return content, partialEditResult{index: i, err: rerr}
		}
		return updated, partialEditResult{index: i, applied: true, lineRange: summariseLineChanges(content, updated)}
	}
	if edit.OldStr == "" {
		return content, partialEditResult{index: i, err: fmt.Errorf("old_string must not be empty — use write_file to replace the entire file or start_line to replace by line range")}
	}
	oldStr, newStr, count, stripped := resolveStrMatch(content, edit)
	if count == 0 {
		return content, partialEditResult{index: i, err: t.notFoundError(i, path, edit.OldStr, oldStr, preReadMtime)}
	}
	if !edit.ReplaceAll && count > 1 {
		return content, partialEditResult{index: i, err: ambiguousError(i, count, path, edit.OldStr, oldStr)}
	}
	var note string
	if stripped {
		note = gutterStrippedNote
	}
	before := content
	if edit.ReplaceAll {
		content = strings.ReplaceAll(content, oldStr, newStr)
	} else {
		content = strings.Replace(content, oldStr, newStr, 1)
	}
	return content, partialEditResult{index: i, applied: true, lineRange: summariseLineChanges(before, content), note: note}
}

// tryEditPartial reads the file and applies each edit independently.
// Returns per-edit results, writeResult, original content, final content, and any write error.
// If no edits succeeded the file is not written and writeResult is zero.
func (t *EditFile) tryEditPartial(ctx context.Context, path string, edits []strEdit) ([]partialEditResult, writeResult, string, string, error) {
	_ = ctx

	info, statErr := os.Stat(path)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return nil, writeResult{}, "", "", &editLogicErr{
				fmt.Errorf("edit_file: file not found: %q — use write_file to create new files", path),
			}
		}
		return nil, writeResult{}, "", "", fmt.Errorf("edit_file: stat %q: %w", path, statErr)
	}
	preReadMtime := info.ModTime()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, writeResult{}, "", "", fmt.Errorf("edit_file: reading %q: %w", path, err)
	}
	original := string(data)
	content := original

	results := make([]partialEditResult, len(edits))
	for i, edit := range edits {
		updated, res := t.applyPartialEdit(content, edit, i, path, preReadMtime)
		results[i] = res
		if res.applied {
			content = updated
		}
	}

	if content == original {
		return results, writeResult{}, original, original, nil
	}

	if info2, err := os.Stat(path); err == nil {
		if !info2.ModTime().Equal(preReadMtime) {
			return results, writeResult{}, original, original, fmt.Errorf(
				"edit_file: file %q changed between read and write — retry required", path,
			)
		}
	}

	perm := info.Mode().Perm()
	if perm == 0 {
		perm = 0o644
	}
	res, writeErr := safeWrite(path, []byte(content), perm)
	if writeErr != nil {
		return results, writeResult{}, original, original, fmt.Errorf("edit_file: write failed: %w", writeErr)
	}
	return results, res, original, content, nil
}
