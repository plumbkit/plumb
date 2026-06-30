package topology

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestOpenDB_PragmasApplyToEveryPooledConnection verifies foreign_keys and
// busy_timeout are set on EVERY pooled connection, not just the one a one-off
// db.Exec would touch. They are per-connection SQLite pragmas, so the topology
// DB sets them via the DSN. With FK enforcement off on a pooled connection,
// ON DELETE CASCADE silently no-ops and orphan topology_edges accumulate.
// Regression test for topo-001.
func TestOpenDB_PragmasApplyToEveryPooledConnection(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "topology.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	const n = 4
	db.SetMaxOpenConns(n)
	ctx := context.Background()

	// Hold several connections at once to force the pool to open distinct ones.
	conns := make([]*sql.Conn, 0, n)
	for i := 0; i < n; i++ {
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		conns = append(conns, c)
	}

	for i, c := range conns {
		var fk, bt int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Errorf("connection %d: foreign_keys = %d, want 1 (ON DELETE CASCADE silently no-ops when off)", i, fk)
		}
		if bt != 5000 {
			t.Errorf("connection %d: busy_timeout = %d, want 5000", i, bt)
		}
	}
}
