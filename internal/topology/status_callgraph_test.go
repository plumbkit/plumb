package topology

import (
	"strings"
	"testing"
)

// TestFormatStatus_DisclosesCallGraphReachWhereTheUserReadsIt pins the trade-off
// disclosure at the surface a user actually sees. A cross-file call graph that
// reaches a small minority of call sites is dangerous read as complete, so the
// reach share is stated in the tool response — not only in the docs.
func TestFormatStatus_DisclosesCallGraphReachWhereTheUserReadsIt(t *testing.T) {
	s := Status{CallGraph: CallGraphStatus{
		CallSites:          1000,
		QualifiedSites:     600,
		Resolved:           25,
		ResolvedNonTest:    10,
		UnresolvedReceiver: 500,
		ExternalPackage:    70,
		UnmatchedTarget:    5,
	}}
	out := FormatStatus(s, "/ws")

	// The reach share must be computed against EVERY call site, not against the
	// qualified subset: 25/600 would read as 4.2% and flatter the feature.
	if !strings.Contains(out, "2.5%") {
		t.Errorf("reach share missing or computed against the wrong denominator:\n%s", out)
	}
	for _, want := range []string{
		"25 cross-file call edges", "10 from non-test callers",
		"500", "absent, not caller-free", "Go only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status is missing %q:\n%s", want, out)
		}
	}
}

// TestFormatStatus_SaysNothingWhenNoCallSitesWereRecorded keeps the disclosure
// honest in the other direction: a workspace with no recorded call sites has no
// reach to report, and printing "0.0% reached" would read as a finding about the
// user's code rather than about the index.
func TestFormatStatus_SaysNothingWhenNoCallSitesWereRecorded(t *testing.T) {
	out := FormatStatus(Status{}, "/ws")
	if strings.Contains(out, "call graph:") {
		t.Errorf("a status with no call sites still printed a call-graph line:\n%s", out)
	}
}

// TestFormatStatus_ReachSharePercentageTracksTheCounts is the relationship
// assertion behind the literal above: the printed share must move with the
// numbers, so a hard-coded string cannot satisfy it.
func TestFormatStatus_ReachSharePercentageTracksTheCounts(t *testing.T) {
	low := FormatStatus(Status{CallGraph: CallGraphStatus{CallSites: 1000, Resolved: 25}}, "/ws")
	high := FormatStatus(Status{CallGraph: CallGraphStatus{CallSites: 1000, Resolved: 250}}, "/ws")
	if !strings.Contains(low, "2.5%") || !strings.Contains(high, "25.0%") {
		t.Errorf("share does not track the counts:\nlow:\n%s\nhigh:\n%s", low, high)
	}
}
