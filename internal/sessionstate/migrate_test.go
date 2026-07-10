package sessionstate

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// openV1 hand-builds a database in the v1 shape — pinned_workspace without the
// source column, user_version=1 — so the migration is exercised against a real
// pre-existing file rather than a freshly created one. Every installed daemon
// has a database in exactly this shape.
func openV1(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v1: %v", err)
	}
	defer db.Close()
	const v1 = `
CREATE TABLE IF NOT EXISTS pinned_workspace (
    proxy_session_id TEXT    PRIMARY KEY,
    workspace        TEXT    NOT NULL,
    language         TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL
);
INSERT INTO pinned_workspace (proxy_session_id, workspace, language, updated_at)
VALUES ('legacy', '/tmp/legacy-root', 'go', 1);
PRAGMA user_version = 1;
`
	if _, err := db.Exec(v1); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
}

func pinColumns(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.db.Query("PRAGMA table_info(pinned_workspace)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	return cols
}

func userVersion(t *testing.T, s *Store) int {
	t.Helper()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func TestMigration_V1ToV2AddsSourceColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_state.db")
	openV1(t, path)

	s, err := openAt(path)
	if err != nil {
		t.Fatalf("openAt on a v1 database: %v", err)
	}
	defer s.Close()

	if !pinColumns(t, s)["source"] {
		t.Fatal("migration did not add pinned_workspace.source")
	}
	if got := userVersion(t, s); got != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, SchemaVersion)
	}

	// A row written before the column existed must survive, reading as the
	// unknown origin — which deliberately does NOT outrank client roots, so an
	// upgrade changes no behaviour until the next deliberate re-pin.
	ws, lang, src, ok, err := s.LoadPin("legacy")
	if err != nil || !ok {
		t.Fatalf("legacy pin lost: ok=%v err=%v", ok, err)
	}
	if ws != "/tmp/legacy-root" || lang != "go" {
		t.Fatalf("legacy pin corrupted: %q %q", ws, lang)
	}
	if src != PinSourceUnknown {
		t.Fatalf("legacy pin source = %q, want empty (unknown origin)", src)
	}
}

func TestMigration_FreshDBIsV2(t *testing.T) {
	// A fresh database starts at user_version=0, so it passes through the same
	// migration as a v1 file. This is why the baseline schema must NOT declare
	// the column: it would make the ALTER fail with "duplicate column name".
	s := newTestStore(t)
	if !pinColumns(t, s)["source"] {
		t.Fatal("fresh database has no pinned_workspace.source")
	}
	if got := userVersion(t, s); got != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, SchemaVersion)
	}
}

func TestMigration_Idempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_state.db")
	openV1(t, path)
	for i := range 3 {
		s, err := openAt(path)
		if err != nil {
			t.Fatalf("openAt #%d: %v", i+1, err)
		}
		s.Close()
	}
}

func TestPinRoundTripsSource(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertPin("proxyX", "/tmp/root", "go", PinSourceSessionStart); err != nil {
		t.Fatalf("UpsertPin: %v", err)
	}
	_, _, src, ok, err := s.LoadPin("proxyX")
	if err != nil || !ok {
		t.Fatalf("LoadPin: ok=%v err=%v", ok, err)
	}
	if src != PinSourceSessionStart {
		t.Fatalf("source = %q, want %q", src, PinSourceSessionStart)
	}
}
