package topology

import (
	"context"
	"testing"
)

// edgeSources returns the source of every edge in a neighbourhood, so an
// assertion can be about WHICH edges were served rather than how many.
func edgeSources(nb *Neighbourhood) map[string]int {
	out := map[string]int{}
	for _, e := range nb.Edges {
		out[e.Source]++
	}
	return out
}

// TestExplore_ExcludesDerivedCallEdgesUnlessAskedFor enforces the deliberate
// step-6 consumer rollout. The derived lifecycle is durable across incremental
// re-indexes, but the default remains source-filtered until each consumer has a
// measured before/after; without this filter, four shipped tools would receive
// resolver edges because they share kind = "calls" with extractor edges.
//
// Both directions are asserted against the SAME index in the SAME test, because
// a filter that also dropped the real intra-file `calls` edges would be a worse
// regression than the leak it fixes.
func TestExplore_ExcludesDerivedCallEdgesUnlessAskedFor(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)
	ctx := context.Background()

	// Non-vacuity: the index really does hold a derived edge leaving Run, so an
	// empty result below would mean the filter worked, not that there was nothing
	// to filter.
	var derived int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM topology_edges WHERE source = ? AND from_id = ?`,
		callResolverSource, f.run).Scan(&derived); err != nil {
		t.Fatal(err)
	}
	if derived == 0 {
		t.Fatal("no derived edge leaves Run; this test would pass over an empty table")
	}

	opts := ExploreOpts{
		Depth: 1, MaxNodes: 50, MaxBytes: 50000, IncludeSource: "none",
		EdgeKinds: []string{string(EdgeCalls)}, Direction: DirectionOutward,
	}

	nb, err := ExploreFrom(ctx, f.db, Node{ID: f.run, Name: "Run"}, opts)
	if err != nil {
		t.Fatalf("ExploreFrom: %v", err)
	}
	got := edgeSources(nb)
	if got[callResolverSource] != 0 {
		t.Errorf("a `calls` traversal served %d %s edges; derived calls require an explicit "+
			"measured step-6 rollout", got[callResolverSource], callResolverSource)
	}
	// The other direction: the extractor's intra-file call edge must survive the
	// filter untouched.
	if got["extractor"] != 1 {
		t.Errorf("extractor-emitted intra-file `calls` edges served = %d, want 1 — the source filter "+
			"must preserve extractor edges while derived consumers remain deliberately excluded; sources served: %v", got["extractor"], got)
	}

	// Opting in serves them, which is what step 6 flips per consumer.
	opts.IncludeDerivedCalls = true
	nbIn, err := ExploreFrom(ctx, f.db, Node{ID: f.run, Name: "Run"}, opts)
	if err != nil {
		t.Fatalf("ExploreFrom (opted in): %v", err)
	}
	gotIn := edgeSources(nbIn)
	if gotIn[callResolverSource] != derived {
		t.Errorf("with IncludeDerivedCalls the traversal served %d %s edges, want %d — the flag is inert",
			gotIn[callResolverSource], callResolverSource, derived)
	}
	if gotIn["extractor"] != got["extractor"] {
		t.Errorf("opting in changed the extractor edge count from %d to %d", got["extractor"], gotIn["extractor"])
	}
}

// TestImpact_DoesNotServeDerivedCallEdges covers the second entry point. Impact
// builds its own ExploreOpts internally, so a filter applied only at Explore's
// door would leave topology_impact, topology_affected and minimal_diff_review
// consuming the edges through ImpactFrom.
func TestImpact_DoesNotServeDerivedCallEdges(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)

	res, err := ImpactFrom(context.Background(), f.db, Node{ID: f.run, Name: "Run"},
		ImpactOpts{Depth: 2, MaxNodes: 200, MaxBytes: 100000, EdgeKinds: []string{string(EdgeCalls)}})
	if err != nil {
		t.Fatalf("ImpactFrom: %v", err)
	}
	for _, nb := range []*Neighbourhood{res.DependsOn, res.DependedOnBy} {
		if nb == nil {
			continue
		}
		if n := edgeSources(nb)[callResolverSource]; n != 0 {
			t.Errorf("an impact walk served %d %s edges", n, callResolverSource)
		}
	}
	if n := edgeSources(res.DependsOn)["extractor"]; n != 1 {
		t.Errorf("impact served %d extractor `calls` edges outward, want 1", n)
	}
}
