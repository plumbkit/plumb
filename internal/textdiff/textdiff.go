// Package textdiff computes line-based edit scripts (Myers' O(ND) algorithm)
// and renders and applies unified diffs. It is stdlib-only (foundation layer),
// so both the write tools' response rendering and the history store use one
// implementation.
//
// Concurrency: every function is pure and safe for concurrent use.
package textdiff

import "strings"

// Op is one edit-script operation, spelled as its unified-diff prefix.
type Op byte

// The three edit-script operations.
const (
	Equal  Op = ' '
	Delete Op = '-'
	Insert Op = '+'
)

// Line is one entry of an edit script. Text never contains '\n'. NoEOL marks
// a side's final line that has no terminating newline; only ComputeExact sets
// it.
type Line struct {
	Op    Op
	Text  string
	NoEOL bool
}

// Script is a full edit script, in order.
type Script []Line

// SplitLines splits s on '\n' and drops the empty element a trailing newline
// leaves. It is the legacy line model (final-newline state is lost), kept for
// callers that match line text rather than reproduce bytes.
func SplitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// ComputeExact diffs before and after so the script reproduces them byte for
// byte: lines are compared WITH their terminator, so "b\n" and "b" differ,
// and a missing final newline is carried as NoEOL.
func ComputeExact(before, after string) Script {
	raw := Compute(splitKeepEOL(before), splitKeepEOL(after))
	out := make(Script, len(raw))
	for i, l := range raw {
		text, hadEOL := strings.CutSuffix(l.Text, "\n")
		out[i] = Line{Op: l.Op, Text: text, NoEOL: !hadEOL}
	}
	return out
}

func splitKeepEOL(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// Counts returns how many lines the script inserts and deletes.
func Counts(s Script) (added, removed int) {
	for _, l := range s {
		switch l.Op {
		case Insert:
			added++
		case Delete:
			removed++
		}
	}
	return added, removed
}
