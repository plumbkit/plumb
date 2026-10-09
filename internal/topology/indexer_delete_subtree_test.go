package topology

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
)

// A directory renamed or removed is reported by every watcher backend only
// under its own name, so the indexer must remove what it held from that name
// alone (PLAN-488 review: a -> b left a/x.go and a/y/z.go indexed for good,
// because a resync is suppressed while the watcher runs).

// indexedPaths returns every indexed path, slash-separated and sorted.
func indexedPaths(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT path FROM topology_files ORDER BY path`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, filepath.ToSlash(p))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	slices.Sort(out)
	return out
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// seedSubtreeIndex indexes (in the database only) a tree whose names press on
// every edge of the subtree range: a sibling file and a sibling directory
// sharing the prefix, SQL LIKE wildcards in directory names, names whose bytes
// sort high (multibyte UTF-8, '~') inside and beside the subtree, a sibling
// differing only in case, and a nested directory of the same name elsewhere.
// Every file gets two nodes joined by an edge, so the cleanup of each table can
// be counted.
func seedSubtreeIndex(t *testing.T, db *sql.DB) []string {
	t.Helper()
	paths := []string{
		"a/x.go", "a/y/z.go", "a/y/deeper/w.go", "a/é/ü.go", "a/日本/語.go", "a/~t.go",
		"a.go", "a-b/v.go", "ab/u.go", "a%/p.go", "a_/q.go", "aé/k.go", "A/c.go", "b/a/r.go",
	}
	for _, p := range paths {
		rel := filepath.FromSlash(p)
		id := insertTestFile(t, db, rel)
		n1 := insertTestNode(t, db, id, rel, Node{Kind: KindFunction, Name: "One", Language: "go"})
		n2 := insertTestNode(t, db, id, rel, Node{Kind: KindFunction, Name: "Two", Language: "go"})
		insertTestEdge(t, db, n1, n2, string(EdgeContains))
	}
	return paths
}

func TestIndexer_ProcessDelete_Subtree(t *testing.T) {
	cases := []struct {
		name        string
		delete      string
		wantChanged bool
		wantGone    []string
	}{
		{"a directory takes every file below it", "a", true, []string{"a/x.go", "a/y/z.go", "a/y/deeper/w.go", "a/é/ü.go", "a/日本/語.go", "a/~t.go"}},
		{"a nested directory takes only its own subtree", "a/y", true, []string{"a/y/z.go", "a/y/deeper/w.go"}},
		{"a multibyte directory takes its own subtree", "a/日本", true, []string{"a/日本/語.go"}},
		{"an exact file takes only itself", "a.go", true, []string{"a.go"}},
		{"a percent sign is not a wildcard", "a%", true, []string{"a%/p.go"}},
		{"an underscore is not a wildcard", "a_", true, []string{"a_/q.go"}},
		{"a case-only sibling is another directory", "A", true, []string{"A/c.go"}},
		{"a directory of the same name elsewhere is untouched", "b/a", true, []string{"b/a/r.go"}},
		{"a path never indexed changes nothing", "zzz", false, nil},
		{"a prefix that is not a whole name changes nothing", "a/x", false, nil},
		{"the root is never deleted file by file", ".", false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			idx, db := newTestIndexer(t, dir)
			all := seedSubtreeIndex(t, db)
			changed, err := idx.processDeleteChanged(context.Background(), filepath.FromSlash(tc.delete))
			if err != nil {
				t.Fatalf("processDeleteChanged(%q): %v", tc.delete, err)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v (false would skip the derived-edge passes)", changed, tc.wantChanged)
			}
			var want []string
			for _, p := range all {
				if !slices.Contains(tc.wantGone, p) {
					want = append(want, p)
				}
			}
			slices.Sort(want)
			if got := indexedPaths(t, db); !slices.Equal(got, want) {
				t.Errorf("indexed after deleting %q:\n got  %v\n want %v", tc.delete, got, want)
			}
			kept := len(all) - len(tc.wantGone)
			if n := countRows(t, db, "topology_nodes"); n != 2*kept {
				t.Errorf("nodes = %d, want %d", n, 2*kept)
			}
			if n := countRows(t, db, "topology_fts"); n != 2*kept {
				t.Errorf("fts rows = %d, want %d", n, 2*kept)
			}
			if n := countRows(t, db, "topology_edges"); n != kept {
				t.Errorf("edges = %d, want %d", n, kept)
			}
		})
	}
}

// TestIndexer_ProcessDelete_SubtreeIsOneTransaction: a failure part way through
// a subtree leaves every file of it indexed, never half of it.
func TestIndexer_ProcessDelete_SubtreeIsOneTransaction(t *testing.T) {
	dir := t.TempDir()
	idx, db := newTestIndexer(t, dir)
	all := seedSubtreeIndex(t, db)
	if _, err := db.Exec(`CREATE TRIGGER fail_on_z BEFORE DELETE ON topology_files
		WHEN old.path = '` + filepath.FromSlash("a/y/z.go") + `' BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, err := idx.processDeleteChanged(context.Background(), "a"); err == nil {
		t.Fatal("the injected failure was not reported")
	}
	slices.Sort(all)
	if got := indexedPaths(t, db); !slices.Equal(got, all) {
		t.Errorf("a failed subtree delete was not rolled back:\n got  %v\n want %v", got, all)
	}
	if n := countRows(t, db, "topology_nodes"); n != 2*len(all) {
		t.Errorf("nodes = %d after a rolled-back delete, want %d", n, 2*len(all))
	}
}

