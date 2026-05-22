package tui

import "testing"

func TestContractPathTruncateLeft(t *testing.T) {
	cases := []struct {
		p    string
		n    int
		want string
	}{
		{"abcde", 10, "abcde"},     // fits
		{"abcde", 5, "abcde"},      // fits exactly
		{"abcdefghij", 5, "…ghij"}, // maxW=5: "…" + last 4 = r[6:]
		{"ab", 1, "…"},             // maxW≤1 fallback
		{"a", 1, "a"},              // fits exactly at 1
		{"abc", 2, "…c"},           // maxW=2: "…" + last 1
	}
	for _, tc := range cases {
		if got := contractPathTruncateLeft(tc.p, tc.n); got != tc.want {
			t.Errorf("contractPathTruncateLeft(%q, %d) = %q, want %q", tc.p, tc.n, got, tc.want)
		}
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
