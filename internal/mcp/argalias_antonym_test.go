package mcp

import (
	"strings"
	"testing"
)

// TestClosest_NeverSuggestsTheOppositeParameter is the regression test for a
// hint that was worse than no hint.
//
// search_in_files declares both `glob` (the include filter) and `exclude`.
// `include` is nearer to `exclude` by edit distance than to anything else, so
// the rejection read `unknown parameter "include"; did you mean "exclude"?` —
// 12 such calls in the stats DB. An agent that took the advice would have
// searched with the INVERSE filter and got a confidently wrong answer rather
// than an error.
//
// The alias table's stated rule is "never a semantic flip (include≠exclude)".
// It was enforced on the paths that REWRITE a call and missing on the path that
// ADVISES one.
func TestClosest_NeverSuggestsTheOppositeParameter(t *testing.T) {
	params := []string{"pattern", "use_regex", "path", "glob", "exclude", "case_sensitive"}
	if got := closest("include", params); got == "exclude" {
		t.Errorf(`closest("include") = %q — suggesting the inverse filter is worse than no hint`, got)
	}

	cases := []struct{ key, cand string }{
		{"include_hidden", "exclude_hidden"},
		{"start_line", "end_line"},
		{"min_results", "max_results"},
		{"enabled", "disabled"},
		{"before_context", "after_context"},
		{"allow_dir", "deny_dir"},
		{"old_string", "new_string"},
	}
	for _, c := range cases {
		if !invertsMeaning(c.key, c.cand) {
			t.Errorf("invertsMeaning(%q, %q) = false; suggesting it would swap the meaning", c.key, c.cand)
		}
	}
}

// The guard must not become so broad that it suppresses useful hints. Antonyms
// are matched as WHOLE TOKENS: substring matching reads "append" as containing
// "end", "threshold" as containing "old", and "line" as containing "in".
func TestInvertsMeaning_TokensNotSubstrings(t *testing.T) {
	cases := []struct{ key, cand string }{
		{"append_mode", "start_line"}, // "append" is not the token "end"
		{"threshold", "new_string"},   // "threshold" is not the token "old"
		{"line_count", "out_dir"},     // "line" is not the token "in"
		{"maximum", "max_results"},    // same side of a pair, not opposite
		{"naem", "name"},              // an ordinary typo must still be hinted
	}
	for _, c := range cases {
		if invertsMeaning(c.key, c.cand) {
			t.Errorf("invertsMeaning(%q, %q) = true; a useful suggestion was suppressed", c.key, c.cand)
		}
	}
	if got := closest("naem", []string{"name", "path"}); got != "name" {
		t.Errorf(`closest("naem") = %q, want "name" — ordinary typo hints must survive`, got)
	}
}

// A name carrying BOTH sides of a pair is not the opposite of anything.
func TestInvertsMeaning_BothStemsIsNotAnInversion(t *testing.T) {
	if invertsMeaning("include_exclude_mode", "exclude") {
		t.Error("a name carrying both stems must not count as an inversion")
	}
}

// TestNameTokens_SplitsSeparatorsAndCamelCase pins the tokeniser the guard
// depends on.
func TestNameTokens_SplitsSeparatorsAndCamelCase(t *testing.T) {
	got := nameTokens("startLine_offset-max")
	for _, want := range []string{"start", "line", "offset", "max"} {
		if !got[want] {
			t.Errorf("nameTokens missing %q; got %v", want, keysOf(got))
		}
	}
	if got["startline"] {
		t.Errorf("camelCase was not split; got %v", keysOf(got))
	}
}

func keysOf(m map[string]bool) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}
