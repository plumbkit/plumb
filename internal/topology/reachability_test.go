package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// buildPackageEdge wires a two-hop chain: fromPkg -(imports)-> a fresh import
// node in fromFile -(imports)-> toPkg. This is exactly the shape linkImports
// produces (extractor edge pkg->import, resolver edge import->pkg) and is the
// only shape loadPackageEdges is supposed to fold into a directory edge.
func buildPackageEdge(t *testing.T, db *sql.DB, fromFile int64, fromRelPath string, fromPkg int64, toPkg int64) {
	t.Helper()
	imp := insertTestNode(t, db, fromFile, fromRelPath, Node{Kind: KindImport, Name: "x", Language: "go"})
	insertTestEdge(t, db, fromPkg, imp, string(EdgeImports))
	insertTestEdge(t, db, imp, toPkg, string(EdgeImports))
}

// TestReachabilityLoadPackageEdges_CrossDirectory pins the RECALL direction: a genuine
// two-hop pkg->import->pkg chain across two directories must produce a
// directory-level edge. A regression here (a missing edge) would silently
// under-report reachability — the worse of the two failure modes this card
// calls out.
func TestReachabilityLoadPackageEdges_CrossDirectory(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileA := insertTestFile(t, db, "internal/a/a.go")
	pkgA := insertTestNode(t, db, fileA, "internal/a/a.go", Node{Kind: KindPackage, Name: "a", Language: "go"})
	fileB := insertTestFile(t, db, "internal/b/b.go")
	pkgB := insertTestNode(t, db, fileB, "internal/b/b.go", Node{Kind: KindPackage, Name: "b", Language: "go"})
	buildPackageEdge(t, db, fileA, "internal/a/a.go", pkgA, pkgB)

	g, err := LoadPackageGraph(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadPackageGraph: %v", err)
	}
	if !g.Edges["internal/a"]["internal/b"] {
		t.Errorf("expected directory edge internal/a -> internal/b, got edges=%v", g.Edges)
	}
}

// TestReachabilityLoadPackageEdges_WrongEdgeKindNotFolded pins the FALSE-EDGE direction:
// a two-hop chain where either hop is NOT an "imports" edge (e.g. a "calls"
// edge happens to connect a package node to something that itself points at
// another package node) must NOT be folded into a directory edge. Over-linking
// here would report something reachable that is not.
func TestReachabilityLoadPackageEdges_WrongEdgeKindNotFolded(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileA := insertTestFile(t, db, "internal/a/a.go")
	pkgA := insertTestNode(t, db, fileA, "internal/a/a.go", Node{Kind: KindPackage, Name: "a", Language: "go"})
	fileB := insertTestFile(t, db, "internal/b/b.go")
	pkgB := insertTestNode(t, db, fileB, "internal/b/b.go", Node{Kind: KindPackage, Name: "b", Language: "go"})

	// First hop is "calls", not "imports" — must not be folded.
	imp := insertTestNode(t, db, fileA, "internal/a/a.go", Node{Kind: KindImport, Name: "x", Language: "go"})
	insertTestEdge(t, db, pkgA, imp, string(EdgeCalls))
	insertTestEdge(t, db, imp, pkgB, string(EdgeImports))

	g, err := LoadPackageGraph(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadPackageGraph: %v", err)
	}
	if g.Edges["internal/a"]["internal/b"] {
		t.Errorf("a non-'imports' first hop must not produce a directory edge; got edges=%v", g.Edges)
	}
}

// TestReachabilityLoadPackageEdges_SameDirectorySelfLoopDropped pins that a same-directory
// two-hop chain (a file importing a name that resolves back to its own
// directory) is not reported as a self-loop edge — Go cannot produce one, and
// treating "imports itself" as a real edge would corrupt the SCC/cycle output
// with a spurious size-1 self-cycle.
func TestReachabilityLoadPackageEdges_SameDirectorySelfLoopDropped(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileA1 := insertTestFile(t, db, "internal/a/one.go")
	pkgA1 := insertTestNode(t, db, fileA1, "internal/a/one.go", Node{Kind: KindPackage, Name: "a", Language: "go"})
	fileA2 := insertTestFile(t, db, "internal/a/two.go")
	pkgA2 := insertTestNode(t, db, fileA2, "internal/a/two.go", Node{Kind: KindPackage, Name: "a", Language: "go"})
	buildPackageEdge(t, db, fileA1, "internal/a/one.go", pkgA1, pkgA2)

	g, err := LoadPackageGraph(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadPackageGraph: %v", err)
	}
	if g.Edges["internal/a"]["internal/a"] {
		t.Errorf("same-directory chain must not produce a self-loop edge; got edges=%v", g.Edges)
	}
}

