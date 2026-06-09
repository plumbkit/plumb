package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// seedTwoSameName builds an index with one function named "Target" in each of
// two files, each owning a distinct child via a calls edge, so a traversal that
// starts from the wrong node is detectable by which child it reaches.
func seedTwoSameName(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "resolve.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	fileA := insertTestFile(t, db, "a/foo.go")
	fileB := insertTestFile(t, db, "b/foo.go")
	aID := insertTestNode(t, db, fileA, "a/foo.go", Node{Kind: KindFunction, Name: "Target", Language: "go"})
	bID := insertTestNode(t, db, fileB, "b/foo.go", Node{Kind: KindFunction, Name: "Target", Language: "go"})
	aChild := insertTestNode(t, db, fileA, "a/foo.go", Node{Kind: KindFunction, Name: "AChild", Language: "go"})
	bChild := insertTestNode(t, db, fileB, "b/foo.go", Node{Kind: KindFunction, Name: "BChild", Language: "go"})
	insertTestEdge(t, db, aID, aChild, string(EdgeCalls))
	insertTestEdge(t, db, bID, bChild, string(EdgeCalls))
	return db
}

func TestResolveNodes_AmbiguousDeterministicOrder(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()

	cands, err := ResolveNodes(context.Background(), db, "Target", NodeHint{})
	if err != nil {
		t.Fatalf("ResolveNodes: %v", err)
	}
	if len(cands) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(cands))
	}
	// Ordered by path: a/foo.go before b/foo.go.
	if cands[0].Path != "a/foo.go" || cands[1].Path != "b/foo.go" {
		t.Errorf("non-deterministic order: got %q then %q", cands[0].Path, cands[1].Path)
	}
}

func TestResolveNodes_PathHintSelects(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()

	cands, err := ResolveNodes(context.Background(), db, "Target", NodeHint{PathSubstr: "b/"})
	if err != nil {
		t.Fatalf("ResolveNodes: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("path hint should select exactly 1, got %d", len(cands))
	}
	if cands[0].Path != "b/foo.go" {
		t.Errorf("path hint selected wrong node: %q", cands[0].Path)
	}
}

func TestResolveNodes_UnmatchedHintIgnored(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()

	// A kind that matches nothing must not turn a real symbol into a miss.
	cands, err := ResolveNodes(context.Background(), db, "Target", NodeHint{Kind: string(KindMethod)})
	if err != nil {
		t.Fatalf("ResolveNodes: %v", err)
	}
	if len(cands) != 2 {
		t.Errorf("unmatched hint should be ignored, leaving 2 candidates; got %d", len(cands))
	}
}

func TestResolveNodes_UnknownSymbol(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()

	cands, err := ResolveNodes(context.Background(), db, "Nope", NodeHint{})
	if err != nil {
		t.Fatalf("ResolveNodes: %v", err)
	}
	if len(cands) != 0 {
		t.Errorf("expected no candidates for unknown symbol, got %d", len(cands))
	}
}

// TestExploreFrom_StartsAtChosenNode proves the disambiguation actually changes
// the traversal: starting from the b/foo.go "Target" reaches BChild, never
// AChild (the bug was that a name-keyed BFS could follow either).
func TestExploreFrom_StartsAtChosenNode(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()

	cands, err := ResolveNodes(context.Background(), db, "Target", NodeHint{PathSubstr: "b/"})
	if err != nil {
		t.Fatalf("ResolveNodes: %v", err)
	}
	nb, err := ExploreFrom(context.Background(), db, cands[0], ExploreOpts{Depth: 1, MaxNodes: 50, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("ExploreFrom: %v", err)
	}
	names := map[string]bool{}
	for _, n := range nb.Nodes {
		names[n.Name] = true
	}
	if !names["BChild"] {
		t.Error("expected BChild in neighbourhood of the b/foo.go Target")
	}
	if names["AChild"] {
		t.Error("AChild must not appear — traversal started at the wrong node")
	}
}

func TestImpactFrom_StartsAtChosenNode(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()

	cands, err := ResolveNodes(context.Background(), db, "Target", NodeHint{PathSubstr: "a/"})
	if err != nil {
		t.Fatalf("ResolveNodes: %v", err)
	}
	res, err := ImpactFrom(context.Background(), db, cands[0], ImpactOpts{Depth: 2, MaxNodes: 50, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("ImpactFrom: %v", err)
	}
	if res.Centre.Path != "a/foo.go" {
		t.Errorf("impact centre is the wrong node: %q", res.Centre.Path)
	}
	for _, n := range res.DependsOn.Nodes {
		if n.Name == "BChild" {
			t.Error("BChild reached from the a/foo.go Target — wrong start node")
		}
	}
}
