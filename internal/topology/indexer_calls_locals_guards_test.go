package topology

import (
	"context"
	"path/filepath"
	"testing"
)

// TestResolveCalls_LocalTierGuards pins the local tier's precision guards, each
// with a site that would resolve if the guard were dropped, and the fallback to
// the package-qualified tier when a local reading finds nothing.
func TestResolveCalls_LocalTierGuards(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "guards.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	node := func(file int64, p string, n Node) int64 {
		n.Language = "go"
		return insertTestNode(t, db, file, p, n)
	}
	goFile := func(p, pkg string) int64 {
		id := insertLangFile(t, db, p, "go")
		node(id, p, Node{Kind: KindPackage, Name: pkg})
		return id
	}
	const store, a, b, use = "internal/store/store.go", "internal/app/a.go", "internal/app/b.go", "internal/app/use.go"
	storeFile := goFile(store, "store")
	dbQuery := node(storeFile, store, Node{Kind: KindMethod, Name: "Query", Qualified: "(*DB).Query", Signature: "func (d *DB) Query()"})
	storeOpen := node(storeFile, store, Node{Kind: KindFunction, Name: "Open", Signature: "func Open() (*DB)"})
	node(storeFile, store, Node{Kind: KindFunction, Name: "open", Signature: "func open() (*DB)"})
	node(storeFile, store, Node{Kind: KindFunction, Name: "Make", Signature: "func Make() (other.DB)"})
	// Two declarations of Twin (build-tag variants): not one function to read results from.
	node(storeFile, store, Node{Kind: KindFunction, Name: "Twin", Signature: "func Twin() (*DB)"})
	twinFile := goFile("internal/store/twin_linux.go", "store")
	node(twinFile, "internal/store/twin_linux.go", Node{Kind: KindFunction, Name: "Twin", Signature: "func Twin() (*DB)"})

	aFile := goFile(a, "app")
	node(aFile, a, Node{Kind: KindFunction, Name: "variant", Signature: "func variant() (*Cart)"})
	cartTotal := node(aFile, a, Node{Kind: KindMethod, Name: "Total", Qualified: "(*Cart).Total", Signature: "func (c *Cart) Total()"})
	node(aFile, a, Node{Kind: KindFunction, Name: "elsewhere", Signature: "func elsewhere() (store.DB)"})
	bFile := goFile(b, "app")
	node(bFile, b, Node{Kind: KindFunction, Name: "variant", Signature: "func variant() (*Cart)"})
	// b.go imports store from the same path as use.go: its qualified result is
	// readable from use.go. a.go (elsewhere) imports nothing.
	node(bFile, b, Node{Kind: KindImport, Name: "store", Qualified: "example.com/m/internal/store"})
	node(bFile, b, Node{Kind: KindFunction, Name: "samePath", Signature: "func samePath() (store.DB)"})

	useFile := goFile(use, "app")
	node(useFile, use, Node{Kind: KindImport, Name: "store", Qualified: "example.com/m/internal/store"})
	node(useFile, use, Node{Kind: KindFunction, Name: "here", Signature: "func here() (store.DB)"})
	callers := map[string]int64{}
	site := func(caller, qualifier, callee, receiverType string) {
		callers[caller] = node(useFile, use, Node{Kind: KindFunction, Name: caller, Signature: "func " + caller + "()"})
		if _, err := db.Exec(`INSERT INTO topology_call_sites(file_id, enclosing_id, language, site_kind, callee, qualifier, receiver_type)
			VALUES (?,?,?,?,?,?,?)`, useFile, callers[caller], "go", string(CallSiteCall), callee, qualifier, receiverType); err != nil {
			t.Fatal(err)
		}
	}
	site("Variants", "x", "Total", ReceiverFromCall+"variant/0")          // two declarations of variant: not one function
	site("Unexported", "x", "Query", ReceiverFromCall+"store.open/0")     // another package's unexported function
	site("CrossQualified", "x", "Query", ReceiverFromCall+"store.Make/0") // another package's file names "other"
	site("OtherFile", "x", "Query", ReceiverFromCall+"elsewhere/0")       // same package, another file's imports
	site("SameFile", "x", "Query", ReceiverFromCall+"here/0")             // control: the caller's own file
	site("SamePath", "x", "Query", ReceiverFromCall+"samePath/0")         // control: another file, same import path
	site("CrossTwin", "x", "Query", ReceiverFromCall+"store.Twin/0")      // two exported Twin in the imported package
	site("ImportLocal", "store", "Open", "NoSuchType")                    // rand := rand.New(): falls back to the import
	site("ImportAmbiguous", "store", "Open", ReceiverAmbiguous)           // store declared twice, but an import too
	// The reviewer's ParamRedecl shape: a parameter named like an import, then
	// re-declared in the body. The site is ambiguous, and the import reading must
	// not win over the parameter it may be.
	callers["ParamRedecl"] = node(useFile, use, Node{Kind: KindFunction, Name: "ParamRedecl", Signature: "func ParamRedecl(store *Handle)"})
	if _, err := db.Exec(`INSERT INTO topology_call_sites(file_id, enclosing_id, language, site_kind, callee, qualifier, receiver_type)
		VALUES (?,?,?,?,?,?,?)`, useFile, callers["ParamRedecl"], "go", string(CallSiteCall), "Open", "store", ReceiverAmbiguous); err != nil {
		t.Fatal(err)
	}
	if err := (&Indexer{db: db}).resolveCalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	edge := func(caller string, to int64) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND source=?`,
			callers[caller], to, callResolverSource).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for caller, to := range map[string]int64{
		"Variants": cartTotal, "Unexported": dbQuery, "CrossQualified": dbQuery, "OtherFile": dbQuery,
		"CrossTwin": dbQuery, "ParamRedecl": storeOpen,
	} {
		if n := edge(caller, to); n != 0 {
			t.Errorf("%s: a guarded site resolved (%d edges)", caller, n)
		}
	}
	for _, caller := range []string{"SameFile", "SamePath"} {
		if n := edge(caller, dbQuery); n != 1 {
			t.Errorf("control %s: a qualified result read through the same import path must resolve, got %d edges", caller, n)
		}
	}
	for _, caller := range []string{"ImportLocal", "ImportAmbiguous"} {
		if n := edge(caller, storeOpen); n != 1 {
			t.Errorf("%s: store.Open() must keep its package-qualified edge when the local reading finds nothing, got %d", caller, n)
		}
	}
}
