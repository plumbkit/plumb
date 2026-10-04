package tools

// diff.go — the write tools' response rendering of a change, over
// internal/textdiff (which owns the Myers script, hunking and apply). This file
// adds only the path header and the maxDiffLines cap.

import (
	"strings"

	"github.com/plumbkit/plumb/internal/textdiff"
)

// maxDiffLines is the maximum number of diff output lines (header + hunks)
// included in a write-tool response. Diffs that exceed this are truncated.
const maxDiffLines = 80

// maxMyersDistance aliases the textdiff bound for this package's tests.
const maxMyersDistance = textdiff.MaxMyersDistance

type editScript = textdiff.Script

func diffSplitLines(s string) []string { return textdiff.SplitLines(s) }

func computeEditScript(oldLines, newLines []string) editScript {
	return textdiff.Compute(oldLines, newLines)
}

// unifiedDiff returns the response diff of oldContent → newContent: exact
// (a newline-only change renders, with the no-newline marker), capped at
// maxDiffLines, "" when there is no difference.
func unifiedDiff(path, oldContent, newContent string) string {
	if oldContent == newContent {
		return ""
	}
	return renderUnifiedDiff(path, textdiff.ComputeExact(oldContent, newContent))
}

// renderUnifiedDiff formats a pre-computed edit script as a response diff.
func renderUnifiedDiff(path string, script editScript) string {
	hunks := textdiff.Hunks(script, 3)
	if len(hunks) == 0 {
		return ""
	}
	out := []string{"--- a/" + path, "+++ b/" + path}
	total := len(out)
	truncated := false
	for _, h := range hunks {
		lines := textdiff.FormatHunk(h)
		if total+len(lines) > maxDiffLines {
			out = append(out, lines[:max(0, maxDiffLines-total)]...)
			truncated = true
			break
		}
		out = append(out, lines...)
		total += len(lines)
	}
	if truncated {
		out = append(out, diffTruncatedNote)
	}
	return strings.Join(out, "\n")
}
