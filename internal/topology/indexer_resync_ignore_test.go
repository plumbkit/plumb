package topology

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// indexer_resync_ignore_test.go covers the resync walk's exclusion rules: the
// hardcoded floor (shouldSkipDir), the tree's own .gitignore / .ignore files,
// and [topology] exclude_patterns. The three are additive; each test below
// pins one without letting it stand in for the others.

// resyncPaths runs one full resync over dir and returns the indexed paths.
func resyncPaths(t *testing.T, idx *Indexer, db *sql.DB) []string {
	t.Helper()
	if _, err := idx.processResyncChanged(context.Background()); err != nil {
		t.Fatalf("processResyncChanged: %v", err)
	}
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
	return out
}

// newTestIndexer builds an indexer over dir with the regex extractor.
func newTestIndexer(t *testing.T, dir string) (*Indexer, *sql.DB) {
	t.Helper()
	db, err := openDB(filepath.Join(dir, ".plumb", "topology.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0), db
}

func writeIndexTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

func TestResync_SkipsGitignoredTree(t *testing.T) {
	dir := t.TempDir()
	writeIndexTree(t, dir, map[string]string{
		".gitignore":       "generated/\n*_gen.go\n",
		"main.go":          "package main\nfunc Main() {}\n",
		"generated/big.go": "package generated\nfunc Big() {}\n",
		"generated/d/x.go": "package d\nfunc Deep() {}\n",
		"api/model_gen.go": "package api\nfunc Gen() {}\n",
		"api/model.go":     "package api\nfunc Model() {}\n",
	})
	idx, db := newTestIndexer(t, dir)

	got := resyncPaths(t, idx, db)
	// .gitignore is itself a walked file and gets a row like any other; the
	// point of the assertion is the three excluded paths that are absent.
	want := []string{".gitignore", "api/model.go", "main.go"}
	if !slices.Equal(got, want) {
		t.Errorf("indexed = %v, want %v", got, want)
	}
}

// TestResync_GitignoreIsAdditiveToHardcodedFloor: a repository that ignores
// nothing must still have vendor/, node_modules/ and dot-directories pruned.
func TestResync_GitignoreIsAdditiveToHardcodedFloor(t *testing.T) {
	dir := t.TempDir()
	writeIndexTree(t, dir, map[string]string{
		".gitignore":        "# ignores nothing\n",
		"main.go":           "package main\nfunc Main() {}\n",
		"vendor/dep/d.go":   "package dep\nfunc D() {}\n",
		"node_modules/n.go": "package n\nfunc N() {}\n",
		"testdata/t.go":     "package t\nfunc T() {}\n",
		".venv/v.go":        "package v\nfunc V() {}\n",
	})
	idx, db := newTestIndexer(t, dir)

	if got := resyncPaths(t, idx, db); !slices.Equal(got, []string{".gitignore", "main.go"}) {
		t.Errorf("indexed = %v, want [.gitignore main.go]", got)
	}
}

// TestResync_NewlyIgnoredFileIsPruned pins the un-indexing half: a file that
// was indexed and then became gitignored must lose its rows on the next
// resync, which is pruneDeletedChanged's job once the walk stops visiting it.
func TestResync_NewlyIgnoredFileIsPruned(t *testing.T) {
	dir := t.TempDir()
	writeIndexTree(t, dir, map[string]string{
		"main.go":       "package main\nfunc Main() {}\n",
		"gen/output.go": "package gen\nfunc Output() {}\n",
	})
	idx, db := newTestIndexer(t, dir)

	if got := resyncPaths(t, idx, db); !slices.Equal(got, []string{"gen/output.go", "main.go"}) {
		t.Fatalf("first resync = %v, want both files", got)
	}
	writeIndexTree(t, dir, map[string]string{".gitignore": "gen/\n"})
	got := resyncPaths(t, idx, db)
	if !slices.Equal(got, []string{".gitignore", "main.go"}) {
		t.Errorf("after ignoring gen/: indexed = %v, want [.gitignore main.go]", got)
	}
}

// TestResync_ExcludePatternsSkipCommittedTree is the case .gitignore cannot
// reach: a vendored tree the repository tracks on purpose. Before this change
// the setting was declared, agent-writable, and read by nothing.
func TestResync_ExcludePatternsSkipCommittedTree(t *testing.T) {
	dir := t.TempDir()
	writeIndexTree(t, dir, map[string]string{
		"main.go":              "package main\nfunc Main() {}\n",
		"third_party/lib/a.go": "package lib\nfunc A() {}\n",
		"internal/keep.go":     "package internal\nfunc Keep() {}\n",
		"scratch.go":           "package main\nfunc Scratch() {}\n",
	})
	idx, db := newTestIndexer(t, dir)
	idx.excludePatterns = sanitizeExcludePatterns(dir, []string{"third_party/**", "scratch.go"})

	got := resyncPaths(t, idx, db)
	want := []string{"internal/keep.go", "main.go"}
	if !slices.Equal(got, want) {
		t.Errorf("indexed = %v, want %v", got, want)
	}
}

// TestSanitizeExcludePatterns_RefusesCatchAll is the guard on an
// agent-writable field: `exclude_patterns = ["**"]` would prune every
// directory and let the prune pass delete every row, leaving every topology
// tool answering "nothing found" with no error anywhere.
//
// The refused list used to be a literal denylist of six spellings, and the
// second group below is what walked straight past it — each of those four
// matches every path a walk can present, and each emptied a real index end to
// end. The rule is now a probe through the same matcher the filter runs
// (ignore.ExcludePatternRefusal), which is why respelling the catch-all no
// longer helps. The third group is the other half of the claim: a guard that
// refused everything would not be a fix.
func TestSanitizeExcludePatterns_RefusesCatchAll(t *testing.T) {
	refused := []string{
		// The spellings the original denylist knew.
		"*", "**", "**/*", "*/**", ".", "./**", "/**/", "  ", "",
		// The respellings it did not, all proven to empty a real index.
		"**/**", "**/**/**", "**/**/*", "?*",
		// And the shape those four share: wildcards with nothing named.
		"?/**", "*/*/**", "**/?*",
	}
	for _, p := range refused {
		if got := sanitizeExcludePatterns("/ws", []string{p}); got != nil {
			t.Errorf("sanitizeExcludePatterns(%q) = %v, want nil", p, got)
		}
	}
	for _, p := range []string{"vendor/**", "*.pb.go", "third_party/**", "gen", "*_generated.go", "a*/**"} {
		if got := sanitizeExcludePatterns("/ws", []string{p}); !slices.Equal(got, []string{p}) {
			t.Errorf("sanitizeExcludePatterns(%q) = %v, want it kept — a guard that "+
				"refuses a legitimate pattern is not a fix", p, got)
		}
	}
	got := sanitizeExcludePatterns("/ws", []string{"**", " third_party/** ", "gen"})
	want := []string{"third_party/**", "gen"}
	if !slices.Equal(got, want) {
		t.Errorf("sanitizeExcludePatterns = %v, want %v", got, want)
	}
}

// TestSanitizeExcludePatterns_RespelledCatchAllEmptiesTheIndex is the end-to-end
// half: the four respellings above are refused because each one, if honoured,
// leaves the resync with nothing to index. Proving that here rather than only
// asserting the guard's return value is what keeps the guard tied to the
// consequence it exists to prevent.
func TestSanitizeExcludePatterns_RespelledCatchAllEmptiesTheIndex(t *testing.T) {
	for _, pattern := range []string{"**/**", "**/**/**", "**/**/*", "?*"} {
		t.Run(pattern, func(t *testing.T) {
			dir := t.TempDir()
			writeIndexTree(t, dir, map[string]string{
				"main.go":          "package main\nfunc Main() {}\n",
				"internal/keep.go": "package internal\nfunc Keep() {}\n",
			})
			idx, db := newTestIndexer(t, dir)
			// Unsanitised: what the index would hold if the guard let it through.
			idx.excludePatterns = []string{pattern}
			if got := resyncPaths(t, idx, db); len(got) != 0 {
				t.Fatalf("%q indexed %v; the premise of this test is that it excludes everything", pattern, got)
			}
			if got := sanitizeExcludePatterns(dir, []string{pattern}); got != nil {
				t.Errorf("sanitizeExcludePatterns(%q) = %v, want nil — it empties the index", pattern, got)
			}
		})
	}
}

func TestMatchesExcludePattern(t *testing.T) {
	patterns := []string{"third_party/**", "*.pb.go", "genfiles"}
	match := []string{"third_party/a.go", "third_party/x/y/z.go", "api/wire.pb.go", "genfiles", "a/b/genfiles"}
	miss := []string{"main.go", "internal/third_party_notes.md", "api/wire.go", "genfiles2"}
	for _, p := range match {
		if !matchesExcludePattern(patterns, p) {
			t.Errorf("matchesExcludePattern(%q) = false, want true", p)
		}
	}
	for _, p := range miss {
		if matchesExcludePattern(patterns, p) {
			t.Errorf("matchesExcludePattern(%q) = true, want false", p)
		}
	}
	if matchesExcludePattern(nil, "anything.go") {
		t.Error("empty pattern list must exclude nothing")
	}
}

// TestResync_RootDirectoryNameIsNotJudged: a checkout living at a path whose
// own base name is on the skip list (~/.config/repo, ~/src/build) indexed
// nothing, because the walk applied shouldSkipDir to the root itself.
func TestResync_RootDirectoryNameIsNotJudged(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "build")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIndexTree(t, dir, map[string]string{"main.go": "package main\nfunc Main() {}\n"})
	idx, db := newTestIndexer(t, dir)

	if got := resyncPaths(t, idx, db); !slices.Equal(got, []string{"main.go"}) {
		t.Errorf("indexed = %v, want [main.go] — the workspace root must not be judged by shouldSkipDir", got)
	}
}
