package topology

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

func TestResolveNodes_UnmatchedHintReturnsMismatchError(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()

	// A hint that matches nothing must return a HintMismatchError naming the
	// candidates rather than silently selecting an unrelated node.
	cands, err := ResolveNodes(context.Background(), db, "Target", NodeHint{Kind: string(KindMethod)})
	if err == nil {
		t.Fatalf("expected HintMismatchError, got nil with %d candidates", len(cands))
	}
	var hintErr *HintMismatchError
	if !errors.As(err, &hintErr) {
		t.Fatalf("expected *HintMismatchError, got %T: %v", err, err)
	}
	if len(hintErr.Candidates) != 2 {
		t.Errorf("expected 2 candidates in HintMismatchError, got %d", len(hintErr.Candidates))
	}
	errMsg := hintErr.Error()
	if !strings.Contains(errMsg, `kind: "method"`) || !strings.Contains(errMsg, "Retry with") {
		t.Errorf("unexpected error message: %q", errMsg)
	}
}

func TestResolveNodes_ReceiverVariantsResolveSameNode(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "variants.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "pkg/search.go")
	insertTestNode(t, db, fileID, "pkg/search.go", Node{
		Kind:      KindMethod,
		Name:      "Execute",
		Qualified: "(*WorkspaceSearch).Execute",
		Language:  "go",
	})

	queries := []string{
		"WorkspaceSearch.Execute",
		"(*WorkspaceSearch).Execute",
		"(WorkspaceSearch).Execute",
		"*WorkspaceSearch.Execute",
		"WorkspaceSearch/Execute",
		"Execute",
	}

	for _, q := range queries {
		cands, err := ResolveNodes(context.Background(), db, q, NodeHint{})
		if err != nil {
			t.Fatalf("ResolveNodes(%q): %v", q, err)
		}
		if len(cands) != 1 {
			t.Fatalf("ResolveNodes(%q) want 1 match, got %d", q, len(cands))
		}
		if cands[0].Qualified != "(*WorkspaceSearch).Execute" {
			t.Errorf("ResolveNodes(%q) = %q, want (*WorkspaceSearch).Execute", q, cands[0].Qualified)
		}
	}
}

