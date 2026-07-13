package tools

import "testing"

func TestAppendXcodeHint(t *testing.T) {
	if got := appendXcodeHint("empty", "file:///App.swift", func(uri string) string {
		return " hint for " + uri
	}); got != "empty hint for file:///App.swift" {
		t.Fatalf("got %q", got)
	}
	if got := appendXcodeHint("empty", "", nil); got != "empty" {
		t.Fatalf("nil callback changed result: %q", got)
	}
}
