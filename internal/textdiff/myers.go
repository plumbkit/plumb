package textdiff

// MaxMyersDistance bounds the edit distance the exact algorithm will explore.
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
const MaxMyersDistance = 1500

// Compute runs Myers' O(ND) shortest-edit-script algorithm and
// returns the full edit script as a flat sequence of diffLines.
//
// Myers' algorithm builds a greedy forward pass (finding the furthest-
// reaching d-path on each diagonal k) and then backtracks through saved
// snapshots to reconstruct the exact edit sequence. Past MaxMyersDistance it
// gives up and describes the change as a whole-file replacement instead.
func Compute(oldLines, newLines []string) Script {
	n, m := len(oldLines), len(newLines)
	if n == 0 && m == 0 {
		return nil
	}
	maxD := n + m // worst-case edit distance
	bounded := false
	if maxD > MaxMyersDistance {
		maxD = MaxMyersDistance
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
func wholeFileEditScript(oldLines, newLines []string) Script {
	script := make(Script, 0, len(oldLines)+len(newLines))
	for _, l := range oldLines {
		script = append(script, Line{Op: '-', Text: l})
	}
	for _, l := range newLines {
		script = append(script, Line{Op: '+', Text: l})
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
func myersBacktrack(oldLines, newLines []string, n, m int, trace [][]int, endD, offset int) Script {
	x, y := n, m
	var script Script
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
				script = append(script, Line{Op: ' ', Text: oldLines[x]})
			}
			y-- // the actual insertion
			script = append(script, Line{Op: '+', Text: newLines[y]})
		} else {
			// Deletion from diagonal k-1: snake from (prevX+1, prevY) → (x, y).
			for x > prevX+1 && y > prevY && oldLines[x-1] == newLines[y-1] {
				x--
				y--
				script = append(script, Line{Op: ' ', Text: oldLines[x]})
			}
			x-- // the actual deletion
			script = append(script, Line{Op: '-', Text: oldLines[x]})
		}
		x, y = prevX, prevY
	}
	for x > 0 { // any remaining diagonal at the start is all common lines
		x--
		y--
		script = append(script, Line{Op: ' ', Text: oldLines[x]})
	}
	// Backtracking produces the script in reverse order; flip it.
	for i, j := 0, len(script)-1; i < j; i, j = i+1, j-1 {
		script[i], script[j] = script[j], script[i]
	}
	return script
}
