package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var sqlSrc = []byte(`CREATE TABLE users (
    id SERIAL PRIMARY KEY,
    email VARCHAR(255) NOT NULL UNIQUE,
    created_at TIMESTAMP DEFAULT NOW()
);

CREATE INDEX idx_users_email ON users (email);

CREATE VIEW active_users AS
SELECT id, email FROM users WHERE created_at > NOW();

INSERT INTO users (email) VALUES ('a@example.com');

SELECT u.id FROM users u ORDER BY u.created_at DESC;

UPDATE users SET email = 'b@example.com' WHERE id = 1;

DELETE FROM users WHERE id = 2;
`)

func TestSQL_KindsExtracted(t *testing.T) {
	nodes, _, err := NewSQL().Extract(context.Background(), "schema/init.sql", sqlSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindType, "users"},           // table
		{topology.KindType, "idx_users_email"}, // index
		{topology.KindType, "active_users"},    // view
		{topology.KindField, "id"},
		{topology.KindField, "email"},
		{topology.KindField, "created_at"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestSQL_DMLNotExtracted(t *testing.T) {
	nodes, _, err := NewSQL().Extract(context.Background(), "init.sql", sqlSrc)
	if err != nil {
		t.Fatal(err)
	}
	// INSERT/SELECT/UPDATE/DELETE are operations, not declarations. The only
	// type nodes are the table, index and view — three in total.
	if got := names(nodes, topology.KindType); len(got) != 3 {
		t.Errorf("want exactly 3 type nodes (table/index/view), got %v", got)
	}
}

func TestSQL_ColumnContainmentCertain(t *testing.T) {
	nodes, edges, err := NewSQL().Extract(context.Background(), "init.sql", sqlSrc)
	if err != nil {
		t.Fatal(err)
	}
	var tableIdx, emailIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindType && n.Name == "users":
			tableIdx = int64(i)
		case n.Kind == topology.KindField && n.Name == "email":
			emailIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == tableIdx && e.ToID == emailIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge users→email; edges=%v", edges)
}

func TestSQL_QualifiedColumnName(t *testing.T) {
	nodes, _, err := NewSQL().Extract(context.Background(), "init.sql", sqlSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindField && n.Name == "email" {
			if n.Qualified != "users.email" {
				t.Errorf("email Qualified=%q, want users.email", n.Qualified)
			}
			return
		}
	}
	t.Fatal("email field not found")
}

func TestSQL_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("-- just a comment\n-- more\n")} {
		nodes, edges, err := NewSQL().Extract(context.Background(), "e.sql", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestSQL_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewSQL().Extract(context.Background(), "schema/init.sql", sqlSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "sql" {
			t.Errorf("node %q language=%q, want sql", n.Name, n.Language)
		}
		if n.Path != "schema/init.sql" {
			t.Errorf("node %q path=%q, want schema/init.sql", n.Name, n.Path)
		}
	}
}

func TestSQL_Extensions(t *testing.T) {
	if !slices.Contains(NewSQL().Extensions(), ".sql") {
		t.Error(".sql missing from SQL Extensions()")
	}
}
