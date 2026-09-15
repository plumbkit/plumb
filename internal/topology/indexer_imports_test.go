package topology

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMatchImportDir covers the FALLBACK matcher. Since PLAN-380 a Go import
// reaches it only in a workspace that declares no module the index has seen;
// every other language reaches it always. The residue cases below are therefore
// still true of this function, and no longer true of Go in an ordinary
// repository — see TestImportTargetDir_GoDoesNotFallBack for the dispatch that
// decides which one answers.
func TestMatchImportDir(t *testing.T) {
	pkgs := map[string][]int64{
		"internal/stats": {1},
		"internal/cli":   {2},
		"lib/format":     {3},
		"strings":        {4}, // a local dir that shadows a stdlib name
		"store":          {5}, // a package at the top level, one segment deep
		"http":           {6}, // shadows the TAIL of net/http, not a whole stdlib name
		"json":           {9}, // ditto for encoding/json — without it that case is vacuous
		"httptest":       {7}, // shadows the tail of a THREE-segment stdlib path
		"stats":          {8}, // top-level, and also the tail of internal/stats above
	}
	cases := []struct {
		name      string
		qualified string
		want      string
		wantOK    bool
	}{
		{"go module-internal import", "github.com/plumbkit/plumb/internal/stats", "internal/stats", true},
		{"already repo-relative", "internal/cli", "internal/cli", true},
		// A top-level package is reachable once a module prefix has been stripped.
		// Refusing it made the minimum a rule about how deep the DIRECTORY sits, and
		// every repository whose packages live at the root got no edges at all.
		{"package one directory deep", "example.com/m/store", "store", true},
		// The cost of the line above, stated rather than discovered: a third-party
		// import whose last segment matches a top-level local package binds to it.
		// For Go this is now unreachable wherever a go.mod is indexed — the module
		// path refuses it before this matcher is consulted — so what remains here is
		// the no-manifest case, where the resolver stays recall-biased and an extra
		// package costs a test run.
		{"third-party import reaching a local package of the same name", "github.com/boltdb/store", "store", true},
		// The regression an independent review caught before this shipped. net/http
		// HAS a segment to strip, so a rule that only guarded the whole-path candidate
		// let one stripped segment reach a local http/ — and then every file importing
		// net/http depends on it, which in a web service is most of the repository.
		// A module path spends at least two segments (host.tld/name) getting to its
		// first package; a stdlib root spends one.
		{"two-segment stdlib import does not reach its tail", "net/http", "", false},
		{"two-segment stdlib import, second root", "encoding/json", "", false},
		// What the segment count cannot separate, pinned so it is a known residue
		// rather than a surprise: a stdlib path of three segments has finally spent
		// enough prefix to pass. Closed for Go by module resolution, which is why
		// this case describes the fallback and not what a Go repository experiences.
		{"three-segment stdlib import still reaches its tail", "net/http/httptest", "httptest", true},
		// And the cost on the other side: by counting alone a ONE-segment dotless
		// module path (`module myapp`) cannot be told from a stdlib root. go.mod can,
		// and does — TestResolveGoImport_OneSegmentModuleIsResolvable pins that
		// myapp/stats resolves once the module is declared.
		{"one-segment module path is not separable from a stdlib root", "myapp/stats", "", false},
		// Longest suffix still wins: internal/stats is preferred over the top-level
		// stats/ that also sits in the map.
		{"longest match wins over a shorter top-level shadow", "example.com/m/internal/stats", "internal/stats", true},
		// These two match the WHOLE cleaned path, which is why they are unaffected by
		// the stripped-segment rule. A relative path's single-segment candidate is now
		// refused (`../store` no longer reaches a top-level store/), and that is
		// unreachable rather than a loss: pkgsByDir is keyed on KindPackage nodes, and
		// no TypeScript or JavaScript extractor emits one.
		{"relative TypeScript style", "./lib/format", "lib/format", true},
		{"parent-relative", "../lib/format", "lib/format", true},
		{"stdlib single segment is never matched", "strings", "", false},
		{"third-party miss", "github.com/spf13/cobra", "", false},
		{"unknown internal path", "github.com/plumbkit/plumb/internal/nope", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := matchImportDir(tc.qualified, pkgs)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("matchImportDir(%q) = (%q,%v), want (%q,%v)",
					tc.qualified, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

// TestMatchImportDir_SingleSegmentShadowIsNotLinked is called out separately
// because it is the whole reason for minImportSegments. A workspace with a
// top-level strings/ directory must NOT have every file's `import "strings"`
// linked to it — that would recreate, as real edges, exactly the false
// dependency that the affected-tests recall bug was made of.
func TestMatchImportDir_SingleSegmentShadowIsNotLinked(t *testing.T) {
	pkgs := map[string][]int64{"strings": {1}}
	if got, ok := matchImportDir("strings", pkgs); ok {
		t.Errorf("stdlib import linked to local dir %q; a single-segment candidate with "+
			"nothing stripped to form it must be refused", got)
	}
}

// TestLinkImports_CreatesCrossFileEdges is the end-to-end check: before this
// pass existed the index had zero edges crossing a file boundary, so "affected
// by a dependency edge" could never fire for a Go test.
func TestLinkImports_CreatesCrossFileEdges(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "imports.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// The imported package: two files, so two package nodes share the directory.
	statsA := insertTestFile(t, db, "internal/stats/savings.go")
	pkgA := insertTestNode(t, db, statsA, "internal/stats/savings.go",
		Node{Kind: KindPackage, Name: "stats", Language: "go"})
	statsB := insertTestFile(t, db, "internal/stats/reader.go")
	pkgB := insertTestNode(t, db, statsB, "internal/stats/reader.go",
		Node{Kind: KindPackage, Name: "stats", Language: "go"})

	// The importer, plus a stdlib import that must stay unlinked.
	cliFile := insertTestFile(t, db, "internal/cli/stats.go")
	insertTestNode(t, db, cliFile, "internal/cli/stats.go",
		Node{Kind: KindPackage, Name: "cli", Language: "go"})
	impInternal := insertTestNode(t, db, cliFile, "internal/cli/stats.go",
		Node{Kind: KindImport, Name: "stats", Qualified: "github.com/plumbkit/plumb/internal/stats", Language: "go"})
	impStdlib := insertTestNode(t, db, cliFile, "internal/cli/stats.go",
		Node{Kind: KindImport, Name: "strings", Qualified: "strings", Language: "go"})

	idx := &Indexer{db: db}
	if err := idx.linkImports(); err != nil {
		t.Fatalf("linkImports: %v", err)
	}

	// The module-internal import links to EVERY package node in the target dir,
	// because importing a package depends on all of its files.
	for _, want := range []int64{pkgA, pkgB} {
		var n int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND kind=? AND source=?`,
			impInternal, want, string(EdgeImports), importResolverSource).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("expected an edge %d→%d, got %d", impInternal, want, n)
		}
	}

	var stdlibEdges int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM topology_edges WHERE from_id=?`, impStdlib).Scan(&stdlibEdges); err != nil {
		t.Fatal(err)
	}
	if stdlibEdges != 0 {
		t.Errorf(`import "strings" produced %d edges; stdlib imports must not be linked`, stdlibEdges)
	}

	// The edges must actually cross a file boundary — the property the whole
	// pass exists to create.
	var crossFile int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM topology_edges e
		JOIN topology_nodes a ON a.id = e.from_id
		JOIN topology_nodes b ON b.id = e.to_id
		WHERE a.file_id <> b.file_id`).Scan(&crossFile); err != nil {
		t.Fatal(err)
	}
	if crossFile != 2 {
		t.Errorf("cross-file edges = %d, want 2", crossFile)
	}

	// Idempotent: a second pass rebuilds rather than duplicating.
	if err := idx.linkImports(); err != nil {
		t.Fatalf("linkImports (second pass): %v", err)
	}
	var total int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM topology_edges WHERE source=?`, importResolverSource).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Errorf("resolver edges after second pass = %d, want 2 (edges must be rebuilt, not appended)", total)
	}
}

func TestLinkImports_ScopedCalleeReindexRepointsAndPreservesOtherPackage(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "imports.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statsFile := insertTestFile(t, db, "internal/stats/stats.go")
	_ = insertTestNode(t, db, statsFile, "internal/stats/stats.go", Node{Kind: KindPackage, Name: "stats", Language: "go"})
	otherFile := insertTestFile(t, db, "internal/other/other.go")
	otherPkg := insertTestNode(t, db, otherFile, "internal/other/other.go", Node{Kind: KindPackage, Name: "other", Language: "go"})
	importer := insertTestFile(t, db, "internal/cli/imports.go")
	statsImport := insertTestNode(t, db, importer, "internal/cli/imports.go", Node{Kind: KindImport, Name: "stats", Qualified: "example.com/m/internal/stats", Language: "go"})
	otherImport := insertTestNode(t, db, importer, "internal/cli/imports.go", Node{Kind: KindImport, Name: "other", Qualified: "example.com/m/internal/other", Language: "go"})
	idx := &Indexer{db: db}
	if err := idx.linkImports(); err != nil {
		t.Fatal(err)
	}
	var statsFileID int64
	if err := db.QueryRow(`SELECT id FROM topology_files WHERE path='internal/stats/stats.go'`).Scan(&statsFileID); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := captureIncomingDerived(tx, statsFileID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM topology_nodes WHERE file_id=?`, statsFileID); err != nil {
		t.Fatal(err)
	}
	newStats := insertTestNodeTx(t, tx, statsFileID, "internal/stats/stats.go", Node{Kind: KindPackage, Name: "stats", Language: "go"})
	if err := restoreIncomingDerived(tx, statsFileID, "internal/stats/stats.go"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	changes := indexChanges{paths: map[string]struct{}{"internal/stats/stats.go": {}}}
	if err := idx.linkImportsContext(context.Background(), rebuildScoped, changes); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND source=?`, statsImport, newStats, importResolverSource).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("stats import after reindex = %d, want 1", n)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND source=?`, otherImport, otherPkg, importResolverSource).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("unrelated other import after stats reindex = %d, want 1", n)
	}
	var identity string
	if err := db.QueryRow(`SELECT to_identity FROM topology_edges WHERE from_id=? AND to_id=? AND source=?`, statsImport, newStats, importResolverSource).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if identity != "internal/stats/stats.go\x00stats" {
		t.Fatalf("import identity = %q", identity)
	}
}

func TestPlanRebuild_TargetPackageAdditionAndRemovalReconcilesImporter(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "imports.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	importerFile := insertTestFile(t, db, "internal/cli/imports.go")
	insertTestNode(t, db, importerFile, "internal/cli/imports.go", Node{Kind: KindPackage, Name: "cli", Language: "go"})
	importNode := insertTestNode(t, db, importerFile, "internal/cli/imports.go",
		Node{Kind: KindImport, Name: "stats", Qualified: "example.com/m/internal/stats", Language: "go"})
	idx := &Indexer{db: db}
	ctx := context.Background()
	if err := idx.linkImportsContext(ctx, rebuildFull, indexChanges{full: true}); err != nil {
		t.Fatalf("initial full import link: %v", err)
	}
	var edges int
	if err := db.QueryRow("SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND source=?", importNode, importResolverSource).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 0 {
		t.Fatalf("importer edges before target package exists = %d, want 0", edges)
	}
	fp, err := resolverSurfaceFingerprint(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(ctx, db, fp); err != nil {
		t.Fatal(err)
	}

	targetFile := insertTestFile(t, db, "internal/stats/stats.go")
	target := insertTestNode(t, db, targetFile, "internal/stats/stats.go", Node{Kind: KindPackage, Name: "stats", Language: "go"})
	changes := indexChanges{paths: map[string]struct{}{"internal/stats/stats.go": {}}}
	mode, _, err := idx.planRebuild(ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	if mode != rebuildFull {
		t.Fatalf("package target addition selected %v, want full reconciliation", mode)
	}
	if err := idx.linkImportsContext(ctx, mode, changes); err != nil {
		t.Fatalf("link after package target addition: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND to_id=? AND source=?", importNode, target, importResolverSource).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 1 {
		t.Fatalf("importer edge after package target addition = %d, want 1", edges)
	}
	fp, err = resolverSurfaceFingerprint(ctx, db, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMeta(ctx, db, fp); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec("DELETE FROM topology_files WHERE id=?", targetFile); err != nil {
		t.Fatal(err)
	}
	mode, _, err = idx.planRebuild(ctx, changes)
	if err != nil {
		t.Fatal(err)
	}
	if mode != rebuildFull {
		t.Fatalf("package target removal selected %v, want full reconciliation", mode)
	}
	if err := idx.linkImportsContext(ctx, mode, changes); err != nil {
		t.Fatalf("link after package target removal: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM topology_edges WHERE from_id=? AND source=?", importNode, importResolverSource).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if edges != 0 {
		t.Fatalf("importer edges after package target removal = %d, want 0", edges)
	}
}
