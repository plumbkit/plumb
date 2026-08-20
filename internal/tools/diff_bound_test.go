package tools

import (
	"runtime"
	"strings"
	"testing"
)

// TestComputeEditScript_BoundedFallsBackToWholeFile pins the Myers bound. Past
// maxMyersDistance the exact script is abandoned for a whole-file replacement,
// which must still be a VALID script: the renderers and the summary take it
// without a special case.
func TestComputeEditScript_BoundedFallsBackToWholeFile(t *testing.T) {
	// The inputs share a COMMON line at each end. That is what makes this test
	// able to tell the fallback apart from an exact run: exact Myers would emit
	// those as ' ' context lines, whereas wholeFileEditScript emits none. Without
	// a shared line, an all-different input produces the same delete-then-add
	// shape either way, and the test passes with the bound short-circuited —
	// which an independent review caught it doing.
	n := maxMyersDistance + 200
	before := make([]string, 0, n+2)
	after := make([]string, 0, n+2)
	before = append(before, "shared header")
	after = append(after, "shared header")
	for range n {
		before = append(before, "old line")
		after = append(after, "new line")
	}
	before = append(before, "shared footer")
	after = append(after, "shared footer")

	script := computeEditScript(before, after)
	if len(script) == 0 {
		t.Fatal("a bounded computation must still return a usable script, not nil")
	}

	var dels, adds, common int
	for _, l := range script {
		switch l.kind {
		case '-':
			dels++
		case '+':
			adds++
		case ' ':
			common++
		}
	}
	// THE discriminator: an exact run would keep the two shared lines as context.
	if common != 0 {
		t.Errorf("expected the whole-file fallback (no common lines), got %d common lines — the bound did not fire", common)
	}
	if dels != len(before) || adds != len(after) {
		t.Errorf("whole-file fallback should remove %d and add %d lines, got -%d +%d", len(before), len(after), dels, adds)
	}

	// The summary must still render something meaningful, not panic or blank.
	if summary := summariseEditScript(script); summary == "" {
		t.Error("expected a non-empty summary from the fallback script")
	}
}

// TestComputeEditScript_ExactBelowBound guards that the bound did NOT change the
// common case: an edit well inside the budget must still produce the precise,
// minimal script rather than a whole-file replacement.
func TestComputeEditScript_ExactBelowBound(t *testing.T) {
	before := make([]string, 400)
	for i := range before {
		before[i] = "shared line"
	}
	after := append([]string(nil), before...)
	after[200] = "one changed line"

	script := computeEditScript(before, after)
	var changed int
	for _, l := range script {
		if l.kind != ' ' {
			changed++
		}
	}
	// A one-line substitution is one delete plus one add; anything near 800 means
	// it degraded to a whole-file replacement.
	if changed > 4 {
		t.Errorf("a small edit must stay exact, got %d changed lines in the script", changed)
	}
	if summary := summariseEditScript(script); !strings.Contains(summary, "L201") {
		t.Errorf("expected the precise changed line in the summary, got %q", summary)
	}
}

// TestComputeEditScript_BoundIsMemoryBounded is the regression test for the cost
// the bound exists to remove. The forward pass keeps one trace snapshot per
// round, each of length 2*maxD+1, so memory is O(D²) in the edit distance. Two
// sizes past the bound must cost about the SAME, not quadratically more —
// measured at 590 MB vs 37 MB per call for a 3000-line rewrite before the fix.
func TestComputeEditScript_BoundIsMemoryBounded(t *testing.T) {
	measure := func(n int) uint64 {
		before := make([]string, n)
		after := make([]string, n)
		for i := range before {
			before[i] = "old"
			after[i] = "new"
		}
		return allocBytesFor(func() { _ = computeEditScript(before, after) })
	}
	small := measure(maxMyersDistance + 100)
	large := measure((maxMyersDistance + 100) * 2)

	// Without the bound this ratio is quadratic (~4x for a doubled input).
	if large > small*2 {
		t.Errorf("allocation grew %dx when the input doubled (%d → %d bytes); the bound is not holding",
			large/max(small, 1), small, large)
	}
}

// allocBytesFor reports the bytes allocated while fn runs.
func allocBytesFor(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}
