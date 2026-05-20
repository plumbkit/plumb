package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// insertNodeWithFTS inserts a node and its FTS entry, returning the node ID.
func insertNodeWithFTS(t *testing.T, db *sql.DB, fileID int64, relPath string, n Node) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO topology_nodes(file_id, kind, name, qualified, signature, start_line, end_line, docstring, language)
         VALUES (?,?,?,?,?,?,?,?,?)`,
		fileID, string(n.Kind), n.Name, n.Qualified, n.Signature, n.StartLine, n.EndLine, n.Docstring, n.Language)
	if err != nil {
		t.Fatalf("insert node %q: %v", n.Name, err)
	}
	id, _ := res.LastInsertId()
	tokens := splitIdentifier(n.Name)
	if _, err := db.Exec(
		`INSERT INTO topology_fts(rowid, name, name_tokens, qualified, signature, docstring, path, kind)
         VALUES (?,?,?,?,?,?,?,?)`,
		id, n.Name, tokens, n.Qualified, n.Signature, n.Docstring, relPath, string(n.Kind)); err != nil {
		t.Fatalf("insert fts for %q: %v", n.Name, err)
	}
	return id
}

func TestSearch_ByName(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "s.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "internal/foo/foo.go")
	for _, n := range []Node{
		{Kind: KindFunction, Name: "HandleRequest", Language: "go", StartLine: 10, EndLine: 20},
		{Kind: KindFunction, Name: "parseArgs", Language: "go", StartLine: 30, EndLine: 40},
		{Kind: KindType, Name: "RequestHandler", Language: "go", StartLine: 50, EndLine: 60},
	} {
		insertNodeWithFTS(t, db, fileID, "internal/foo/foo.go", n)
	}

	results, err := Search(context.Background(), db, "HandleRequest", SearchOpts{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result, got 0")
	}
	if results[0].Node.Name != "HandleRequest" {
		t.Errorf("first result name = %q, want %q", results[0].Node.Name, "HandleRequest")
	}
}

func TestSearch_ByToken(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "tok.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "pkg/ws/pool.go")
	n := Node{Kind: KindType, Name: "workspacePool", Language: "go", StartLine: 5, EndLine: 10}
	insertNodeWithFTS(t, db, fileID, "pkg/ws/pool.go", n)

	tokens := splitIdentifier("workspacePool")
	if tokens == "" {
		t.Error("splitIdentifier(workspacePool) returned empty string")
	}

	// Search by the split token "workspace" — should find workspacePool via name_tokens.
	results, err := Search(context.Background(), db, "workspace", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result for token search, got 0")
	}
}

func TestSearch_EmptyQuery(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "empty.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	_, err = Search(context.Background(), db, "", SearchOpts{Limit: 10})
	if err == nil {
		t.Error("expected error for empty query, got nil")
	}
}

func TestSearch_KindFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "kf.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "a.go")
	insertNodeWithFTS(t, db, fileID, "a.go", Node{Kind: KindFunction, Name: "Foo", Language: "go", StartLine: 1, EndLine: 2})
	insertNodeWithFTS(t, db, fileID, "a.go", Node{Kind: KindType, Name: "FooType", Language: "go", StartLine: 3, EndLine: 4})

	results, err := Search(context.Background(), db, "Foo", SearchOpts{
		Limit: 10,
		Kinds: []string{"function"},
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range results {
		if string(r.Node.Kind) != "function" {
			t.Errorf("got kind %q after kind filter, want function", r.Node.Kind)
		}
	}
}

func TestSearch_RankOrder(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "rank.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "rank.go")
	insertNodeWithFTS(t, db, fileID, "rank.go", Node{Kind: KindFunction, Name: "exactMatch", Language: "go", StartLine: 1})
	insertNodeWithFTS(t, db, fileID, "rank.go", Node{Kind: KindFunction, Name: "notMatch", Language: "go", StartLine: 5})

	results, err := Search(context.Background(), db, "exactMatch", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results")
	}
	// The score for the first result should be >= any subsequent result (higher score = better).
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[0].Score {
			t.Errorf("result[%d].Score (%f) > result[0].Score (%f) — not ranked by score", i, results[i].Score, results[0].Score)
		}
	}
}
