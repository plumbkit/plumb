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
		ExternalPackage:    60,
		UnmatchedTarget:    5,
		RepeatOfEdge:       8,
		NoCallerNode:       2,

		ResolvedSamePackage:  9,
		ResolvedReceiverSelf: 4,
		ResolvedParamTyped:   2,
		SameFile:             300,
		BareUnmatched:        100,
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
		// The two buckets a reader would otherwise have to discover by
		// subtracting and finding the breakdown short.
		"8 repeat", "2 sit outside any function",
		// The tier split: 25 resolved = 10 qualified + 9 bare + 4 receiver-self
		// + 2 typed-parameter, and the same-file share the extractor links.
		"10 package-qualified", "9 same-package bare calls", "4 calls on the method's own receiver",
		"2 on a typed parameter",
		"300 more (30.0%)", "100 bare calls naming no",
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

// TestCallGraphCensus_PublishesEveryBucketAndTheyReconcile is the reconciliation
// a reader of `topology_status` performs by hand: the buckets printed must add up
// to the call-site count printed beside them. It runs the REAL resolver, so it
// fails both when a bucket stops summing and when a bucket exists but is never
// published to topology_meta for the census to read.
func TestCallGraphCensus_PublishesEveryBucketAndTheyReconcile(t *testing.T) {
	f := newSamePackageFixture(t)
	f.resolve(t)

	c := callGraphCensus(f.db)
	sum := c.Resolved + c.UnresolvedReceiver + c.ExternalPackage +
		c.UnmatchedTarget + c.RepeatOfEdge + c.NoCallerNode + c.SameFile + c.BareUnmatched
	if sum != c.CallSites {
		t.Errorf("published buckets sum to %d against %d call sites (%+v); a reader who subtracts "+
			"gets a number the resolver never produced", sum, c.CallSites, c)
	}
	if c.CallSites == 0 {
		t.Fatal("the census read no call sites; the reconciliation above is vacuous")
	}
	// Every new bucket must be read back non-zero, or the sum above holds over a
	// fixture that cannot exercise it.
	if c.ResolvedSamePackage == 0 || c.ResolvedReceiverSelf == 0 || c.SameFile == 0 || c.BareUnmatched == 0 {
		t.Errorf("census did not publish the same-package buckets: %+v", c)
	}
	if c.RepeatOfEdge == 0 || c.NoCallerNode == 0 {
		t.Errorf("census did not publish the non-outcome buckets (repeat=%d, no-caller=%d); "+
			"the fixture exercises both, so a zero means the meta key is not being read",
			c.RepeatOfEdge, c.NoCallerNode)
	}
	if c.Resolved == 0 || c.Resolved >= c.CallSites {
		t.Errorf("resolved = %d against %d call sites; the reconciliation would hold trivially",
			c.Resolved, c.CallSites)
	}
}