// TestIndexer_ProcessUpsert_LiveDirectoryKeepsItsFiles: an event for a
// directory that is still there (created, or touched) must not drop its files.
func TestIndexer_ProcessUpsert_LiveDirectoryKeepsItsFiles(t *testing.T) {
	dir := t.TempDir()
	idx, db := newTestIndexer(t, dir)
	writeIndexTree(t, dir, map[string]string{"a/x.go": "package a\nfunc X() {}\n"})
	if err := idx.processUpsert(context.Background(), filepath.FromSlash("a/x.go")); err != nil {
		t.Fatal(err)
	}
	if err := idx.processUpsert(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if got := indexedPaths(t, db); !slices.Equal(got, []string{"a/x.go"}) {
		t.Errorf("indexed = %v after an event on the live directory a", got)
	}
}

// TestStore_DirectoryMoveAndRemoveUnindexOldFiles drives the whole path on the
// platform's real watcher: a populated directory renamed within the workspace,
// then removed, with periodic resync suppressed by the running watcher.
func TestStore_DirectoryMoveAndRemoveUnindexOldFiles(t *testing.T) {
	root := t.TempDir()
	writeIndexTree(t, root, map[string]string{
		"a/x.go":   "package p\nfunc F() {}\n",
		"a/y/z.go": "package p\nfunc G() {}\n",
		"ab/k.go":  "package p\nfunc K() {}\n",
	})
	s, err := Open(root, config.TopologyConfig{
		Watch:                 true,
		MaxFileSizeBytes:      512 * 1024,
		ResyncIntervalMinutes: 60,
		ExtractTimeoutSeconds: 2,
	}, []Extractor{&minimalExtractor{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	under := func(dir string) int {
		n := 0
		for _, p := range indexedPaths(t, s.db) {
			if strings.HasPrefix(p, dir+"/") {
				n++
			}
		}
		return n
	}
	if !waitFor(func() bool { return under("a") == 2 && under("ab") == 1 }, 10*time.Second) {
		t.Fatalf("initial indexing did not finish: %v", indexedPaths(t, s.db))
	}
	// The watcher must be live before the move: a write it reports proves it.
	if !waitFor(func() bool {
		writeIndexTree(t, root, map[string]string{"probe.go": "package p\nfunc P() {}\n"})
		return waitFor(func() bool { return slices.Contains(indexedPaths(t, s.db), "probe.go") }, 500*time.Millisecond)
	}, 30*time.Second) {
		t.Fatal("the watcher reported nothing")
	}

	if err := os.Rename(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return under("b") == 2 && under("a") == 0 }, 15*time.Second) {
		t.Fatalf("after a -> b: a/ holds %d, b/ holds %d; indexed %v", under("a"), under("b"), indexedPaths(t, s.db))
	}
	if under("ab") != 1 {
		t.Errorf("the sibling ab/ lost its file: %v", indexedPaths(t, s.db))
	}

	if runtime.GOOS == "windows" {
		return // removing a directory a watch holds is flaky on Windows
	}
	if err := os.RemoveAll(filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	if !waitFor(func() bool { return under("b") == 0 }, 15*time.Second) {
		t.Fatalf("after removing b: b/ still holds %d; indexed %v", under("b"), indexedPaths(t, s.db))
	}
	if under("ab") != 1 {
		t.Errorf("the sibling ab/ lost its file: %v", indexedPaths(t, s.db))
	}
}
