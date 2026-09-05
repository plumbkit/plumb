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

// openV2 hand-builds a database in the v2 shape — pinned_workspace WITH the
// source column but no session_names table, user_version=2 — so the v3
// migration is exercised against a real pre-existing file.
func openV2(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v2: %v", err)
	}
	defer db.Close()
	const v2 = `
CREATE TABLE IF NOT EXISTS pinned_workspace (
    proxy_session_id TEXT    PRIMARY KEY,
    workspace        TEXT    NOT NULL,
    language         TEXT    NOT NULL DEFAULT '',
    source           TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL
);
INSERT INTO pinned_workspace (proxy_session_id, workspace, language, source, updated_at)
VALUES ('legacy', '/tmp/legacy-root', 'go', 'roots', 1);
PRAGMA user_version = 2;
`
	if _, err := db.Exec(v2); err != nil {
		t.Fatalf("seed v2: %v", err)
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

func tableExists(t *testing.T, s *Store, table string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
	).Scan(&n); err != nil {
		t.Fatalf("lookup table %s: %v", table, err)
	}
	return n == 1
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

func TestMigration_V2ToV3AddsSessionNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session_state.db")
	openV2(t, path)

	s, err := openAt(path)
	if err != nil {
		t.Fatalf("openAt on a v2 database: %v", err)
	}
	defer s.Close()

	if !tableExists(t, s, "session_names") {
		t.Fatal("migration did not add the session_names table")
	}
	if got := userVersion(t, s); got != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, SchemaVersion)
	}

	// Pre-existing pins must survive the migration untouched.
	ws, _, _, ok, err := s.LoadPin("legacy")
	if err != nil || !ok || ws != "/tmp/legacy-root" {
		t.Fatalf("legacy pin lost: ws=%q ok=%v err=%v", ws, ok, err)
	}
}

func TestMigration_FreshDBIsCurrent(t *testing.T) {
	// A fresh database starts at user_version=0, so it passes through the same
	// migrations as a v1 file. This is why the baseline schema must NOT declare
	// the migrated shapes: it would make the v2 ALTER fail with "duplicate
	// column name" and the v3 CREATE a no-op hiding version drift.
	s := newTestStore(t)
	if !pinColumns(t, s)["source"] {
		t.Fatal("fresh database has no pinned_workspace.source")
	}
	if !tableExists(t, s, "session_names") {
		t.Fatal("fresh database has no session_names table")
	}
	if got := userVersion(t, s); got != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", got, SchemaVersion)
	}
}

func TestMigration_Idempotent(t *testing.T) {
	for _, seed := range []struct {
		name string
		open func(t *testing.T, path string)
	}{
		{"v1", openV1},
		{"v2", openV2},
	} {
		t.Run(seed.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session_state.db")
			seed.open(t, path)
			for i := range 3 {
				s, err := openAt(path)
				if err != nil {
					t.Fatalf("openAt #%d: %v", i+1, err)
				}
				s.Close()
			}
		})
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

// openV3 hand-builds a database in the v3 shape — session_names WITHOUT the
// plumb_session_id column, user_version=3 — which is the shape every daemon
// installed before mailbox identity inheritance has on disk.
func openV3(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open v3: %v", err)
	}
	defer db.Close()
	const v3 = `
CREATE TABLE IF NOT EXISTS pinned_workspace (
    proxy_session_id TEXT    PRIMARY KEY,
    workspace        TEXT    NOT NULL,
    language         TEXT    NOT NULL DEFAULT '',
    updated_at       INTEGER NOT NULL,
    source           TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS session_names (
    proxy_session_id TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL,
    updated_at       INTEGER NOT NULL
);
INSERT INTO session_names (proxy_session_id, name, updated_at)
VALUES ('legacy-proxy', 'steady-otter', 1);
PRAGMA user_version = 3;
`
	if _, err := db.Exec(v3); err != nil {
		t.Fatalf("seed v3: %v", err)
	}
}

// TestMigrateV3ToV4_NameSurvivesAndInheritsNothing. The v4 column exists so a
// reconnect can prove which session it continues. A row written before it
// existed carries no such proof, so it must restore the NAME exactly as it
// always did and hand out NO identity — an empty SessionID has to read as "no
// predecessor", never as a wildcard that would match every bound row.
func TestMigrateV3ToV4_NameSurvivesAndInheritsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	openV3(t, path)

	s, err := openAt(path)
	if err != nil {
		t.Fatalf("open (migrating) v3: %v", err)
	}
	defer s.Close()

	got, ok, err := s.LoadIdentity("legacy-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Name != "steady-otter" {
		t.Fatalf("LoadIdentity = (%+v, %v), want the v3 name preserved", got, ok)
	}
	if got.SessionID != "" {
		t.Errorf("SessionID = %q, want empty — a pre-v4 row proves no predecessor", got.SessionID)
	}

	// And the column really is usable afterwards.
	if err := s.SaveIdentity("legacy-proxy", Identity{Name: "steady-otter", SessionID: "sess-new"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.LoadIdentity("legacy-proxy"); got.SessionID != "sess-new" {
		t.Errorf("after upgrade SessionID = %q, want sess-new", got.SessionID)
	}
}