// TestReachabilityFrom_TransitiveClosureNotDepthCapped pins RECALL over a chain
// longer than the node-level bfs()'s hardCapDepth (4): main -> a -> b -> c ->
// d -> e is five directory hops. A depth-capped traversal would silently stop
// partway and report "e" unreachable when it genuinely is reachable — the
// false negative this feature exists to avoid.
func TestReachabilityFrom_TransitiveClosureNotDepthCapped(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	dirs := []string{"cmd/main", "internal/a", "internal/b", "internal/c", "internal/d", "internal/e"}
	pkgs := map[string]int64{}
	files := map[string]int64{}
	for i, d := range dirs {
		rel := d + "/x.go"
		f := insertTestFile(t, db, rel)
		name := "x"
		if i == 0 {
			name = "main"
		}
		pkgs[d] = insertTestNode(t, db, f, rel, Node{Kind: KindPackage, Name: name, Language: "go"})
		files[d] = f
	}
	for i := range len(dirs) - 1 {
		buildPackageEdge(t, db, files[dirs[i]], dirs[i]+"/x.go", pkgs[dirs[i]], pkgs[dirs[i+1]])
	}
	// An isolated directory with no edge from the chain at all — the genuine
	// unreachable case (pins the false-edge/over-reach direction alongside the
	// depth test: nothing marks it reachable).
	isoFile := insertTestFile(t, db, "internal/isolated/x.go")
	insertTestNode(t, db, isoFile, "internal/isolated/x.go", Node{Kind: KindPackage, Name: "isolated", Language: "go"})

	g, err := LoadPackageGraph(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadPackageGraph: %v", err)
	}
	res := ReachableFrom(g, g.MainDirs())
	if len(res.Roots) != 1 || res.Roots[0] != "cmd/main" {
		t.Fatalf("expected root cmd/main, got %v", res.Roots)
	}
	for _, d := range dirs[1:] {
		if !res.Reachable[d] {
			t.Errorf("expected %q reachable from cmd/main, got Reachable=%v", d, res.Reachable)
		}
	}
	if res.Reachable["internal/isolated"] {
		t.Errorf("internal/isolated has no import edge from the chain and must be unreachable")
	}
	unreached := g.Unreachable(res.Reachable)
	if len(unreached) != 1 || unreached[0].Dir != "internal/isolated" {
		t.Errorf("Unreachable() = %v, want exactly [internal/isolated]", unreached)
	}
}

// TestReachabilityFrom_CycleDoesNotBreakTraversal pins RECALL through a cycle:
// main -> x -> y -> x. A visited-set bug that treats "already queued" as
// "done forever" before both directions are recorded could drop y.
func TestReachabilityFrom_CycleDoesNotBreakTraversal(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "reach.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fMain := insertTestFile(t, db, "cmd/main/m.go")
	pMain := insertTestNode(t, db, fMain, "cmd/main/m.go", Node{Kind: KindPackage, Name: "main", Language: "go"})
	fX := insertTestFile(t, db, "internal/x/x.go")
	pX := insertTestNode(t, db, fX, "internal/x/x.go", Node{Kind: KindPackage, Name: "x", Language: "go"})
	fY := insertTestFile(t, db, "internal/y/y.go")
	pY := insertTestNode(t, db, fY, "internal/y/y.go", Node{Kind: KindPackage, Name: "y", Language: "go"})

	buildPackageEdge(t, db, fMain, "cmd/main/m.go", pMain, pX)
	buildPackageEdge(t, db, fX, "internal/x/x.go", pX, pY)
	buildPackageEdge(t, db, fY, "internal/y/y.go", pY, pX)

	g, err := LoadPackageGraph(context.Background(), db)
	if err != nil {
		t.Fatalf("LoadPackageGraph: %v", err)
	}
	res := ReachableFrom(g, g.MainDirs())
	if !res.Reachable["internal/x"] || !res.Reachable["internal/y"] {
		t.Fatalf("expected both cycle members reachable, got %v", res.Reachable)
	}

	path, ok := PathTo(res, "internal/y")
	if !ok {
		t.Fatal("expected a path to internal/y")
	}
	want := []string{"cmd/main", "internal/x", "internal/y"}
	if len(path) != len(want) {
		t.Fatalf("PathTo(y) = %v, want %v", path, want)
	}
	for i := range want {
		if path[i] != want[i] {
			t.Fatalf("PathTo(y) = %v, want %v", path, want)
		}
	}
}

