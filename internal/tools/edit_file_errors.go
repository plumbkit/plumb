package tools

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// notFoundError builds the "old_string not found" error. It tiers its message on
// what the daemon knows about the agent's prior read of this path: when a
// read_file mtime is recorded and differs from the current mtime, the file
// definitely changed since the agent read it (re-read needed); when the
// recorded mtime equals the current mtime, the file is unchanged and the
// snippet itself is wrong (snippet needs verification); when no read is
// recorded, we fall back to the generic message.
//
// If matchLineEndings transformed old_string, both the sent and the searched
// forms are surfaced so the agent can see what plumb actually looked for.
func (t *EditFile) notFoundError(i int, path, sent, searched string, preReadMtime time.Time) error {
	recorded := t.deps.Reads.Mtime(path)

	sentSnippet := truncateSnippet(sent)
	searchedSnippet := truncateSnippet(searched)

	var b strings.Builder
	fmt.Fprintf(&b, "edit_file: edit[%d]: old_string not found in %q", i, path)

	switch {
	case !recorded.IsZero() && !recorded.Equal(preReadMtime):
		fmt.Fprintf(&b, " — file has been modified since you read it")
	case !recorded.IsZero():
		fmt.Fprintf(&b, " — file unchanged since your read; the snippet is incorrect")
	}

	fmt.Fprintf(&b, "\n  old_string: %q", sentSnippet)
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
	// Multi-line unambiguous gutters are stripped automatically before this
	// error can fire (resolveStrMatch); this hint covers the residue — a
	// single guttered line, or a stripped form that still didn't match.
	if looksGuttered(searched) {
		b.WriteString("\n  Hint: old_string appears to include the display-only line-number gutter from read_file/read_symbol (\"<n>\\t\" at line start) — strip the gutter and retry.")
	}
	return errors.New(b.String())
}

// ambiguousError builds the "old_string appears N times" error. Re-reading
// doesn't help when the snippet is non-unique, so we don't consult
// ReadTracker here — we only surface the post-normalisation form when it
// differs from what the agent sent.
func ambiguousError(i, count int, path, sent, searched string) error {
	sentSnippet := truncateSnippet(sent)
	searchedSnippet := truncateSnippet(searched)

	var b strings.Builder
	fmt.Fprintf(&b, "edit_file: edit[%d]: old_string appears %d times in %q — must be unique", i, count, path)
	fmt.Fprintf(&b, "\n  old_string: %q", sentSnippet)
	if searched != sent {
		fmt.Fprintf(&b, "\n  searched (after newline normalisation): %q", searchedSnippet)
	}
	b.WriteString("\n  Add more surrounding context to old_string to make it unambiguous.")
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

// editLogicErr wraps errors caused by bad edit logic (wrong old_string, empty
// old_string, ambiguous match, expected_mtime mismatch). These are distinct from
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
