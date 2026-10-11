package topology

import (
	"context"
	"path/filepath"
	"testing"
)

// The extractor's qualifier markers never resolve: a local value's bare call is
// counted as naming no function of the package, and a call on a receiver with no
// name stays an unresolved receiver call, even when a package function or a method
// of the enclosing receiver has the same name.
func TestResolveCalls_QualifierMarkersNeverResolve(t *testing.T) {
	f := newSamePackageFixture(t)
	site := func(enclosing int64, qualifier, callee string) {
		insertSite(t, f.db, f.aFile, enclosing, "go", CallSiteCall, qualifier, callee)
	}
	// helperB and (*Server).stopB exist: these sites name them, through markers.
	site(f.runA, QualifierLocal, "helperB")
	site(f.start, QualifierOpaque, "stopB")
	site(f.start, "s"+QualifierIndexedSuffix, "stopB")
	f.resolve(t)

	want := map[string]int{
		metaCallSites:          14,
		metaCallQualifiedSites: 5, // the local marker is a bare call, not a qualified one
		metaCallBareUnmatched:  3, // len, helperB from svc_test, and the local helperB
		metaCallUnresolvedRecv: 4, // x.stopB, s.promoted, ().stopB, s[].stopB
		metaCallResolved:       4,
	}
	for key, n := range want {
		if got := callMeta(t, f.db, key); got != n {
			t.Errorf("%s = %d, want %d", key, got, n)
		}
	}
	// Positive control: the unmarked sites still resolve to the same targets.
	if n, _ := f.edge(t, f.runA, f.helperB); n != 1 {
		t.Errorf("control: runA→helperB edges = %d, want 1 (from the unmarked site)", n)
	}
	if n, _ := f.edge(t, f.start, f.serverStopB); n != 1 {
		t.Errorf("control: Start→stopB edges = %d, want 1 (from s.stopB)", n)
	}
}

// A scoped pass reaches the unchanged siblings of a changed file in a directory
// whose name is not ASCII. SQLite's substr counts characters, so a byte-length
// prefix match missed them until the next full pass.
func TestResolveCalls_ScopedPassWidensInANonASCIIDirectory(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "utf8.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	const dir = "internal/módulo"
	goFile := func(p string) int64 {
		id := insertLangFile(t, db, p, "go")
		insertTestNode(t, db, id, p, Node{Kind: KindPackage, Name: "modulo", Language: "go"})
		return id
	}
	a := goFile(dir + "/a.go")
	caller := insertTestNode(t, db, a, dir+"/a.go", Node{Kind: KindFunction, Name: "Caller", Language: "go"})
	insertSite(t, db, a, caller, "go", CallSiteCall, "", "added")
	b := goFile(dir + "/b.go")
	idx := &Indexer{db: db}
	if err := idx.resolveCalls(context.Background()); err != nil {
		t.Fatal(err)
	}

	added := insertTestNode(t, db, b, dir+"/b.go", Node{Kind: KindFunction, Name: "added", Language: "go"})
	changes := indexChanges{paths: map[string]struct{}{dir + "/b.go": {}}}
	if err := idx.resolveCallsContext(context.Background(), rebuildScoped, changes); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND source=?`,
		caller, added, callResolverSource).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("unchanged %s/a.go Caller→added edges = %d after a scoped pass for b.go, want 1", dir, n)
	}
}
