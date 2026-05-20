package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func insertTestNode(t *testing.T, db *sql.DB, fileID int64, relPath string, n Node) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO topology_nodes(file_id, kind, name, qualified, signature, start_line, end_line, docstring, language)
         VALUES (?,?,?,?,?,?,?,?,?)`,
		fileID, string(n.Kind), n.Name, n.Qualified, n.Signature, n.StartLine, n.EndLine, n.Docstring, n.Language)
	if err != nil {
		t.Fatalf("insert node %q: %v", n.Name, err)
	}
	id, _ := res.LastInsertId()
	// Also insert into FTS so searches work.
	tokens := splitIdentifier(n.Name)
	if _, err := db.Exec(
		`INSERT INTO topology_fts(rowid, name, name_tokens, qualified, signature, docstring, path, kind)
         VALUES (?,?,?,?,?,?,?,?)`,
		id, n.Name, tokens, n.Qualified, n.Signature, n.Docstring, relPath, string(n.Kind)); err != nil {
		t.Fatalf("insert fts for node %q: %v", n.Name, err)
	}
	return id
}

func insertTestEdge(t *testing.T, db *sql.DB, fromID, toID int64, kind string) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO topology_edges(from_id, to_id, kind, confidence, source) VALUES (?,?,?,?,?)`,
		fromID, toID, kind, 1.0, "test"); err != nil {
		t.Fatalf("insert edge %d→%d: %v", fromID, toID, err)
	}
}

func TestExplore_BFS_Depth2(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "exp.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "a.go")
	n1 := insertTestNode(t, db, fileID, "a.go", Node{Kind: KindPackage, Name: "mypkg", Language: "go"})
	n2 := insertTestNode(t, db, fileID, "a.go", Node{Kind: KindFunction, Name: "Alpha", Language: "go"})
	n3 := insertTestNode(t, db, fileID, "a.go", Node{Kind: KindFunction, Name: "Beta", Language: "go"})
	insertTestEdge(t, db, n1, n2, string(EdgeContains))
	insertTestEdge(t, db, n2, n3, string(EdgeContains))

	nb, err := Explore(context.Background(), db, "mypkg", ExploreOpts{Depth: 2, MaxNodes: 50, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("Explore: %v", err)
	}
	if len(nb.Nodes) < 2 {
		t.Errorf("expected ≥2 neighbours, got %d", len(nb.Nodes))
	}
	if nb.Truncated {
		t.Error("unexpected truncation with 3 nodes and max=50")
	}
}

func TestExplore_MaxNodesTruncation(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "trunc.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "b.go")
	n1 := insertTestNode(t, db, fileID, "b.go", Node{Kind: KindPackage, Name: "pkg", Language: "go"})
	for i := 0; i < 5; i++ {
		nx := insertTestNode(t, db, fileID, "b.go", Node{Kind: KindFunction, Name: "fn", Language: "go"})
		insertTestEdge(t, db, n1, nx, string(EdgeContains))
	}

	nb, err := Explore(context.Background(), db, "pkg", ExploreOpts{Depth: 2, MaxNodes: 2, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("Explore: %v", err)
	}
	if !nb.Truncated {
		t.Error("expected Truncated=true with max_nodes=2 and 5 children")
	}
}

func TestExplore_UnknownSymbol(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "unk.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	_, err = Explore(context.Background(), db, "doesNotExist", ExploreOpts{Depth: 2})
	if err == nil {
		t.Error("expected error for unknown symbol, got nil")
	}
}

func TestExplore_HardCaps(t *testing.T) {
	opts := clampOpts(ExploreOpts{Depth: 99, MaxNodes: 99999, MaxBytes: 99999999})
	if opts.Depth != hardCapDepth {
		t.Errorf("Depth not clamped: got %d, want %d", opts.Depth, hardCapDepth)
	}
	if opts.MaxNodes != hardCapNodes {
		t.Errorf("MaxNodes not clamped: got %d, want %d", opts.MaxNodes, hardCapNodes)
	}
	if opts.MaxBytes != hardCapBytes {
		t.Errorf("MaxBytes not clamped: got %d, want %d", opts.MaxBytes, hardCapBytes)
	}
}

func TestExplore_Defaults(t *testing.T) {
	opts := clampOpts(ExploreOpts{})
	if opts.Depth != defaultDepth {
		t.Errorf("Depth default: got %d, want %d", opts.Depth, defaultDepth)
	}
	if opts.MaxNodes != defaultMaxNodes {
		t.Errorf("MaxNodes default: got %d, want %d", opts.MaxNodes, defaultMaxNodes)
	}
	if opts.MaxBytes != defaultMaxBytes {
		t.Errorf("MaxBytes default: got %d, want %d", opts.MaxBytes, defaultMaxBytes)
	}
}
