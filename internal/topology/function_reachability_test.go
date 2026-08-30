package topology

import (
	"context"
	"testing"
)

func TestReachableFunctions_FullClosurePathAndNoOverreach(t *testing.T) {
	nodes := map[int64]Node{}
	for i := int64(1); i <= 7; i++ {
		nodes[i] = Node{ID: i, Kind: KindFunction, Name: string(rune('A' + i - 1)), Path: "pkg/" + string(rune('a'+i-1)) + ".go", StartLine: int(i), Language: "go"}
	}
	g := &FunctionGraph{
		Nodes: nodes,
		Edges: map[int64][]int64{
			1: {2}, 2: {3}, 3: {4}, 4: {5}, 5: {6},
		},
	}
	res := ReachableFunctions(g, []int64{1})
	if len(res.Reachable) != 6 {
		t.Fatalf("reachable=%d, want 6", len(res.Reachable))
	}
	if res.Reachable[7] {
		t.Fatal("unconnected function was over-reached")
	}
	chain, ok := FunctionPathTo(g, res, 6)
	if !ok || len(chain) != 6 || chain[0].Name != "A" || chain[len(chain)-1].Name != "F" {
		t.Fatalf("path=%v, ok=%v; want A through F", chain, ok)
	}
	if _, ok := FunctionPathTo(g, res, 7); ok {
		t.Fatal("unreachable target fabricated a path")
	}
}

func TestCondenseFunctionSCCs_FlagsRecursiveCycleAndLayers(t *testing.T) {
	nodes := map[int64]Node{
		1: {ID: 1, Kind: KindFunction, Name: "main", Path: "cmd/main.go", StartLine: 1},
		2: {ID: 2, Kind: KindFunction, Name: "a", Path: "pkg/a.go", StartLine: 1},
		3: {ID: 3, Kind: KindFunction, Name: "b", Path: "pkg/b.go", StartLine: 1},
	}
	g := &FunctionGraph{
		Nodes: nodes,
		Edges: map[int64][]int64{1: {2}, 2: {3}, 3: {2}},
	}
	res := ReachableFunctions(g, []int64{1})
	sccs := CondenseFunctionSCCs(g, res.Reachable)
	if len(sccs) != 2 {
		t.Fatalf("SCCs=%d, want 2", len(sccs))
	}
	var found bool
	for _, s := range sccs {
		if len(s.Nodes) == 2 && s.Cycle {
			found = true
			if s.Nodes[0].Name != "a" || s.Nodes[1].Name != "b" {
				t.Fatalf("cycle nodes=%v, want deterministic a,b", s.Nodes)
			}
		}
	}
	if !found {
		t.Fatal("recursive SCC was not flagged")
	}
}

func TestLoadFunctionGraph_ExcludesTestCallersAndKeepsDerivedEdges(t *testing.T) {
	f := newResolverFixture(t)
	f.resolve(t)
	g, err := LoadFunctionGraph(context.Background(), f.db, "go", true)
	if err != nil {
		t.Fatalf("LoadFunctionGraph: %v", err)
	}
	if _, ok := g.Nodes[f.runTest]; ok {
		t.Fatal("test-file caller leaked into production graph")
	}
	if !containsFunctionEdge(g.Edges[f.run], f.alphaDo) {
		t.Fatal("production cross-file call-resolver edge was not admitted")
	}
	if !containsFunctionEdge(g.Edges[f.run], f.helper) {
		t.Fatal("production intra-file extractor edge was dropped")
	}
}

func containsFunctionEdge(edges []int64, want int64) bool {
	for _, id := range edges {
		if id == want {
			return true
		}
	}
	return false
}