func TestTypeMembers_ExposesMethods(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "members.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "pkg/search.go")
	typeID := insertTestNode(t, db, fileID, "pkg/search.go", Node{
		Kind:      KindType,
		Name:      "WorkspaceSearch",
		Qualified: "WorkspaceSearch",
		Language:  "go",
	})
	insertTestNode(t, db, fileID, "pkg/search.go", Node{
		Kind:      KindMethod,
		Name:      "Execute",
		Qualified: "(*WorkspaceSearch).Execute",
		Language:  "go",
	})
	insertTestNode(t, db, fileID, "pkg/search.go", Node{
		Kind:      KindMethod,
		Name:      "Name",
		Qualified: "(*WorkspaceSearch).Name",
		Language:  "go",
	})

	typeNode := Node{ID: typeID, Kind: KindType, Name: "WorkspaceSearch", Path: "pkg/search.go"}
	members, err := TypeMembers(context.Background(), db, typeNode, 10)
	if err != nil {
		t.Fatalf("TypeMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].Qualified != "(*WorkspaceSearch).Execute" || members[1].Qualified != "(*WorkspaceSearch).Name" {
		t.Errorf("unexpected members: %+v", members)
	}
}

func TestTypeMembers_SamePackageScoping(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "twopkg.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Package A has type Store and method MethodA.
	fileA := insertTestFile(t, db, "pkgA/store.go")
	typeA := insertTestNode(t, db, fileA, "pkgA/store.go", Node{
		Kind:      KindType,
		Name:      "Store",
		Qualified: "Store",
		Language:  "go",
	})
	insertTestNode(t, db, fileA, "pkgA/store.go", Node{
		Kind:      KindMethod,
		Name:      "MethodA",
		Qualified: "(*Store).MethodA",
		Language:  "go",
	})

	// Package B also has a type Store and method MethodB.
	fileB := insertTestFile(t, db, "pkgB/store.go")
	typeB := insertTestNode(t, db, fileB, "pkgB/store.go", Node{
		Kind:      KindType,
		Name:      "Store",
		Qualified: "Store",
		Language:  "go",
	})
	insertTestNode(t, db, fileB, "pkgB/store.go", Node{
		Kind:      KindMethod,
		Name:      "MethodB",
		Qualified: "(*Store).MethodB",
		Language:  "go",
	})

	// A subpackage of A (pkgA/sub) is a different Go package with its own Store.
	// The SQL prefilter `f.path LIKE 'pkgA/%'` admits it, so only the exact
	// directory check keeps MethodSub out of pkgA's members.
	fileSub := insertTestFile(t, db, "pkgA/sub/store.go")
	insertTestNode(t, db, fileSub, "pkgA/sub/store.go", Node{
		Kind:      KindType,
		Name:      "Store",
		Qualified: "Store",
		Language:  "go",
	})
	insertTestNode(t, db, fileSub, "pkgA/sub/store.go", Node{
		Kind:      KindMethod,
		Name:      "MethodSub",
		Qualified: "(*Store).MethodSub",
		Language:  "go",
	})

	// Members of pkgA's Store must only contain MethodA, never MethodB from pkgB
	// or MethodSub from pkgA/sub.
	typeNodeA := Node{ID: typeA, Kind: KindType, Name: "Store", Path: "pkgA/store.go", FileID: fileA}
	membersA, err := TypeMembers(context.Background(), db, typeNodeA, 10)
	if err != nil {
		t.Fatalf("TypeMembers(pkgA): %v", err)
	}
	if len(membersA) != 1 {
		t.Fatalf("expected exactly 1 member for pkgA.Store, got %d: %+v", len(membersA), membersA)
	}
	if membersA[0].Qualified != "(*Store).MethodA" {
		t.Errorf("expected (*Store).MethodA, got %q", membersA[0].Qualified)
	}

	// Members of pkgB's Store must only contain MethodB, never MethodA from pkgA.
	typeNodeB := Node{ID: typeB, Kind: KindType, Name: "Store", Path: "pkgB/store.go", FileID: fileB}
	membersB, err := TypeMembers(context.Background(), db, typeNodeB, 10)
	if err != nil {
		t.Fatalf("TypeMembers(pkgB): %v", err)
	}
	if len(membersB) != 1 {
		t.Fatalf("expected exactly 1 member for pkgB.Store, got %d: %+v", len(membersB), membersB)
	}
	if membersB[0].Qualified != "(*Store).MethodB" {
		t.Errorf("expected (*Store).MethodB, got %q", membersB[0].Qualified)
	}
}

func TestExploreFrom_TypeMembersMaxBytesBudget(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "budget.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "pkg/service.go")
	typeID := insertTestNode(t, db, fileID, "pkg/service.go", Node{
		Kind:      KindType,
		Name:      "Service",
		Qualified: "Service",
		Language:  "go",
	})
	for i := range 20 {
		insertTestNode(t, db, fileID, "pkg/service.go", Node{
			Kind:      KindMethod,
			Name:      fmt.Sprintf("Method%d", i),
			Qualified: fmt.Sprintf("(*Service).Method%d", i),
			Language:  "go",
		})
	}

	typeNode := Node{ID: typeID, Kind: KindType, Name: "Service", Path: "pkg/service.go", FileID: fileID}
	nb, err := ExploreFrom(context.Background(), db, typeNode, ExploreOpts{
		Depth:    1,
		MaxNodes: 50,
		MaxBytes: 250,
	})
	if err != nil {
		t.Fatalf("ExploreFrom: %v", err)
	}
	if !nb.Truncated {
		t.Errorf("expected Truncated=true under tight MaxBytes budget")
	}
	if len(nb.Members) >= 20 {
		t.Errorf("expected truncated members under tight MaxBytes budget, got %d", len(nb.Members))
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