// TestReachabilityPathTo_UnreachableTargetReturnsFalse pins that an unreachable target is
// a legitimate false-y answer, not a crash or a fabricated path.
func TestReachabilityPathTo_UnreachableTargetReturnsFalse(t *testing.T) {
	g := &PackageGraph{Dirs: map[string]*PackageInfo{"a": {Dir: "a"}, "b": {Dir: "b"}}, Edges: map[string]map[string]bool{}}
	res := ReachableFrom(g, []string{"a"})
	if _, ok := PathTo(res, "b"); ok {
		t.Error("expected PathTo to report no path for an unreached directory")
	}
}

// TestSCCCondense_CycleFlaggedSingletonsNot pins the SCC/layers shape: a
// genuine cycle {x,y} must land in ONE component with Cycle=true (an SCC >1
// package IS the finding), while non-cyclic directories must not be
// falsely merged into it.
func TestSCCCondense_CycleFlaggedSingletonsNot(t *testing.T) {
	g := &PackageGraph{
		Dirs: map[string]*PackageInfo{"main": {}, "a": {}, "x": {}, "y": {}},
		Edges: map[string]map[string]bool{
			"main": {"a": true, "x": true},
			"x":    {"y": true},
			"y":    {"x": true},
		},
	}
	scope := map[string]bool{"main": true, "a": true, "x": true, "y": true}
	sccs := CondenseSCCs(g, scope)

	var cyclic, mainComp, aComp *SCC
	for i := range sccs {
		s := &sccs[i]
		switch {
		case len(s.Packages) == 2:
			cyclic = s
		case s.Packages[0] == "main":
			mainComp = s
		case s.Packages[0] == "a":
			aComp = s
		}
	}
	if cyclic == nil || !cyclic.Cycle {
		t.Fatalf("expected a 2-package cyclic SCC, got %+v", sccs)
	}
	if (cyclic.Packages[0] != "x" || cyclic.Packages[1] != "y") && (cyclic.Packages[0] != "y" || cyclic.Packages[1] != "x") {
		t.Errorf("cyclic SCC = %v, want {x,y}", cyclic.Packages)
	}
	if mainComp == nil || mainComp.Cycle {
		t.Errorf("main must be its own non-cyclic SCC, got %+v", sccs)
	}
	if aComp == nil || aComp.Cycle {
		t.Errorf("a must be its own non-cyclic SCC, got %+v", sccs)
	}
	if mainComp.Layer != 0 {
		t.Errorf("main has no in-scope predecessor; layer = %d, want 0", mainComp.Layer)
	}
	if aComp.Layer != 1 {
		t.Errorf("a's only predecessor is main (layer 0); layer = %d, want 1", aComp.Layer)
	}
	if cyclic.Layer != 1 {
		t.Errorf("x/y's only external predecessor is main (layer 0); layer = %d, want 1", cyclic.Layer)
	}
}

// TestReachabilityResolveDir_ExactThenSuffix pins root resolution: an exact indexed
// directory always wins; otherwise a UNIQUE suffix match resolves, and an
// ambiguous suffix (matching more than one directory) refuses rather than
// guessing — the same class of bug as the 636-node "strings" collision this
// package's ORDER BY comment documents for resolveNode.
func TestReachabilityResolveDir_ExactThenSuffix(t *testing.T) {
	g := &PackageGraph{Dirs: map[string]*PackageInfo{
		"cmd/plumb":       {},
		"internal/config": {},
		"other/config":    {},
	}}
	if d, ok := g.ResolveDir("cmd/plumb"); !ok || d != "cmd/plumb" {
		t.Errorf("exact match: got (%q,%v), want (cmd/plumb,true)", d, ok)
	}
	if d, ok := g.ResolveDir("plumb"); !ok || d != "cmd/plumb" {
		t.Errorf("unique suffix match: got (%q,%v), want (cmd/plumb,true)", d, ok)
	}
	if _, ok := g.ResolveDir("config"); ok {
		t.Error("ambiguous suffix (internal/config AND other/config) must refuse, not guess")
	}
	if _, ok := g.ResolveDir("nope"); ok {
		t.Error("no match must refuse")
	}
}
