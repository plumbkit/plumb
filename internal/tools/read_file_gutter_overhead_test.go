package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGutterOverhead measures the byte overhead the display line-number gutter
// adds, so the "will it cost too many tokens?" decision rests on numbers, not an
// estimate. It runs over this package's own Go sources (real code) plus a
// short-line worst case, logs the percentage, and asserts a sane ceiling. The
// numbers it prints are recorded in docs/internal/todo-to-review.md.
//
// Token overhead tracks byte overhead closely for ASCII source: the gutter adds
// one line-number token + one tab per line, and a line number is ~1 token, so
// the percentage below is a faithful proxy for the token cost.
func TestGutterOverhead(t *testing.T) {
	type sample struct {
		name string
		body string
	}
	// Real Go corpus: every .go source in this package (skip _test.go to keep it
	// to production code shapes).
	entries, _ := os.ReadDir(".")
	var goTotal, goGuttered int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(e.Name()))
		if err != nil {
			continue
		}
		goTotal += len(b)
		goGuttered += len(withLineGutter(string(b), 1))
	}
	if goTotal > 0 {
		pct := 100 * float64(goGuttered-goTotal) / float64(goTotal)
		t.Logf("real Go corpus (%d files, %d KiB): +%.1f%% bytes", countGoFiles(entries), goTotal/1024, pct)
		if pct > 20 {
			t.Errorf("Go-corpus gutter overhead %.1f%% exceeds the 20%% sanity ceiling", pct)
		}
	}

	// Synthetic profiles spanning realistic line-length regimes.
	samples := []sample{
		{"short lines (worst case ~8 chars/line)", strings.Repeat("x := 1\n", 1000)},
		{"typical code (~40 chars/line)", strings.Repeat("    result := computeSomething(input, opts)\n", 1000)},
		{"prose/markdown (~75 chars/line)", strings.Repeat("This is a sentence of documentation prose that wraps near eighty cols.\n", 1000)},
	}
	for _, s := range samples {
		got := len(withLineGutter(s.body, 1))
		pct := 100 * float64(got-len(s.body)) / float64(len(s.body))
		t.Logf("%-42s +%.1f%% bytes", s.name, pct)
	}
}

func countGoFiles(entries []os.DirEntry) int {
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			n++
		}
	}
	return n
}
