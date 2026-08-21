package topology

import (
	"path/filepath"
	"testing"
)

func TestMatchImportDir(t *testing.T) {
	pkgs := map[string][]int64{
		"internal/stats": {1},
		"internal/cli":   {2},
		"lib/format":     {3},
		"strings":        {4}, // a local dir that shadows a stdlib name
	}
	cases := []struct {
		name      string
		qualified string
		want      string
		wantOK    bool
	}{
		{"go module-internal import", "github.com/plumbkit/plumb/internal/stats", "internal/stats", true},
		{"already repo-relative", "internal/cli", "internal/cli", true},
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
		t.Errorf("stdlib import linked to local dir %q; single-segment matches must be refused", got)
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
