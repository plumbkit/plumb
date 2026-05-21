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
	for range 5 {
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

func TestExplore_EdgeKindFilter(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "ek.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "c.go")
	n1 := insertTestNode(t, db, fileID, "c.go", Node{Kind: KindPackage, Name: "pkg", Language: "go"})
	n2 := insertTestNode(t, db, fileID, "c.go", Node{Kind: KindFunction, Name: "Fn", Language: "go"})
	n3 := insertTestNode(t, db, fileID, "c.go", Node{Kind: KindFunction, Name: "Callee", Language: "go"})
	insertTestEdge(t, db, n1, n2, string(EdgeContains))
	insertTestEdge(t, db, n2, n3, string(EdgeCalls))

	// Filter to only "contains" edges — should not traverse the "calls" edge.
	nb, err := Explore(context.Background(), db, "pkg", ExploreOpts{
		Depth:     2,
		MaxNodes:  50,
		MaxBytes:  100000,
		EdgeKinds: []string{string(EdgeContains)},
	})
	if err != nil {
		t.Fatalf("Explore: %v", err)
	}
	// Fn should be reachable via contains; Callee should NOT appear (only via calls).
	nameSet := map[string]bool{}
	for _, n := range nb.Nodes {
		nameSet[n.Name] = true
	}
	if !nameSet["Fn"] {
		t.Error("expected Fn in neighbourhood via 'contains' edge")
	}
	if nameSet["Callee"] {
		t.Error("Callee should not appear when filtering to 'contains' edges only")
	}
}

func TestExplore_NoDanglingEdgesOnTruncation(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "de.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "d.go")
	centre := insertTestNode(t, db, fileID, "d.go", Node{Kind: KindPackage, Name: "hub", Language: "go"})
	// Create more children than MaxNodes=1 allows.
	for range 3 {
		child := insertTestNode(t, db, fileID, "d.go", Node{Kind: KindFunction, Name: "fn", Language: "go"})
		insertTestEdge(t, db, centre, child, string(EdgeContains))
	}

	nb, err := Explore(context.Background(), db, "hub", ExploreOpts{Depth: 2, MaxNodes: 1, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("Explore: %v", err)
	}
	if !nb.Truncated {
		t.Error("expected Truncated=true")
	}
	// Build the set of node IDs in output (centre + nb.Nodes).
	outputIDs := map[int64]bool{nb.Centre.ID: true}
	for _, n := range nb.Nodes {
		outputIDs[n.ID] = true
	}
	// Every edge must reference only nodes that are in the output set.
	for _, e := range nb.Edges {
		if !outputIDs[e.FromID] {
			t.Errorf("edge %d: FromID %d not in output set", e.ID, e.FromID)
		}
		if !outputIDs[e.ToID] {
			t.Errorf("edge %d: ToID %d not in output set", e.ID, e.ToID)
		}
	}
}

func TestExplore_NoEdgeDuplicates(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "dup.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "e.go")
	n1 := insertTestNode(t, db, fileID, "e.go", Node{Kind: KindPackage, Name: "pkgX", Language: "go"})
	n2 := insertTestNode(t, db, fileID, "e.go", Node{Kind: KindFunction, Name: "A", Language: "go"})
	n3 := insertTestNode(t, db, fileID, "e.go", Node{Kind: KindFunction, Name: "B", Language: "go"})
	insertTestEdge(t, db, n1, n2, string(EdgeContains))
	insertTestEdge(t, db, n1, n3, string(EdgeContains))
	insertTestEdge(t, db, n2, n3, string(EdgeCalls))

	nb, err := Explore(context.Background(), db, "pkgX", ExploreOpts{Depth: 3, MaxNodes: 50, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("Explore: %v", err)
	}
	seen := map[int64]int{}
	for _, e := range nb.Edges {
		seen[e.ID]++
	}
	for id, count := range seen {
		if count > 1 {
			t.Errorf("edge %d appears %d times in output, want 1", id, count)
		}
	}
}

func TestExplore_MaxNodesSemantics(t *testing.T) {
	// MaxNodes caps nb.Nodes (neighbours); the centre is not counted against the
	// budget. With MaxNodes=3 and 5 children, we get exactly 3 in nb.Nodes.
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "sem.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "s.go")
	centre := insertTestNode(t, db, fileID, "s.go", Node{Kind: KindPackage, Name: "root", Language: "go"})
	for i := 0; i < 5; i++ {
		child := insertTestNode(t, db, fileID, "s.go", Node{Kind: KindFunction, Name: "fn", Language: "go"})
		insertTestEdge(t, db, centre, child, string(EdgeContains))
	}

	const maxNodes = 3
	nb, err := Explore(context.Background(), db, "root", ExploreOpts{Depth: 1, MaxNodes: maxNodes, MaxBytes: 100000})
	if err != nil {
		t.Fatalf("Explore: %v", err)
	}
	if !nb.Truncated {
		t.Error("expected Truncated=true with 5 children and MaxNodes=3")
	}
	if len(nb.Nodes) > maxNodes {
		t.Errorf("len(nb.Nodes) = %d, must be ≤ MaxNodes=%d", len(nb.Nodes), maxNodes)
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
