package tools

// edit_file_anchor.go — the anchor-bounded edit mode. Instead of an exact
// old_string, the caller supplies two unique anchors (start_anchor, end_anchor)
// and a new_string replacing the span they bound. The resolver mirrors the
// str_replace matcher (CRLF tolerance, display-only gutter forgiveness,
// exactly-once uniqueness) and lowers the request into a single synthetic
// str_replace edit, so the actual write reuses the existing apply path verbatim
// — only computing the replacement span is new.

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// resolveAnchorEdit reads the current file bytes and lowers the anchor-bounded
// request into one synthetic str_replace edit. The returned note carries a
// gutter-forgiveness advisory when an anchor's display-only line-number prefix
// was stripped before matching. Callers run this under the per-path lock.
func (t *EditFile) resolveAnchorEdit(path string, a editFileArgs) (strEdit, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return strEdit{}, "", &editLogicErr{
				fmt.Errorf("edit_file: file not found: %q — use write_file to create new files", path),
			}
		}
		return strEdit{}, "", fmt.Errorf("edit_file: reading %q: %w", path, err)
	}
	return buildAnchorEdit(string(data), a)
}

// buildAnchorEdit computes the replacement span between (or including) the two
// anchors against content and returns the equivalent str_replace edit. The
// synthetic old_string is the full inclusive span start..end — which contains
// both unique anchors and therefore occurs exactly once — so the downstream
// exactly-once matcher re-verifies it on the real write. new_string re-attaches
// the matched anchors unless include_anchors is set.
func buildAnchorEdit(content string, a editFileArgs) (strEdit, string, error) {
	start, startCount, startStripped := resolveAnchor(content, a.StartAnchor)
	if err := anchorMatchError("start_anchor", a.StartAnchor, start, startCount); err != nil {
		return strEdit{}, "", err
	}
	end, endCount, endStripped := resolveAnchor(content, a.EndAnchor)
	if err := anchorMatchError("end_anchor", a.EndAnchor, end, endCount); err != nil {
		return strEdit{}, "", err
	}

	startIdx := strings.Index(content, start)
	endIdx := strings.Index(content, end)
	if endIdx < startIdx+len(start) {
		return strEdit{}, "", &editLogicErr{errors.New(
			"edit_file: end_anchor must occur after start_anchor in the file (and the two must not overlap)",
		)}
	}

	fullSpan := content[startIdx : endIdx+len(end)]
	newStr := matchLineEndings(a.NewStr, content)

	replacement := start + newStr + end
	if a.IncludeAnchors {
		replacement = newStr
	}
	return strEdit{OldStr: fullSpan, NewStr: replacement}, anchorGutterNote(startStripped, endStripped), nil
}

// resolveAnchor normalises an anchor against content and counts its
// occurrences, retrying once with the display-only line-number gutter stripped
// when the literal form misses — exactly mirroring resolveStrMatch, so an
// anchor copied verbatim from guttered read_file output still resolves.
func resolveAnchor(content, anchor string) (resolved string, count int, gutterStripped bool) {
	norm := matchLineEndings(anchor, content)
	if c := strings.Count(content, norm); c > 0 {
		return norm, c, false
	}
	stripped, ok := stripLineGutter(norm)
	if !ok {
		return norm, 0, false
	}
	return stripped, strings.Count(content, stripped), true
}

// anchorMatchError builds the zero-match / ambiguous-match error for one anchor,
// mirroring the str_replace uniqueness contract. Returns nil when count == 1.
func anchorMatchError(field, sent, searched string, count int) error {
	switch {
	case count == 0:
		var b strings.Builder
		fmt.Fprintf(&b, "edit_file: %s not found in the file: %q", field, truncateSnippet(sent))
		if searched != sent {
			fmt.Fprintf(&b, "\n  searched (after newline normalisation): %q", truncateSnippet(searched))
		}
		if looksGuttered(searched) {
			b.WriteString("\n  Hint: the anchor appears to include the display-only line-number gutter " +
				"from read_file (\"<n>\\t\" at line start) — strip the gutter and retry.")
		}
		return &editLogicErr{errors.New(b.String())}
	case count > 1:
		return &editLogicErr{fmt.Errorf(
			"edit_file: %s appears %d times in the file — must be unique; add more surrounding context: %q",
			field, count, truncateSnippet(sent),
		)}
	}
	return nil
}

// anchorGutterNote returns the gutter-forgiveness advisory when either anchor
// had its display-only line-number prefix stripped before matching.
func anchorGutterNote(startStripped, endStripped bool) string {
	if !startStripped && !endStripped {
		return ""
	}
	return "note: anchor included the display-only line-number gutter from read_file — " +
		"stripped automatically before matching; omit the \"<n>\\t\" prefix in future edits"
}
