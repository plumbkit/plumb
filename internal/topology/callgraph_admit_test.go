package topology

import (
	"context"
	"path/filepath"
	"testing"
)

// TestDerivedCallsAdmitted_AgreesWithTheFullAdmission pins the short path to the
// rule it shortcuts: for every subject, DerivedCallsAdmitted must answer exactly
// what AdmitCallGraph's Admitted says, except for a module-resolved language
// (Python, TypeScript, JavaScript), whose import-named edges a traversal may
// follow while the full admission, which carries Go-only claims, still refuses
// it. It exists because DerivedCallsAdmitted no longer runs AdmitCallGraph (whose
// wording queries were most of a context pack's time), so the two could drift
// apart unnoticed.
func TestDerivedCallsAdmitted_AgreesWithTheFullAdmission(t *testing.T) {
	ctx := context.Background()
	withGoPackage, err := openDB(filepath.Join(t.TempDir(), "with.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { withGoPackage.Close() })
	goFile := insertLangFile(t, withGoPackage, "a/a.go", "go")
	insertTestNode(t, withGoPackage, goFile, "a/a.go", Node{Kind: KindPackage, Name: "a", Language: "go"})
	goFn := insertTestNode(t, withGoPackage, goFile, "a/a.go", Node{Kind: KindFunction, Name: "F", Language: "go"})
	pyFile := insertLangFile(t, withGoPackage, "b/b.py", "python")
	insertTestNode(t, withGoPackage, pyFile, "b/b.py", Node{Kind: KindPackage, Name: "b", Language: "python"})
	pyFn := insertTestNode(t, withGoPackage, pyFile, "b/b.py", Node{Kind: KindFunction, Name: "g", Language: "python"})
	rsFile := insertLangFile(t, withGoPackage, "d/d.rs", "rust")
	insertTestNode(t, withGoPackage, rsFile, "d/d.rs", Node{Kind: KindPackage, Name: "d", Language: "rust"})
	rsFn := insertTestNode(t, withGoPackage, rsFile, "d/d.rs", Node{Kind: KindFunction, Name: "h", Language: "rust"})

	// A Go function in an index with no Go package node: supported, but nothing to
	// traverse, so not admitted.
	noPackage, err := openDB(filepath.Join(t.TempDir(), "without.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { noPackage.Close() })
	loneFile := insertLangFile(t, noPackage, "c/c.go", "go")
	loneFn := insertTestNode(t, noPackage, loneFile, "c/c.go", Node{Kind: KindFunction, Name: "H", Language: "go"})

	admittedSeen := false
	for name, tc := range map[string]struct {
		db             *Store
		id             int64
		want, wantFull bool
	}{
		"go with a package node":      {&Store{db: withGoPackage}, goFn, true, true},
		"module-resolved language":    {&Store{db: withGoPackage}, pyFn, true, false},
		"unsupported language":        {&Store{db: withGoPackage}, rsFn, false, false},
		"go without any package node": {&Store{db: noPackage}, loneFn, false, false},
		"unknown node":                {&Store{db: withGoPackage}, 999999, false, false},
	} {
		subject, err := CallGraphSubjectForNode(ctx, tc.db.db, tc.id)
		if err != nil {
			t.Fatalf("%s: subject: %v", name, err)
		}
		full, err := AdmitCallGraph(ctx, tc.db.db, subject)
		if err != nil {
			t.Fatalf("%s: AdmitCallGraph: %v", name, err)
		}
		short := tc.db.DerivedCallsAdmittedFor(ctx, tc.id)
		if short != tc.want || full.Admitted != tc.wantFull {
			t.Errorf("%s: DerivedCallsAdmitted = %v, AdmitCallGraph.Admitted = %v, want %v and %v", name, short, full.Admitted, tc.want, tc.wantFull)
		}
		admittedSeen = admittedSeen || short
	}
	if !admittedSeen {
		t.Fatal("control: no case was admitted, so agreement on false alone proves nothing")
	}
}
