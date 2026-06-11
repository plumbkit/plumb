package tools

import (
	"fmt"
	"strings"
	"testing"
)

// numberedLines returns "L1\nL2\n…\nLn\n" — a deterministic multi-line file body
// for exercising the line-change summary.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	return b.String()
}

func TestSummariseLineChanges(t *testing.T) {
	tests := []struct {
		name           string
		before, after  string
		want           string
		wantNoMoreTail bool // assert the "…(+N more)" tail is absent
	}{
		{
			name:   "identical",
			before: "a\nb\nc\n",
			after:  "a\nb\nc\n",
			want:   "",
		},
		{
			// The regression: a small insertion near the top of a long file must
			// name only the inserted range, never every line below it.
			name:           "insertion near top of long file",
			before:         numberedLines(30),
			after:          "L1\nL2\nL3\nL4\nL5\nNEW-A\nNEW-B\n" + tailFrom(6, 30),
			want:           "lines changed: L6-7",
			wantNoMoreTail: true,
		},
		{
			name:   "single line modification",
			before: numberedLines(10),
			after:  strings.Replace(numberedLines(10), "L4\n", "CHANGED\n", 1),
			want:   "lines changed: L4",
		},
		{
			// A pure deletion has no new line of its own; report the position the
			// following content now occupies, not a downstream tail.
			name:   "pure deletion mid file",
			before: numberedLines(10),
			after:  numberedLines(4) + tailFrom(7, 10), // drop L5,L6
			want:   "lines changed: L5",
		},
		{
			name:   "replacement delete two add three",
			before: "a\nb\nc\nd\ne\n",
			after:  "a\nX\nY\nZ\ne\n", // b,c,d -> X,Y,Z
			want:   "lines changed: L2-4",
		},
		{
			// More than five separate change regions collapse with a count tail.
			name:           "more than five changes truncates",
			before:         numberedLines(20),
			after:          changeEveryOther(20),
			wantNoMoreTail: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summariseLineChanges(tt.before, tt.after)
			if tt.want != "" && got != tt.want {
				t.Errorf("summariseLineChanges = %q, want %q", got, tt.want)
			}
			if tt.wantNoMoreTail && strings.Contains(got, "more)") {
				t.Errorf("summary unexpectedly truncated with a tail: %q", got)
			}
			if tt.name == "more than five changes truncates" && !strings.Contains(got, "more)") {
				t.Errorf("expected a truncation tail for many changes, got %q", got)
			}
		})
	}
}

// tailFrom returns "L<from>\n…\nL<to>\n".
func tailFrom(from, to int) string {
	var b strings.Builder
	for i := from; i <= to; i++ {
		fmt.Fprintf(&b, "L%d\n", i)
	}
	return b.String()
}

// changeEveryOther rewrites every even-numbered line so the diff has many
// separate single-line change regions.
func changeEveryOther(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i%2 == 0 {
			fmt.Fprintf(&b, "CH%d\n", i)
		} else {
			fmt.Fprintf(&b, "L%d\n", i)
		}
	}
	return b.String()
}

// TestSummariseLineChanges_MatchesDiffNumbering asserts the summary's line
// numbers agree with the unified diff's new-file hunk start for a trailing-
// newline file — the two outputs ship in the same response and must not disagree.
func TestSummariseLineChanges_MatchesDiffNumbering(t *testing.T) {
	before := numberedLines(12)
	after := strings.Replace(before, "L7\n", "CHANGED\n", 1)

	summary := summariseLineChanges(before, after)
	if summary != "lines changed: L7" {
		t.Fatalf("summary = %q, want %q", summary, "lines changed: L7")
	}
	// The hunk header opens on the first context line (4), three lines of
	// context above the single change at line 7 of a 12-line file.
	diff := unifiedDiff("f.txt", before, after)
	if !strings.Contains(diff, "@@ -4,7 +4,7 @@") || !strings.Contains(diff, "+CHANGED") {
		t.Errorf("unified diff does not cover the change at new line 7:\n%s", diff)
	}
}
