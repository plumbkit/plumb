package topology

import (
	"context"
	"path/filepath"
	"testing"
)

// paramFixture covers the typed-parameter tier: a same-package parameter type,
// another package's type reached through the caller's import, a parameter that
// shadows an import of the same name, and the shapes that must stay unresolved.
type paramFixture struct {
	*samePackageFixture
	use, shadow                     int64
	storeGet, dbQuery, dbQueryLower int64
	localOpen, storeOpenFunc        int64
}

func newParamFixture(t *testing.T) *paramFixture {
	t.Helper()
	db, err := openDB(filepath.Join(t.TempDir(), "params.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	f := &paramFixture{samePackageFixture: &samePackageFixture{db: db}}
	node := func(file int64, p string, n Node) int64 {
		n.Language = "go"
		return insertTestNode(t, db, file, p, n)
	}
	goFile := func(p, pkg string) int64 {
		id := insertLangFile(t, db, p, "go")
		node(id, p, Node{Kind: KindPackage, Name: pkg})
		return id
	}
	site := func(file, enclosing int64, qualifier, callee string) {
		insertSite(t, db, file, enclosing, "go", CallSiteCall, qualifier, callee)
	}

	// internal/app: a Store type in one file, its users in another.
	const store, use = "internal/app/store.go", "internal/app/use.go"
	storeFile := goFile(store, "app")
	f.storeGet = node(storeFile, store, Node{
		Kind: KindMethod, Name: "Get", Qualified: "(*Store).Get",
		Signature: "func (s *Store) Get(key string) string",
	})
	f.localOpen = node(storeFile, store, Node{
		Kind: KindMethod, Name: "Open", Qualified: "(*Local).Open",
		Signature: "func (l *Local) Open()",
	})
	useFile := goFile(use, "app")
	node(useFile, use, Node{Kind: KindImport, Name: "db", Qualified: "example.com/m/internal/db"})
	f.use = node(useFile, use, Node{
		Kind: KindFunction, Name: "use",
		Signature: "func use(a, st *Store, xs []Store, db *db.DB)",
	})
	f.shadow = node(useFile, use, Node{Kind: KindFunction, Name: "shadow", Signature: "func shadow(db *Local)"})

	// internal/db: another package's type, with an exported and an unexported
	// method, and a package-level Open function the shadowing case must not reach.
	const dbPath = "internal/db/db.go"
	dbFile := goFile(dbPath, "db")
	f.dbQuery = node(dbFile, dbPath, Node{
		Kind: KindMethod, Name: "Query", Qualified: "(*DB).Query",
		Signature: "func (d *DB) Query()",
	})
	f.dbQueryLower = node(dbFile, dbPath, Node{
		Kind: KindMethod, Name: "query", Qualified: "(*DB).query",
		Signature: "func (d *DB) query()",
	})
	f.storeOpenFunc = node(dbFile, dbPath, Node{Kind: KindFunction, Name: "Open", Signature: "func Open()"})

	site(useFile, f.use, "st", "Get")     // same-package typed parameter: edge
	site(useFile, f.use, "a", "Get")      // the first name of a grouped `a, st *Store`: edge too
	site(useFile, f.use, "xs", "Get")     // slice parameter: not a named type, unresolved
	site(useFile, f.use, "db", "Query")   // another package's type through the import: edge
	site(useFile, f.use, "db", "query")   // unexported method of another package: unresolved
	site(useFile, f.shadow, "db", "Open") // the parameter shadows the import: (*Local).Open, not db.Open
	return f
}

func TestResolveCalls_TypedParameterLinksItsTypesMethod(t *testing.T) {
	f := newParamFixture(t)
	f.resolve(t)

	if n, conf := f.edge(t, f.use, f.storeGet); n != 1 || conf != samePackageCallConfidence {
		t.Errorf("use→(*Store).Get edges = %d (confidence %v), want 1 at %v", n, conf, samePackageCallConfidence)
	}
	if n, _ := f.edge(t, f.use, f.dbQuery); n != 1 {
		t.Errorf("use→(*db.DB).Query edges = %d, want 1 — the parameter's package is the caller's import", n)
	}
	if n, _ := f.edge(t, f.use, f.dbQueryLower); n != 0 {
		t.Errorf("use→(*db.DB).query edges = %d, want 0 — an unexported method of another package is not callable", n)
	}
	if got := callMeta(t, f.db, metaCallResolvedParam); got != 3 {
		t.Errorf("resolved typed-parameter sites = %d, want 3 (st.Get, db.Query, shadow's db.Open; a.Get repeats st.Get's edge)", got)
	}
	if got := callMeta(t, f.db, metaCallUnresolvedRecv); got != 2 {
		t.Errorf("unresolved receiver sites = %d, want 2 (xs.Get, db.query)", got)
	}
}

func TestResolveCalls_ParameterShadowsAnImportOfTheSameName(t *testing.T) {
	f := newParamFixture(t)
	f.resolve(t)

	if n, _ := f.edge(t, f.shadow, f.localOpen); n != 1 {
		t.Errorf("shadow→(*Local).Open edges = %d, want 1", n)
	}
	if n, _ := f.edge(t, f.shadow, f.storeOpenFunc); n != 0 {
		t.Errorf("shadow→db.Open edges = %d, want 0 — inside shadow, db is the parameter, not the package", n)
	}
}

// TestResolverSurface_ExportedMethodChangesTheFingerprint pins the lifecycle
// rule the typed-parameter tier adds: a caller in another package can gain an
// edge to an exported method without its own file changing, so adding one must
// force a full pass. An unexported method cannot be reached across packages and
// must leave the fingerprint alone.
func TestResolverSurface_ExportedMethodChangesTheFingerprint(t *testing.T) {
	f := newParamFixture(t)
	ctx := context.Background()
	before, err := resolverSurfaceFingerprint(ctx, f.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var dbFile int64
	if err := f.db.QueryRow(`SELECT id FROM topology_files WHERE path='internal/db/db.go'`).Scan(&dbFile); err != nil {
		t.Fatal(err)
	}
	insertTestNode(t, f.db, dbFile, "internal/db/db.go",
		Node{Kind: KindMethod, Name: "helper", Qualified: "(*DB).helper", Language: "go"})
	unexported, err := resolverSurfaceFingerprint(ctx, f.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if unexported != before {
		t.Errorf("an unexported method changed the resolver surface: %s → %s", before, unexported)
	}
	insertTestNode(t, f.db, dbFile, "internal/db/db.go",
		Node{Kind: KindMethod, Name: "Exec", Qualified: "(*DB).Exec", Language: "go"})
	exported, err := resolverSurfaceFingerprint(ctx, f.db, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if exported == unexported {
		t.Error("adding an exported method left the resolver surface unchanged; cross-package callers would miss it")
	}
}

func TestNamedParams(t *testing.T) {
	for sig, want := range map[string]map[string]string{
		"func (s *S) Run(ctx context.Context, a, b *Store, xs []T, fn _) (int, error)": {
			"ctx": "context.Context", "a": "Store", "b": "Store",
		},
		"func plain(int, string)":         nil,
		"func none()":                     nil,
		"func blank(_ *Store, k Key) Key": {"k": "Key"},
	} {
		got := namedParams(sig)
		if len(got) != len(want) {
			t.Errorf("namedParams(%q) = %v, want %v", sig, got, want)
			continue
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("namedParams(%q)[%q] = %q, want %q", sig, k, got[k], v)
			}
		}
	}
}
