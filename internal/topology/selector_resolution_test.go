package topology

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestResolveSelector_NoneOneAmbiguous(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()
	ctx := context.Background()

	none, err := ResolveSelector(ctx, db, "Nowhere", NodeHint{})
	if err != nil || none.Kind != ResolutionNone || len(none.Candidates) != 0 {
		t.Fatalf("unknown selector = %+v, %v; want none", none, err)
	}

	one, err := ResolveSelector(ctx, db, "AChild", NodeHint{})
	if err != nil || one.Kind != ResolutionOne || one.Node.Name != "AChild" || one.Candidates != nil {
		t.Fatalf("unique selector = %+v, %v; want one AChild", one, err)
	}

	amb, err := ResolveSelector(ctx, db, "Target", NodeHint{})
	if err != nil || amb.Kind != ResolutionAmbiguous || len(amb.Candidates) != 2 {
		t.Fatalf("shared name = %+v, %v; want ambiguous with 2", amb, err)
	}
	if amb.Candidates[0].Path != "a/foo.go" || amb.Candidates[1].Path != "b/foo.go" || amb.Node.ID != 0 {
		t.Errorf("ambiguous candidates %v (node %+v): want ResolveNodes' order and no chosen node", amb.Candidates, amb.Node)
	}

	narrowed, err := ResolveSelector(ctx, db, "Target", NodeHint{PathSubstr: "b/"})
	if err != nil || narrowed.Kind != ResolutionOne || narrowed.Node.Path != "b/foo.go" {
		t.Fatalf("hinted selector = %+v, %v; want one in b/foo.go", narrowed, err)
	}
}

// A hint that excludes every candidate is still ResolveNodes' own error, so a
// caller that already handles it needs no new case.
func TestResolveSelector_HintMismatchPassesThrough(t *testing.T) {
	db := seedTwoSameName(t)
	defer db.Close()
	_, err := ResolveSelector(context.Background(), db, "Target", NodeHint{Kind: string(KindMethod)})
	var mismatch *HintMismatchError
	if !errors.As(err, &mismatch) || len(mismatch.Candidates) != 2 {
		t.Fatalf("err = %v, want *HintMismatchError with both candidates", err)
	}
}

// Every receiver spelling names the one method.
func TestResolveSelector_ReceiverFormsAreOne(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "recv.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := insertTestFile(t, db, "cart/cart.go")
	insertTestNode(t, db, f, "cart/cart.go", Node{Kind: KindMethod, Name: "Add", Qualified: "(*Cart).Add", Language: "go"})
	for _, sel := range []string{"(*Cart).Add", "Cart.Add", "*Cart.Add", "Cart/Add"} {
		r, err := ResolveSelector(context.Background(), db, sel, NodeHint{})
		if err != nil || r.Kind != ResolutionOne || r.Node.Qualified != "(*Cart).Add" {
			t.Errorf("%q = %+v, %v; want the one method", sel, r, err)
		}
	}
}

// A declaration outranks the imports and package clauses that merely share its
// name; with no declaration, the references are what there is.
func TestResolveSelector_DeclarationsOutrankReferences(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "refs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := insertTestFile(t, db, "a/a.go")
	b := insertTestFile(t, db, "b/b.go")
	insertTestNode(t, db, a, "a/a.go", Node{Kind: KindImport, Name: "stats", Language: "go"})
	insertTestNode(t, db, b, "b/b.go", Node{Kind: KindImport, Name: "stats", Language: "go"})
	refs, err := ResolveSelector(context.Background(), db, "stats", NodeHint{})
	if err != nil || refs.Kind != ResolutionAmbiguous || len(refs.Candidates) != 2 {
		t.Fatalf("references only = %+v, %v; want ambiguous between the two imports", refs, err)
	}
	insertTestNode(t, db, b, "b/b.go", Node{Kind: KindFunction, Name: "stats", Language: "go"})
	decl, err := ResolveSelector(context.Background(), db, "stats", NodeHint{})
	if err != nil || decl.Kind != ResolutionOne || decl.Node.Kind != KindFunction {
		t.Fatalf("with a declaration = %+v, %v; want the one function", decl, err)
	}
}

// DerivedCallsAdmitted is the admission rule on a node's own subject, failing
// closed: a Go node in a workspace with a Go package is admitted; a Python node
// is not (Python is not a supported call-graph language); an unknown node is
// not.
func TestDerivedCallsAdmitted(t *testing.T) {
	db := newLangIndex(t, "go", "python")
	ctx := context.Background()
	goFile := insertLangFile(t, db, "src/go/x.go", "go")
	goFn := insertTestNode(t, db, goFile, "src/go/x.go", Node{Kind: KindFunction, Name: "F", Language: "go"})
	pyFile := insertLangFile(t, db, "src/python/x.py", "python")
	pyFn := insertTestNode(t, db, pyFile, "src/python/x.py", Node{Kind: KindFunction, Name: "f", Language: "python"})

	if !DerivedCallsAdmitted(ctx, db, goFn) {
		t.Error("a Go node in a workspace with a Go package was not admitted")
	}
	if DerivedCallsAdmitted(ctx, db, pyFn) {
		t.Error("a Python node was admitted")
	}
	if DerivedCallsAdmitted(ctx, db, 1<<40) {
		t.Error("an unknown node was admitted")
	}
	db.Close()
	if DerivedCallsAdmitted(ctx, db, goFn) {
		t.Error("an index error was admitted rather than failing closed")
	}
}

// AdmitLanguage answers with the first admitted package of the language, says
// no when there is none, and propagates an index error rather than degrading.
func TestStoreAdmitLanguage(t *testing.T) {
	db := newLangIndex(t, "go", "python")
	s := &Store{db: db}
	ctx := context.Background()
	d, ok, err := s.AdmitLanguage(ctx, "go")
	if err != nil || !ok || !d.Admitted || d.Language != "go" {
		t.Fatalf("go = %+v, %v, %v; want admitted", d, ok, err)
	}
	if _, ok, err := s.AdmitLanguage(ctx, "python"); err != nil || ok {
		t.Fatalf("python = %v, %v; want not admitted, no error", ok, err)
	}
	if _, ok, err := s.AdmitLanguage(ctx, "rust"); err != nil || ok {
		t.Fatalf("no rust package = %v, %v; want not admitted, no error", ok, err)
	}
	db.Close()
	if _, _, err := s.AdmitLanguage(ctx, "go"); err == nil {
		t.Fatal("an index error was swallowed")
	}
}
