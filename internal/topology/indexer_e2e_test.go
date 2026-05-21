package topology

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// minimalExtractor is a thin regex extractor used only for the end-to-end
// indexer test. It avoids an import cycle with extractors/golang.
type minimalExtractor struct{}

func (m *minimalExtractor) Language() string     { return "go" }
func (m *minimalExtractor) Extensions() []string { return []string{".go"} }

var reFuncDecl = regexp.MustCompile(`(?m)^func\s+(?:\([^)]+\)\s+)?(\w+)\s*\(`)

func (m *minimalExtractor) Extract(_ context.Context, relPath string, src []byte) ([]Node, []Edge, error) {
	var nodes []Node
	for _, match := range reFuncDecl.FindAllSubmatch(src, -1) {
		if len(match) < 2 {
			continue
		}
		name := string(match[1])
		nodes = append(nodes, Node{
			Kind:     KindFunction,
			Name:     name,
			Language: "go",
			Path:     relPath,
		})
	}
	return nodes, nil, nil
}

// TestIndexer_EndToEnd opens a Store against a small synthetic workspace,
// indexes one Go file, and verifies FTS search returns the indexed symbol.
// It also verifies that the token-split path works.
func TestIndexer_EndToEnd(t *testing.T) {
	dir := t.TempDir()

	// Write a synthetic Go file with a recognisable function name.
	goFile := filepath.Join(dir, "internal", "cli", "pool.go")
	if err := os.MkdirAll(filepath.Dir(goFile), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	src := []byte("package cli\n\nfunc workspacePool() {}\n\nfunc handleConn() {}\n")
	if err := os.WriteFile(goFile, src, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	dbPath := filepath.Join(dir, ".plumb", "topology.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	// Register db.Close first (runs last in LIFO order) so idx.Stop always
	// drains the background goroutine before the database connection closes.
	t.Cleanup(func() { db.Close() })

	idx := newIndexer(dir, db, []Extractor{&minimalExtractor{}}, 512*1024, 0)
	idx.Start()
	t.Cleanup(idx.Stop) // registered second → runs first (LIFO)

	// Wait for the initial resync to complete (up to 10 s).
	// We wait until both: (1) at least one node exists in the DB, and (2) the
	// indexer state is idle. This avoids the race where we read "idle" before
	// the background goroutine has been scheduled for the first time.
	deadline := time.Now().Add(10 * time.Second)
	var count int
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		db.QueryRow(`SELECT COUNT(*) FROM topology_nodes`).Scan(&count) //nolint:errcheck
		if count > 0 && idx.State() == "idle" {
			break
		}
	}
	if count == 0 {
		t.Fatalf("no nodes indexed after resync (state=%q err=%q)", idx.State(), idx.LastError())
	}
	t.Logf("indexed %d nodes", count)

	// Exact name search.
	results, err := Search(context.Background(), db, "workspacePool", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Error("search for 'workspacePool' returned 0 results")
	} else {
		t.Logf("search 'workspacePool': %d results, top=%q", len(results), results[0].Node.Name)
	}

	// Token-split search: "workspace" should match "workspacePool" via name_tokens.
	results2, err := Search(context.Background(), db, "workspace", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("token search: %v", err)
	}
	found := false
	for _, r := range results2 {
		if strings.Contains(r.Node.Name, "workspace") || strings.Contains(r.Node.Name, "Workspace") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("token search 'workspace' did not find workspacePool: got %v", results2)
	}
	t.Logf("token search 'workspace': %d results", len(results2))
}
