package topology

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

var (
	createTableRe = regexp.MustCompile(`(?is)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\);`)
	referencesRe  = regexp.MustCompile(`REFERENCES (\w+)\(`)
)

// TestTopologyTables_ChildrenPrecedeTheirParents guards the DROP order that a
// schema-version bump runs. It reads the FK graph out of the schema text rather
// than restating a table order, so a table added later is covered without
// anyone remembering to extend this test — and a child registered after its
// parent fails here instead of orphaning rows on the next version bump.
func TestTopologyTables_ChildrenPrecedeTheirParents(t *testing.T) {
	position := map[string]int{}
	for i, name := range topologyTables {
		position[name] = i
	}
	var checked int
	for _, m := range createTableRe.FindAllStringSubmatch(schema, -1) {
		child, body := m[1], m[2]
		childPos, listed := position[child]
		if !listed {
			continue // deliberately preserved across a recreate (the embeddings cache)
		}
		for _, ref := range referencesRe.FindAllStringSubmatch(body, -1) {
			parent := ref[1]
			parentPos, ok := position[parent]
			if !ok {
				t.Errorf("%s references %s, which topologyTables does not list at all", child, parent)
				continue
			}
			checked++
			if childPos >= parentPos {
				t.Errorf("topologyTables drops %s (position %d) at or after its parent %s (position %d); "+
					"the next schema bump would drop the parent and orphan the child",
					child, childPos, parent, parentPos)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no foreign keys were checked — the schema regexes stopped matching and this guard is vacuous")
	}
}

// TestCallSitesTable_IsRegisteredForDrop is the specific case the generic guard
// above generalises, kept because it is the one that was missed.
func TestCallSitesTable_IsRegisteredForDrop(t *testing.T) {
	var found bool
	for _, name := range topologyTables {
		if name == "topology_call_sites" {
			found = true
		}
	}
	if !found {
		t.Fatal("topology_call_sites is not in topologyTables; a schema bump would leave it behind with the old column set")
	}
}

// TestSchemaVersionBump_DropsCallSites proves the version gate actually reaches
// the new table: a database stamped at the previous version must come back
// empty, not carrying rows written under the old schema.
func TestSchemaVersionBump_DropsCallSites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "topology.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	fileID := insertLangFile(t, db, "a.go", "go")
	insertSite(t, db, fileID, 0, "go", CallSiteCall, "pkg", "Do")
	if _, err := db.Exec(`PRAGMA user_version = ` + strconv.Itoa(SchemaVersion-1)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db2, err := openDB(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	var n int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM topology_call_sites`).Scan(&n); err != nil {
		t.Fatalf("call sites table missing after upgrade: %v", err)
	}
	if n != 0 {
		t.Errorf("call sites after a version bump = %d, want 0 — the table was not dropped and rebuilt", n)
	}
}

// TestPersistFile_ReplacesCallSitesOnReindex covers the deletion path that a
// cascade cannot: a package-level site has a NULL enclosing_id, so deleting the
// file's NODES would leave it behind and every re-index would add another copy.
func TestPersistFile_ReplacesCallSitesOnReindex(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	idx := &Indexer{db: db}

	src := filepath.Join(dir, "a.go")
	if err := os.WriteFile(src, []byte("package a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	out := extractOutput{
		nodes: []Node{{Kind: KindPackage, Name: "a", Language: "go", Path: "a.go"}},
		sites: []CallSite{
			{EnclosingIdx: -1, Kind: CallSiteCall, Callee: "HandleFunc", Qualifier: "mux"},
			{EnclosingIdx: 0, Kind: CallSiteCall, Callee: "Do", Qualifier: "pkg"},
		},
	}
	count := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM topology_call_sites`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := idx.persistFile(0, "a.go", info, "h1", "go", out); err != nil {
		t.Fatalf("persistFile: %v", err)
	}
	if got := count(); got != 2 {
		t.Fatalf("call sites after first index = %d, want 2", got)
	}
	var fileID int64
	if err := db.QueryRow(`SELECT id FROM topology_files WHERE path='a.go'`).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	if err := idx.persistFile(fileID, "a.go", info, "h2", "go", out); err != nil {
		t.Fatalf("persistFile (re-index): %v", err)
	}
	if got := count(); got != 2 {
		t.Errorf("call sites after re-index = %d, want 2 — the previous rows must be replaced, not appended", got)
	}

	// The package-level site is the one a node cascade cannot reach: assert it
	// round-tripped with a NULL enclosing rather than being silently dropped.
	var nullEnclosing int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM topology_call_sites WHERE enclosing_id IS NULL AND callee='HandleFunc'`).Scan(&nullEnclosing); err != nil {
		t.Fatal(err)
	}
	if nullEnclosing != 1 {
		t.Errorf("package-level sites with NULL enclosing = %d, want 1", nullEnclosing)
	}
}

// TestCallSites_QualifierIsNullNotEmpty pins the column convention the resolver
// depends on: it scans `qualifier IS NOT NULL`, so a bare call stored as ”
// would be handed to the resolver as a qualified call with an empty package name.
func TestCallSites_QualifierIsNullNotEmpty(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "topology.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	idx := &Indexer{db: db}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	fileID := insertLangFile(t, db, "a.go", "go")
	_ = tx.Rollback()
	if err := insertCallSitesFor(idx, fileID, []CallSite{
		{EnclosingIdx: -1, Kind: CallSiteCall, Callee: "local"},
		{EnclosingIdx: -1, Kind: CallSiteCall, Callee: "Do", Qualifier: "pkg"},
	}); err != nil {
		t.Fatal(err)
	}
	var nulls, nonNull int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_call_sites WHERE qualifier IS NULL`).Scan(&nulls); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_call_sites WHERE qualifier IS NOT NULL`).Scan(&nonNull); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 || nonNull != 1 {
		t.Errorf("qualifier NULL/non-NULL = %d/%d, want 1/1 — a bare call stored as '' would be resolved as a package call", nulls, nonNull)
	}
}

// insertCallSitesFor runs insertCallSites in its own transaction.
func insertCallSitesFor(idx *Indexer, fileID int64, sites []CallSite) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertCallSites(tx, fileID, "go", []int64{}, sites); err != nil {
		return err
	}
	return tx.Commit()
}
