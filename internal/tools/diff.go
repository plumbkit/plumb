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
	oldLines := diffSplitLines(oldContent)
	newLines := diffSplitLines(newContent)
	ops := computeEditScript(oldLines, newLines)
	hunks := groupHunks(ops, 3)
	if len(hunks) == 0 {
		return ""
	}

	var out []string
	out = append(out, fmt.Sprintf("--- a/%s", path))
	out = append(out, fmt.Sprintf("+++ b/%s", path))
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

// computeEditScript runs Myers' O(ND) shortest-edit-script algorithm and
// returns the full edit script as a flat sequence of diffLines.
//
// Myers' algorithm builds a greedy forward pass (finding the furthest-
// reaching d-path on each diagonal k) and then backtracks through saved
// snapshots to reconstruct the exact edit sequence.
func computeEditScript(old, new []string) editScript {
	n, m := len(old), len(new)
	if n == 0 && m == 0 {
		return nil
	}
	maxD := n + m  // worst-case edit distance
	offset := maxD // offset so index k+offset is always ≥0
	trace, endD, found := myersForward(old, new, n, m, maxD, offset)
	if !found {
		return nil // should never happen for finite inputs
	}
	return myersBacktrack(old, new, n, m, trace, endD, offset)
}

// myersForward runs the greedy forward pass of Myers' O(ND) algorithm.
// Returns trace snapshots (v array before each round), the edit distance
// endD, and whether a solution was reached.
func myersForward(old, new []string, n, m, maxD, offset int) (trace [][]int, endD int, found bool) {
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
			for x < n && y < m && old[x] == new[y] { // extend along diagonal
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
func myersBacktrack(old, new []string, n, m int, trace [][]int, endD, offset int) editScript {
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
			for x > prevX && y > prevY+1 && old[x-1] == new[y-1] {
				x--
				y--
				script = append(script, diffLine{' ', old[x]})
			}
			y-- // the actual insertion
			script = append(script, diffLine{'+', new[y]})
		} else {
			// Deletion from diagonal k-1: snake from (prevX+1, prevY) → (x, y).
			for x > prevX+1 && y > prevY && old[x-1] == new[y-1] {
				x--
				y--
				script = append(script, diffLine{' ', old[x]})
			}
			x-- // the actual deletion
			script = append(script, diffLine{'-', old[x]})
		}
		x, y = prevX, prevY
	}
	for x > 0 { // any remaining diagonal at the start is all common lines
		x--
		y--
		script = append(script, diffLine{' ', old[x]})
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
