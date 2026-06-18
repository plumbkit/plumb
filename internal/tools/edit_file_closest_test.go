package tools

import (
	"strings"
	"testing"
)

func TestClosestMatchDiff(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		searched      string
		wantEmpty     bool
		wantContains  []string
		wantNotInDiff []string
	}{
		{
			// The reported case: agent guessed the wrong indentation. The line is
			// present, only its leading whitespace differs — surface that drift.
			name:         "indentation drift surfaces a diff",
			content:      "func f() {\n\treturn 42\n}\n",
			searched:     "func f() {\n    return 42\n}\n",
			wantContains: []string{"closest match", "-    return 42", "+\treturn 42"},
		},
		{
			name:         "single token drift on one line",
			content:      "total := countItems()\n",
			searched:     "total := countItem()\n",
			wantContains: []string{"closest match", "-total := countItem()", "+total := countItems()"},
		},
		{
			// Completely unrelated snippet: no plausible region, stay silent so the
			// existing tiered guidance is not drowned out.
			name:      "unrelated snippet yields nothing",
			content:   "package main\n\nfunc main() {}\n",
			searched:  "the quick brown fox\njumps over\nthe lazy dog\n",
			wantEmpty: true,
		},
		{
			name:      "empty searched yields nothing",
			content:   "anything\n",
			searched:  "",
			wantEmpty: true,
		},
		{
			name:      "empty content yields nothing",
			content:   "",
			searched:  "something\n",
			wantEmpty: true,
		},
		{
			// One matching line out of three is below the similarity floor.
			name:      "below similarity threshold stays silent",
			content:   "shared line\nalpha\nbeta\ngamma\n",
			searched:  "shared line\nwholly\ndifferent\ncontent\nhere\n",
			wantEmpty: true,
		},
		{
			name:    "picks the nearest of several anchor matches",
			content: "x := 1\nfoo()\ny := 2\n\nx := 1\nbar()\nbaz()\nz := 3\n",
			// Aligns to the second block (foo→bar, plus baz) — choose the window
			// that shares the most lines, not merely the first anchor hit.
			searched:     "x := 1\nbar()\nqux()\nz := 3\n",
			wantContains: []string{"closest match", "+baz()"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := closestMatchDiff(tt.content, tt.searched, "f.go")
			if tt.wantEmpty {
				if got != "" {
					t.Fatalf("expected no diff, got:\n%s", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("expected a closest-match diff, got empty")
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("diff missing %q:\n%s", want, got)
				}
			}
			for _, no := range tt.wantNotInDiff {
				if strings.Contains(got, no) {
					t.Errorf("diff unexpectedly contains %q:\n%s", no, got)
				}
			}
		})
	}
}

func TestClosestCandidateStarts(t *testing.T) {
	old := []string{"alpha", "beta"}
	content := []string{"zero", "alpha", "beta", "gamma", "alpha", "beta"}

	starts := closestCandidateStarts(old, content)
	// "alpha" anchors at content indices 1 and 4 (start 1, 4); "beta" anchors at
	// 2 and 5 mapping to starts 1 and 4 — all dedup to {1, 4}.
	want := map[int]bool{1: true, 4: true}
	if len(starts) != len(want) {
		t.Fatalf("starts = %v, want keys %v", starts, want)
	}
	for _, s := range starts {
		if !want[s] {
			t.Errorf("unexpected start %d in %v", s, starts)
		}
	}
}

func TestClosestCandidateStarts_Capped(t *testing.T) {
	old := []string{"dup"}
	var content []string
	for i := 0; i < maxClosestCandidates*2; i++ {
		content = append(content, "dup")
	}
	if got := len(closestCandidateStarts(old, content)); got > maxClosestCandidates {
		t.Fatalf("candidate starts = %d, want <= %d", got, maxClosestCandidates)
	}
}

func TestLineWindowSimilarity(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want float64
	}{
		{"identical", []string{"a", "b"}, []string{"a", "b"}, 1.0},
		{"disjoint", []string{"a", "b"}, []string{"c", "d"}, 0.0},
		{"half", []string{"a", "b"}, []string{"a", "x"}, 0.5},
		{"empty a", nil, []string{"a"}, 0.0},
		{"empty b", []string{"a"}, nil, 0.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lineWindowSimilarity(tt.a, tt.b); got != tt.want {
				t.Errorf("lineWindowSimilarity = %v, want %v", got, tt.want)
			}
		})
	}
}
