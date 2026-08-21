package tools

import (
	"strings"
	"testing"
)

// TestWithTruncationBanner_LeadsThePayload pins the placement, which is the
// whole point: the notice qualifies everything after it, so a reader that stops
// early still sees it. A trailing-only notice is what let a 119 KB
// topology_affected response read as complete when the row that mattered had
// been cut.
func TestWithTruncationBanner_LeadsThePayload(t *testing.T) {
	body := "result one\nresult two\n[truncated: max_results reached]"
	got := withTruncationBanner(body, "packages were cut at max_results=50.")

	if !strings.HasPrefix(got, truncationMarker) {
		t.Errorf("notice must be the first thing in the payload, got:\n%s", got)
	}
	if strings.Index(got, truncationMarker) > strings.Index(got, "result one") {
		t.Error("notice must precede the results it qualifies")
	}
	if !strings.Contains(got, "max_results=50") {
		t.Error("notice must name the parameter that returns the rest")
	}
	// The trailing marker is kept deliberately: the leading copy changes the
	// reading, the trailing one marks where the data actually stops.
	if !strings.HasSuffix(got, "[truncated: max_results reached]") {
		t.Error("the existing trailing marker must survive")
	}
}

// TestWithTruncationBanner_UntouchedWhenComplete keeps the banner off the
// overwhelmingly common path, so its presence is always information.
func TestWithTruncationBanner_UntouchedWhenComplete(t *testing.T) {
	body := "result one\nresult two"
	if got := withTruncationBanner(body, ""); got != body {
		t.Errorf("a complete payload must be returned unchanged, got:\n%s", got)
	}
	if strings.Contains(withTruncationBanner(body, ""), truncationMarker) {
		t.Error("a complete payload must never carry a truncation marker")
	}
}
