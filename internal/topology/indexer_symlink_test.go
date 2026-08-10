package topology

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// symlinkIndexTree builds a workspace whose symlinks reproduce what a hostile
// repository can commit — git stores symlinks natively, so a clone plants them:
//
//	ws/real.go       an ordinary in-workspace source file
//	ws/innocent.go   → base/secret.go   (file OUTSIDE the workspace)
//	ws/alias.go      → ws/real.go       (legitimate in-workspace symlink)
//	ws/escape.dir    → base/outside     (directory OUTSIDE the workspace)
//	ws/dangling.go   → base/gone.go     (target does not exist)
//
// It returns the workspace root and an indexer wired to a fresh database.
func symlinkIndexTree(t *testing.T) (string, *sql.DB, *Indexer) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(base, "secret.go"), "package secret\n\nfunc exfiltratedSecret() {}\n")
	write(filepath.Join(outside, "leak.go"), "package outside\n\nfunc directoryEscapeLeak() {}\n")
	write(filepath.Join(ws, "real.go"), "package ws\n\nfunc inWorkspaceSymbol() {}\n")

	link := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(ws, name)); err != nil {
			t.Skipf("symlinks unsupported on this platform: %v", err)
		}
	}
	link(filepath.Join(base, "secret.go"), "innocent.go")
	link(filepath.Join(ws, "real.go"), "alias.go")
	link(outside, "escape.dir")
	link(filepath.Join(base, "gone.go"), "dangling.go")

	db, err := openDB(filepath.Join(ws, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	idx := newIndexer(ws, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.reclaimFn = func() {} // no FreeOSMemory in a unit test
	return ws, db, idx
}

// indexedSymbol reports whether a symbol name is present in the index.
func indexedSymbol(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_nodes WHERE name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	return count > 0
}

// TestIndexer_SymlinkEscapesWorkspace is the regression test for the
// read-boundary escape: the resync walk followed an in-tree symlink out of the
// workspace and PERSISTED the target's symbols into topology.db, where
// topology_search and workspace_search surfaced them long after the walk.
func TestIndexer_SymlinkEscapesWorkspace(t *testing.T) {
	_, db, idx := symlinkIndexTree(t)

	if err := idx.processResync(context.Background()); err != nil {
		t.Fatalf("processResync: %v", err)
	}

	if indexedSymbol(t, db, "exfiltratedSecret") {
		t.Error("the indexer read and stored a file outside the workspace via an in-tree symlink")
	}
	if indexedSymbol(t, db, "directoryEscapeLeak") {
		t.Error("the indexer descended a symlink to a directory outside the workspace")
	}
	if !indexedSymbol(t, db, "inWorkspaceSymbol") {
		t.Error("the indexer dropped a genuine in-workspace file")
	}
}

// TestIndexer_InWorkspaceSymlinkStillIndexed pins the other half of the
// contract: a symlink whose target is inside the workspace is legitimate and
// must keep being indexed.
func TestIndexer_InWorkspaceSymlinkStillIndexed(t *testing.T) {
	_, db, idx := symlinkIndexTree(t)

	if err := idx.processUpsert(context.Background(), "alias.go"); err != nil {
		t.Fatalf("processUpsert(alias.go): %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM topology_files WHERE path = ?`, "alias.go").Scan(&count); err != nil {
		t.Fatalf("query files: %v", err)
	}
	if count == 0 {
		t.Error("an in-workspace symlink was not indexed; legitimate symlinks must keep working")
	}
}

// TestIndexer_DanglingSymlinkIsNotAnError pins that a symlink resolving to
// nothing neither errors the upsert nor aborts the resync.
func TestIndexer_DanglingSymlinkIsNotAnError(t *testing.T) {
	_, _, idx := symlinkIndexTree(t)
	if err := idx.processUpsert(context.Background(), "dangling.go"); err != nil {
		t.Errorf("a dangling symlink must not error the indexer: %v", err)
	}
}

// TestIndexer_EscapingSymlinkRowsAreHealed covers a database written by the
// vulnerable build: the rows an escaping symlink left behind must be dropped on
// the next pass, not merely skipped.
func TestIndexer_EscapingSymlinkRowsAreHealed(t *testing.T) {
	ws, db, idx := symlinkIndexTree(t)

	// Simulate a poisoned index: register innocent.go as a real in-tree file
	// with the outside file's symbol, exactly as the unguarded indexer did.
	plain := filepath.Join(ws, "poison.go")
	if err := os.WriteFile(plain, []byte("package ws\n\nfunc exfiltratedSecret() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := idx.processUpsert(context.Background(), "poison.go"); err != nil {
		t.Fatalf("processUpsert(poison.go): %v", err)
	}
	if err := os.Rename(plain, filepath.Join(ws, "renamed.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(ws), "secret.go"), filepath.Join(ws, "poison.go")); err != nil {
		t.Fatal(err)
	}

	if err := idx.processUpsert(context.Background(), "poison.go"); err != nil {
		t.Fatalf("processUpsert(poison.go) after the swap: %v", err)
	}
	if indexedSymbol(t, db, "exfiltratedSecret") {
		t.Error("rows from a path that became an escaping symlink survived the re-index")
	}
}
