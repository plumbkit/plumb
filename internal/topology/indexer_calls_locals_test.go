package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// localsFixture holds one caller declaration per site, so every site's edge is
// its own (no repeats) and each assertion names exactly one outcome.
type localsFixture struct {
	db                    *sql.DB
	cartTotal, otherTotal int64
	dbQuery, dbQueryLower int64
	serverClose           int64
	callers               map[string]int64
}

func newLocalsFixture(t *testing.T) *localsFixture {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "locals.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	f := &localsFixture{db: db, callers: map[string]int64{}}
	node := func(file int64, p string, n Node) int64 {
		n.Language = "go"
		return insertTestNode(t, db, file, p, n)
	}
	goFile := func(p, pkg string) int64 {
		id := insertLangFile(t, db, p, "go")
		node(id, p, Node{Kind: KindPackage, Name: pkg})
		return id
	}

	const cart, store, use = "internal/app/cart.go", "internal/store/store.go", "internal/app/use.go"
	cartFile := goFile(cart, "app")
	f.cartTotal = node(cartFile, cart, Node{Kind: KindMethod, Name: "Total", Qualified: "(*Cart).Total", Signature: "func (c *Cart) Total() (int)"})
	f.otherTotal = node(cartFile, cart, Node{Kind: KindMethod, Name: "Total", Qualified: "(*Other).Total", Signature: "func (o *Other) Total() (int)"})
	f.serverClose = node(cartFile, cart, Node{Kind: KindMethod, Name: "Close", Qualified: "(*Server).Close", Signature: "func (s *Server) Close()"})
	node(cartFile, cart, Node{Kind: KindFunction, Name: "NewCart", Signature: "func NewCart() (*Cart)"})
	node(cartFile, cart, Node{Kind: KindFunction, Name: "pair", Signature: "func pair() (n int, c *Cart)"})

	storeFile := goFile(store, "store")
	f.dbQuery = node(storeFile, store, Node{Kind: KindMethod, Name: "Query", Qualified: "(*DB).Query", Signature: "func (d *DB) Query()"})
	f.dbQueryLower = node(storeFile, store, Node{Kind: KindMethod, Name: "query", Qualified: "(*DB).query", Signature: "func (d *DB) query()"})
	node(storeFile, store, Node{Kind: KindFunction, Name: "Open", Signature: "func Open(dsn string) (*DB, error)"})

	useFile := goFile(use, "app")
	node(useFile, use, Node{Kind: KindImport, Name: "store", Qualified: "example.com/m/internal/store"})
	site := func(caller, qualifier, callee, receiverType string) {
		id, ok := f.callers[caller]
		if !ok {
			n := Node{Kind: KindFunction, Name: caller, Signature: "func " + caller + "()"}
			if caller == "Shutdown" {
				n = Node{Kind: KindMethod, Name: caller, Qualified: "(*Server).Shutdown", Signature: "func (s *Server) Shutdown()"}
			}
			id = node(useFile, use, n)
			f.callers[caller] = id
		}
		if _, err := db.Exec(`INSERT INTO topology_call_sites(file_id, enclosing_id, language, site_kind, callee, qualifier, receiver_type)
			VALUES (?,?,?,?,?,?,?)`, useFile, id, "go", string(CallSiteCall), callee, qualifier, receiverType); err != nil {
			t.Fatal(err)
		}
	}
	site("Lit", "lit", "Total", "Cart")
	site("Made", "made", "Total", ReceiverFromCall+"NewCart/0")
	site("Second", "second", "Total", ReceiverFromCall+"pair/1")
	site("Opened", "db", "Query", ReceiverFromCall+"store.Open/0")
	site("Declared", "declared", "Query", "store.DB")
	site("Unexported", "db", "query", ReceiverFromCall+"store.Open/0") // another package's unexported method
	site("ErrResult", "err", "Error", ReceiverFromCall+"store.Open/1") // error is not a named type of the index
	site("NoSuchFunc", "x", "Total", ReceiverFromCall+"missing/0")     // a conversion or a call the index cannot see
	site("Shutdown", "s", "Close", ReceiverAmbiguous)                  // the receiver name, re-declared in the body
	if err := (&Indexer{db: db}).resolveCalls(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *localsFixture) edges(t *testing.T, caller string, to int64) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND source=?`,
		f.callers[caller], to, callResolverSource).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestResolveCalls_ALocalOfEvidentTypeLinksItsTypesMethod(t *testing.T) {
	f := newLocalsFixture(t)
	for caller, target := range map[string]int64{
		"Lit": f.cartTotal, "Made": f.cartTotal, "Second": f.cartTotal,
		"Opened": f.dbQuery, "Declared": f.dbQuery,
	} {
		if n := f.edges(t, caller, target); n != 1 {
			t.Errorf("%s: edges to its local's method = %d, want 1", caller, n)
		}
		if n := f.edges(t, caller, f.otherTotal); n != 0 {
			t.Errorf("%s: an edge to (*Other).Total, a same-named method of another type", caller)
		}
	}
	if got := callMeta(t, f.db, metaCallResolvedLocal); got != 5 {
		t.Errorf("resolved local-typed sites = %d, want 5", got)
	}
}

func TestResolveCalls_ALocalWithNoUsableTypeStaysUnresolved(t *testing.T) {
	f := newLocalsFixture(t)
	if n := f.edges(t, "Unexported", f.dbQueryLower); n != 0 {
		t.Errorf("an unexported method of another package was linked")
	}
	// The ambiguous receiver must not fall back to the receiver-self tier.
	if n := f.edges(t, "Shutdown", f.serverClose); n != 0 {
		t.Errorf("s.Close() with s re-declared in the body was linked to (*Server).Close")
	}
	// Unexported, ErrResult, NoSuchFunc and Shutdown: unresolved receiver calls.
	if got := callMeta(t, f.db, metaCallUnresolvedRecv); got != 4 {
		t.Errorf("unresolved receiver sites = %d, want 4", got)
	}
}

func TestResultTypes(t *testing.T) {
	for sig, want := range map[string][]string{
		"func NewCart() (*Cart)":                {"Cart"},
		"func Open(dsn string) (*DB, error)":    {"DB", "error"},
		"func pair() (n int, c *Cart)":          {"int", "Cart"},
		"func two() (a, b *Cart)":               {"Cart", "Cart"},
		"func (s *S) Get() (store.Item, error)": {"store.Item", "error"},
		"func none()":                           nil,
	} {
		got := resultTypes(sig)
		if len(got) != len(want) {
			t.Errorf("resultTypes(%q) = %q, want %q", sig, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("resultTypes(%q) = %q, want %q", sig, got, want)
			}
		}
	}
}

// A caller in another package resolves `x := store.Open(); x.Query()` through
// Open's results, and its own file does not change when they do: a changed result
// type of an exported function must force the full pass.
func TestResolverSurface_AnExportedFunctionsResultTypesAreSurface(t *testing.T) {
	f := newLocalsFixture(t)
	ctx := context.Background()
	before, err := resolverSurfaceFingerprint(ctx, f.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE topology_nodes SET signature = ? WHERE name = 'Open'`, "func Open(dsn string) (*Conn, error)"); err != nil {
		t.Fatal(err)
	}
	after, err := resolverSurfaceFingerprint(ctx, f.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Error("changing an exported function's result type left the resolver surface unchanged")
	}
	if _, err := f.db.Exec(`UPDATE topology_nodes SET signature = ? WHERE name = 'Open'`, "func Open(name string) (*Conn, error)"); err != nil {
		t.Fatal(err)
	}
	renamed, err := resolverSurfaceFingerprint(ctx, f.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if renamed != after {
		t.Error("renaming a parameter changed the resolver surface; only result types are surface")
	}
}
