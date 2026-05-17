package tools

import (
	"strings"
	"testing"
)

func TestUnifiedDiff_NoChange(t *testing.T) {
	if d := unifiedDiff("f.go", "same\n", "same\n"); d != "" {
		t.Fatalf("expected empty diff for identical content, got:\n%s", d)
	}
}

func TestUnifiedDiff_SingleLineReplacement(t *testing.T) {
	old := "a\nb\nc\n"
	new := "a\nx\nc\n"
	d := unifiedDiff("f.go", old, new)
	if !strings.Contains(d, "-b") {
		t.Errorf("diff missing -b line:\n%s", d)
	}
	if !strings.Contains(d, "+x") {
		t.Errorf("diff missing +x line:\n%s", d)
	}
	if !strings.Contains(d, " a") {
		t.Errorf("diff missing context line ' a':\n%s", d)
	}
	if !strings.Contains(d, " c") {
		t.Errorf("diff missing context line ' c':\n%s", d)
	}
}

func TestUnifiedDiff_PureAddition(t *testing.T) {
	d := unifiedDiff("f.go", "a\nb\n", "a\nb\nc\n")
	if !strings.Contains(d, "+c") {
		t.Errorf("diff missing +c:\n%s", d)
	}
	// Check that no hunk lines start with '-' (the header "---" is expected).
	for _, line := range strings.Split(d, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			t.Errorf("diff should have no deletions, found: %q\n%s", line, d)
		}
	}
}

func TestUnifiedDiff_PureDeletion(t *testing.T) {
	d := unifiedDiff("f.go", "a\nb\nc\n", "a\nc\n")
	if !strings.Contains(d, "-b") {
		t.Errorf("diff missing -b:\n%s", d)
	}
	if strings.Contains(d, "+") && !strings.Contains(d, "+++") {
		t.Errorf("diff should have no insertions (besides header):\n%s", d)
	}
}

func TestUnifiedDiff_OldEmpty(t *testing.T) {
	d := unifiedDiff("f.go", "", "hello\n")
	if !strings.Contains(d, "+hello") {
		t.Errorf("expected +hello for empty→content diff:\n%s", d)
	}
}

func TestUnifiedDiff_NewEmpty(t *testing.T) {
	d := unifiedDiff("f.go", "hello\n", "")
	if !strings.Contains(d, "-hello") {
		t.Errorf("expected -hello for content→empty diff:\n%s", d)
	}
}

func TestUnifiedDiff_HeaderContainsPath(t *testing.T) {
	d := unifiedDiff("internal/foo/bar.go", "old\n", "new\n")
	if !strings.Contains(d, "internal/foo/bar.go") {
		t.Errorf("diff header missing path:\n%s", d)
	}
}

// TestUnifiedDiff_Truncation verifies that a very long diff is capped and
// includes a truncation notice rather than silently dropping output.
func TestUnifiedDiff_Truncation(t *testing.T) {
	// Build a file where every line differs.
	var sb strings.Builder
	for i := range maxDiffLines + 10 {
		sb.WriteString("old line\n")
		_ = i
	}
	old := sb.String()
	sb.Reset()
	for i := range maxDiffLines + 10 {
		sb.WriteString("new line\n")
		_ = i
	}
	new := sb.String()

	d := unifiedDiff("big.go", old, new)
	lines := strings.Split(d, "\n")
	if len(lines) > maxDiffLines+5 {
		t.Fatalf("diff has %d lines, expected ≤ %d+5", len(lines), maxDiffLines)
	}
	if !strings.Contains(d, "truncated") {
		t.Errorf("expected truncation notice in long diff:\n%s", d)
	}
}

// TestUnifiedDiff_NonContiguousChanges validates that two separated hunks
// each appear in the diff output with their own @@ header.
func TestUnifiedDiff_NonContiguousChanges(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\ng\nh\n"
	new := "a\nX\nc\nd\ne\nf\nY\nh\n" // changed b→X and g→Y, separated by 4 common lines
	d := unifiedDiff("f.go", old, new)

	hunks := strings.Count(d, "@@")
	if hunks < 2 {
		t.Errorf("expected ≥2 @@ hunks for non-contiguous changes, got %d:\n%s", hunks, d)
	}
	if !strings.Contains(d, "-b") || !strings.Contains(d, "+X") {
		t.Errorf("first change missing:\n%s", d)
	}
	if !strings.Contains(d, "-g") || !strings.Contains(d, "+Y") {
		t.Errorf("second change missing:\n%s", d)
	}
}

// TestComputeEditScript_BasicCorrectness is a white-box test that the edit
// script produced by Myers' algorithm is a valid transformation of old → new.
func TestComputeEditScript_BasicCorrectness(t *testing.T) {
	cases := []struct {
		old, new []string
	}{
		{nil, nil},
		{[]string{"a"}, []string{"a"}},
		{[]string{"a", "b"}, []string{"a", "x"}},
		{[]string{"a", "b", "c"}, []string{"a", "x", "c"}},
		{[]string{"a"}, []string{"a", "b", "c"}},
		{[]string{"a", "b", "c"}, []string{"a"}},
		{[]string{"x"}, []string{"y"}},
	}
	for _, tc := range cases {
		script := computeEditScript(tc.old, tc.new)
		// Replay the script and verify it reproduces new from old.
		got := replayScript(script)
		want := tc.new
		if len(got) != len(want) {
			t.Errorf("old=%v new=%v: replay produced %v, want %v", tc.old, tc.new, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("old=%v new=%v: replay[%d]=%q, want %q", tc.old, tc.new, i, got[i], want[i])
			}
		}
	}
}

// replayScript applies the edit script to reconstruct the 'new' side.
func replayScript(script editScript) []string {
	var out []string
	for _, dl := range script {
		if dl.kind == ' ' || dl.kind == '+' {
			out = append(out, dl.text)
		}
	}
	return out
}
