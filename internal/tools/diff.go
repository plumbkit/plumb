package tools

// diff.go — minimal unified diff generator for edit_file and write_file responses.
//
// Implements the Myers O(ND) diff algorithm. Used to produce a compact change
// summary in write-tool responses so the calling agent can verify what changed
// without a follow-up read_file call.

import (
	"fmt"
	"strings"
)

// maxDiffLines is the maximum number of diff output lines (header + hunks)
// included in a write-tool response. Diffs that exceed this are truncated.
const maxDiffLines = 80

type diffLine struct {
	kind byte // ' ' common, '-' delete, '+' add
	text string
}

// unifiedDiff returns a unified diff comparing oldContent to newContent.
// path is used only in the header (--- a/path, +++ b/path). Returns "" when
// there is no difference. Output is capped at maxDiffLines; a truncation note
// is appended when the full diff would be longer.
func unifiedDiff(path, oldContent, newContent string) string {
	if oldContent == newContent {
		return ""
	}
	script := computeEditScript(diffSplitLines(oldContent), diffSplitLines(newContent))
	return renderUnifiedDiff(path, script)
}

// renderUnifiedDiff formats a pre-computed edit script as a unified diff. Split
// from unifiedDiff so a caller that already has the script (formatEditFileSuccess,
// which also feeds it to summariseEditScript) renders without a second Myers pass.
func renderUnifiedDiff(path string, script editScript) string {
	hunks := groupHunks(script, 3)
	if len(hunks) == 0 {
		return ""
	}

	var out []string
	out = append(out, "--- a/"+path)
	out = append(out, "+++ b/"+path)
	total := len(out)
	truncated := false
	for _, h := range hunks {
		lines := formatHunk(h)
		if total+len(lines) > maxDiffLines {
			out = append(out, lines[:max(0, maxDiffLines-total)]...)
			truncated = true
			break
		}
		out = append(out, lines...)
		total += len(lines)
	}
	if truncated {
		out = append(out, "… (diff truncated; use file_diff for the full view)")
	}
	return strings.Join(out, "\n")
}

func diffSplitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	// If the string ends with a newline, Split produces a spurious empty last
	// element. Remove it so line counts are accurate.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// editScript is a sequence of diffLines encoding the shortest edit script.
type editScript []diffLine

// maxMyersDistance bounds the edit distance the exact algorithm will explore.
//
// The forward pass keeps one trace snapshot per round, each of length
// 2*maxD+1 — so memory and time are O(D²) in the edit distance, unbounded by
// anything else. With maxD left at n+m, a large range-mode replacement in a
// multi-thousand-line file allocated on the order of 10⁷ ints (~80 MB of
// transient garbage) inside a single edit_file call, and it ran on EVERY edit:
// show_write_diff gates only the rendering, because summariseEditScript needs
// the same script.
//
// Beyond this distance the exact script buys nothing a caller can see: the diff
// is truncated at maxDiffLines (80) and the summary collapses after five ranges,
// so an edit that large is already reported in aggregate.
const maxMyersDistance = 1500

// computeEditScript runs Myers' O(ND) shortest-edit-script algorithm and
// returns the full edit script as a flat sequence of diffLines.
//
// Myers' algorithm builds a greedy forward pass (finding the furthest-
// reaching d-path on each diagonal k) and then backtracks through saved
// snapshots to reconstruct the exact edit sequence. Past maxMyersDistance it
// gives up and describes the change as a whole-file replacement instead.
func computeEditScript(oldLines, newLines []string) editScript {
	n, m := len(oldLines), len(newLines)
	if n == 0 && m == 0 {
		return nil
	}
	maxD := n + m // worst-case edit distance
	bounded := false
	if maxD > maxMyersDistance {
		maxD = maxMyersDistance
		bounded = true
	}
	offset := maxD // offset so index k+offset is always ≥0
	trace, endD, found := myersForward(oldLines, newLines, n, m, maxD, offset)
	if !found {
		if bounded {
			// The real distance exceeds the budget. Fall back to the coarse
			// whole-file script rather than spending O(D²) to describe an edit
			// whose rendering is capped anyway.
			return wholeFileEditScript(oldLines, newLines)
		}
		return nil // should never happen for finite inputs within budget
	}
	return myersBacktrack(oldLines, newLines, n, m, trace, endD, offset)
}

// wholeFileEditScript describes the change as a full replacement: every old line
// removed, every new line added. It is a valid editScript, so the summary and
// unified-diff renderers need no special case.
func wholeFileEditScript(oldLines, newLines []string) editScript {
	script := make(editScript, 0, len(oldLines)+len(newLines))
	for _, l := range oldLines {
		script = append(script, diffLine{kind: '-', text: l})
	}
	for _, l := range newLines {
		script = append(script, diffLine{kind: '+', text: l})
	}
	return script
}

