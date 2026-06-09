package render_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/plumbkit/plumb/internal/render"
)

func TestLeaderRows(t *testing.T) {
	pairs := [][2]string{
		{"HeapAlloc", "12 MB"},
		{"HeapReleased", "1.5 GB"}, // widest label
		{"NumGC", "37"},
	}
	rows := render.LeaderRows(pairs)
	if len(rows) != len(pairs) {
		t.Fatalf("got %d rows, want %d", len(rows), len(pairs))
	}

	width := utf8.RuneCountInString(rows[0])
	for i, r := range rows {
		if w := utf8.RuneCountInString(r); w != width {
			t.Errorf("row %d width %d != %d (values not right-aligned): %q", i, w, width, r)
		}
		if !strings.Contains(r, "⣀") {
			t.Errorf("row %d has no leader dots: %q", i, r)
		}
	}

	// Labels left-aligned, values right-aligned at the common edge.
	if !strings.HasPrefix(rows[0], "HeapAlloc ⣀") {
		t.Errorf("first row = %q, want it to start with the label + leader", rows[0])
	}
	if !strings.HasSuffix(rows[0], " 12 MB") {
		t.Errorf("first row = %q, want it to end with the value", rows[0])
	}
	// The widest label/value pair keeps the minimum leader run.
	if !strings.Contains(rows[1], "HeapReleased ⣀⣀ 1.5 GB") {
		t.Errorf("widest row = %q, want exactly the minimum 2-dot leader", rows[1])
	}
}

func TestLeaderRowsEmpty(t *testing.T) {
	if got := render.LeaderRows(nil); len(got) != 0 {
		t.Fatalf("LeaderRows(nil) = %v, want empty", got)
	}
}
