package tools

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFirstLine_IsRuneSafeAndBudgeted guards the display truncations that echo
// arbitrary workspace content back to the agent.
//
// firstLine, the file_outline signature/doc-headline caps, the find_references
// line preview, the topology docstring, and the embedding payload were all a
// plain s[:N] against a byte budget — which emits a replacement character
// whenever N lands inside a multi-byte sequence. That is not an edge case: it
// happens on any file with an accented word, a CJK comment, or an emoji in a
// string literal, and the corrupted text goes straight into the tool response
// (or, for the embedding payload, into a JSON request body some providers
// reject as invalid UTF-8).
//
// They now all delegate to textfmt.ClampBytes / textfmt.TruncateBytes, whose
// own tests sweep every budget against multi-byte input. firstLine is the one
// that is directly callable as a unit, so it stands in for the group here
// rather than duplicating that sweep five times.
func TestFirstLine_IsRuneSafeAndBudgeted(t *testing.T) {
	inputs := []string{
		strings.Repeat("a", 300),
		strings.Repeat("é", 300),       // 2 bytes: an odd budget always cuts one in half
		strings.Repeat("日", 300),       // 3 bytes
		strings.Repeat("🌍", 100),       // 4 bytes
		"x" + strings.Repeat("日", 300), // leading ASCII shifts every boundary off the budget
		"ascii prefix then " + strings.Repeat("ü", 200),
		"short line",
		"",
	}
	for _, in := range inputs {
		got := firstLine(in)
		if !utf8.ValidString(got) {
			t.Errorf("firstLine(%.12q…) produced invalid UTF-8: %q", in, got)
		}
		if len(got) > 120 {
			t.Errorf("firstLine(%.12q…) = %d bytes, over the 120-byte budget", in, len(got))
		}
	}
}

// TestFirstLine_PreservesShortInput checks the fix did not turn a no-op into a
// truncation: content inside the budget must come back byte-identical.
func TestFirstLine_PreservesShortInput(t *testing.T) {
	for _, in := range []string{"package main", "// héllo wörld", "日本語のコメント", "🌍 emoji line"} {
		if got := firstLine(in); got != in {
			t.Errorf("firstLine(%q) = %q, want it unchanged", in, got)
		}
	}
}