// myersForward runs the greedy forward pass of Myers' O(ND) algorithm.
// Returns trace snapshots (v array before each round), the edit distance
// endD, and whether a solution was reached.
func myersForward(oldLines, newLines []string, n, m, maxD, offset int) (trace [][]int, endD int, found bool) {
	v := make([]int, 2*maxD+1)
	trace = make([][]int, 0, maxD+1)
outer:
	for d := 0; d <= maxD; d++ {
		s := make([]int, len(v))
		copy(s, v)
		trace = append(trace, s)
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[k-1+offset] < v[k+1+offset]) {
				x = v[k+1+offset] // insert: move down on diagonal k+1
			} else {
				x = v[k-1+offset] + 1 // delete: move right on diagonal k-1
			}
			y := x - k
			for x < n && y < m && oldLines[x] == newLines[y] { // extend along diagonal
				x++
				y++
			}
			v[k+offset] = x
			if x == n && y == m {
				endD = d
				found = true
				break outer
			}
		}
	}
	return trace, endD, found
}

// myersBacktrack reconstructs the edit script from the trace snapshots
// produced by myersForward. Returns the script in source order (forward).
func myersBacktrack(oldLines, newLines []string, n, m int, trace [][]int, endD, offset int) editScript {
	x, y := n, m
	var script editScript
	for d := endD; d > 0; d-- {
		vPrev := trace[d] // state of v before round d (= after round d-1)
		k := x - y
		var prevK int
		if k == -d || (k != d && vPrev[k-1+offset] < vPrev[k+1+offset]) {
			prevK = k + 1 // insertion (y-step) from diagonal k+1
		} else {
			prevK = k - 1 // deletion (x-step) from diagonal k-1
		}
		prevX := vPrev[prevK+offset]
		prevY := prevX - prevK
		if prevK == k+1 {
			// Insertion from diagonal k+1: snake from (prevX, prevY+1) → (x, y).
			for x > prevX && y > prevY+1 && oldLines[x-1] == newLines[y-1] {
				x--
				y--
				script = append(script, diffLine{' ', oldLines[x]})
			}
			y-- // the actual insertion
			script = append(script, diffLine{'+', newLines[y]})
		} else {
			// Deletion from diagonal k-1: snake from (prevX+1, prevY) → (x, y).
			for x > prevX+1 && y > prevY && oldLines[x-1] == newLines[y-1] {
				x--
				y--
				script = append(script, diffLine{' ', oldLines[x]})
			}
			x-- // the actual deletion
			script = append(script, diffLine{'-', oldLines[x]})
		}
		x, y = prevX, prevY
	}
	for x > 0 { // any remaining diagonal at the start is all common lines
		x--
		y--
		script = append(script, diffLine{' ', oldLines[x]})
	}
	// Backtracking produces the script in reverse order; flip it.
	for i, j := 0, len(script)-1; i < j; i, j = i+1, j-1 {
		script[i], script[j] = script[j], script[i]
	}
	return script
}

// hunk groups a contiguous changed region with surrounding context lines.
type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []diffLine
}

// groupHunks converts a flat edit script into hunks with ctx lines of context.
func groupHunks(script editScript, ctx int) []hunk {
	if len(script) == 0 {
		return nil
	}
	var hunks []hunk
	i := 0
	oldLine, newLine := 1, 1

	for i < len(script) {
		for i < len(script) && script[i].kind == ' ' {
			oldLine++
			newLine++
			i++
		}
		if i >= len(script) {
			break
		}
		ctxStart := max(0, i-ctx)
		ctxBack := i - ctxStart
		h := hunk{
			oldStart: oldLine - ctxBack,
			newStart: newLine - ctxBack,
		}
		for j := ctxStart; j < i; j++ {
			h.lines = append(h.lines, script[j])
			h.oldCount++
			h.newCount++
		}
		i = collectHunkBody(script, i, ctx, &h, &oldLine, &newLine)
		hunks = append(hunks, h)
	}
	return hunks
}

// collectHunkBody appends lines to h until the trailing common-line run
// reaches ctx length and the next line (if any) is also common.
// Returns the updated index into script.
func collectHunkBody(script editScript, i, ctx int, h *hunk, oldLine, newLine *int) int {
	for i < len(script) {
		dl := script[i]
		h.lines = append(h.lines, dl)
		switch dl.kind {
		case ' ':
			*oldLine++
			*newLine++
			h.oldCount++
			h.newCount++
		case '-':
			*oldLine++
			h.oldCount++
		case '+':
			*newLine++
			h.newCount++
		}
		i++
		if countTrailingCommon(h.lines) >= ctx && (i >= len(script) || script[i].kind == ' ') {
			break
		}
	}
	return i
}

// countTrailingCommon returns the number of trailing common (space-kind) lines.
func countTrailingCommon(lines []diffLine) int {
	n := 0
	for j := len(lines) - 1; j >= 0 && lines[j].kind == ' '; j-- {
		n++
	}
	return n
}

func formatHunk(h hunk) []string {
	header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", h.oldStart, h.oldCount, h.newStart, h.newCount)
	out := make([]string, 0, 1+len(h.lines))
	out = append(out, header)
	for _, dl := range h.lines {
		out = append(out, string(dl.kind)+dl.text)
	}
	return out
}
