package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"github.com/plumbkit/plumb/internal/render"
)

// The truncate-left strategy itself now lives in internal/render (shared with
// the CLI's setup table); this pins that `path_style = "truncate-middle"`
// still routes to it, which is the part the TUI owns.
func TestContractPath_TruncateMiddleStyle(t *testing.T) {
	const p = "/home/someone/projects/deeply/nested/thing.json"
	got := contractPath(p, 20, "truncate-middle")
	if want := render.TruncatePathLeft(render.ContractPath(p), 20); got != want {
		t.Errorf("contractPath(..., \"truncate-middle\") = %q, want %q", got, want)
	}
	if lipgloss.Width(got) > 20 {
		t.Errorf("result is %d columns, over the 20 cap: %q", lipgloss.Width(got), got)
	}
}

func TestContractPathFull(t *testing.T) {
	cases := []struct {
		p    string
		n    int
		want string
	}{
		{"/short/path", 20, "/short/path"}, // fits
		{"/long/path/to/end", 10, "…/end"}, // ellipsis + last component (5 chars ≤ 10)
		{"/a/bcdefghij", 5, "…ghij"},       // last component too long: truncate it
	}
	for _, tc := range cases {
		if got := contractPathFull(tc.p, tc.n); got != tc.want {
			t.Errorf("contractPathFull(%q, %d) = %q, want %q", tc.p, tc.n, got, tc.want)
		}
	}
}

func TestContractPathCompact(t *testing.T) {
	cases := []struct {
		p    string
		n    int
		want string
	}{
		{"/a/b/c/final", 20, "/a/b/c/final"},                                       // fits without change
		{"/Users/alice/Projects/final", 15, "/U/a/P/final"},                        // abbreviate intermediates
		{"~/Projects/long/final", 12, "~/P/l/final"},                               // tilde preserved as-is
		{"~/Projects/experiments/others/cve-explorer", 22, "~/P/e/o/cve-explorer"}, // real-world
		{"/aaaaa/bbbbb/ccccc/fin", 8, "…/fin"},                                     // compact too wide, use "…/last"
		{"/x/y/abcdefghij", 5, "…ghij"},                                            // last component overflows
	}
	for _, tc := range cases {
		if got := contractPathCompact(tc.p, tc.n); got != tc.want {
			t.Errorf("contractPathCompact(%q, %d) = %q, want %q", tc.p, tc.n, got, tc.want)
		}
	}
}

func TestPathStyleValue(t *testing.T) {
	if got := pathStyleValue(""); got != "compact" {
		t.Errorf("pathStyleValue(\"\") = %q, want \"compact\"", got)
	}
	if got := pathStyleValue("full"); got != "full" {
		t.Errorf("pathStyleValue(\"full\") = %q, want \"full\"", got)
	}
}

// TestWrapText_HardBreaksAnUnbreakableToken is issue #358's change 3: a
// whitespace-free token (a client-supplied path is the common case) used to
// pass through wrapText untouched no matter how long it was, producing a line
// wider than width that corrupted the caller's box (see
// TestDashAlertsWidget_LongPathNeverExceedsWidth for the widget-level version
// of this same assertion). Every line must fit, and no rune may be lost —
// concatenating the lines must reproduce the input exactly.
func TestWrapText_HardBreaksAnUnbreakableToken(t *testing.T) {
	input := strings.Repeat("a", 500)
	lines := wrapText(input, 40)
	if len(lines) == 0 {
		t.Fatal("wrapText returned no lines")
	}
	var rebuilt strings.Builder
	for _, l := range lines {
		if w := lipgloss.Width(l); w > 40 {
			t.Errorf("line %q has width %d, want <= 40", l, w)
		}
		rebuilt.WriteString(l)
	}
	if rebuilt.String() != input {
		t.Fatalf("hard-wrapped lines do not reconstruct the input: got %d runes, want %d",
			len([]rune(rebuilt.String())), len([]rune(input)))
	}
}

// TestWrapText_ShortTextStillWrapsOnWordBoundaries is the regression guard for
// the ordinary case: hardBreak must be a no-op for words that already fit, so
// normal prose keeps wrapping on spaces rather than being chopped mid-word.
func TestWrapText_ShortTextStillWrapsOnWordBoundaries(t *testing.T) {
	got := wrapText("the quick brown fox jumps", 12)
	want := []string{"the quick", "brown fox", "jumps"}
	if len(got) != len(want) {
		t.Fatalf("wrapText lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHardBreak_FitsUnchanged(t *testing.T) {
	if got := hardBreak("short", 40); len(got) != 1 || got[0] != "short" {
		t.Fatalf("hardBreak(\"short\", 40) = %v, want [\"short\"] unchanged", got)
	}
}
