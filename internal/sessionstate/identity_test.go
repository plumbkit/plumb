package sessionstate

// identity_test.go — the merge, retention and reservation rules for the
// canonical durable identity record.
//
// The record is an authorisation, not a cache, and these tests are about the
// three ways that distinction shows up in the storage layer: a blank field must
// never erase a proven one, a revision must move only when the name does, and a
// name must stay reserved for as long as the identity behind it is recoverable.

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestSaveIdentity_PreservesAKnownExternalIDAgainstABlankOne(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveIdentity("p", Identity{Name: "calm-stag", SessionID: "id-1", ExternalID: "conv-1"}); err != nil {
		t.Fatal(err)
	}
	// Every reconnect re-records the identity BEFORE session_start has run, so
	// at that moment the session genuinely does not know its external ID.
	// Treating "unknown" as "none" would erase the linkage on the first save
	// after every restart.
	if err := s.SaveIdentity("p", Identity{Name: "calm-stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	rec, _, err := s.LoadIdentity("p")
	if err != nil {
		t.Fatal(err)
	}
	if rec.ExternalID != "conv-1" {
		t.Fatalf("external linkage = %q after a save that did not know it, want it preserved", rec.ExternalID)
	}
	// A non-empty value still replaces: a session that re-links to a different
	// conversation is stating a fact, not omitting one.
	if err := s.SaveIdentity("p", Identity{Name: "calm-stag", SessionID: "id-1", ExternalID: "conv-2"}); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.LoadIdentity("p"); rec.ExternalID != "conv-2" {
		t.Fatalf("external linkage = %q, want the newly stated conv-2", rec.ExternalID)
	}
}

func TestSaveIdentity_RevisionMovesOnlyWithTheName(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveIdentity("p", Identity{Name: "calm-stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	first, _, _ := s.LoadIdentity("p")

	// Re-recording the SAME name is what every reconnect does. If that bumped
	// the revision, the revision would mean "something was written" rather than
	// "the name moved", and would be useless for ordering a rename.
	for range 3 {
		if err := s.SaveIdentity("p", Identity{Name: "calm-stag", SessionID: "id-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if rec, _, _ := s.LoadIdentity("p"); rec.NameRevision != first.NameRevision {
		t.Fatalf("revision moved from %d to %d without a rename", first.NameRevision, rec.NameRevision)
	}

	if err := s.SaveIdentity("p", Identity{Name: "renamed-stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := s.LoadIdentity("p")
	if rec.NameRevision <= first.NameRevision {
		t.Fatalf("revision = %d after a rename, want more than %d — nothing then orders the "+
			"rename against a proxy snapshot holding the older name", rec.NameRevision, first.NameRevision)
	}
}

func TestReservedNames_HoldsRecoverableIdentitiesAndSkipsUnownedRows(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveIdentity("p1", Identity{Name: "Calm-Stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	// A pre-v4 row: a name with no session ID behind it. Reserving it would lock
	// the name out permanently, since no session could ever claim it as its own.
	if err := s.SaveIdentity("p2", Identity{Name: "orphan-otter"}); err != nil {
		t.Fatal(err)
	}

	records, err := s.Reservations()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Reservation{}
	for _, r := range records {
		byName[strings.ToLower(r.Name)] = r
	}
	if got := byName["calm-stag"]; got.SessionID != "id-1" {
		t.Errorf("reservation for calm-stag = %+v, want SessionID id-1", got)
	}
	if _, held := byName["orphan-otter"]; held {
		t.Error("a row with no session ID reserved its name; nobody could ever claim it back")
	}
}

// TestReservations_CarryTheExternalLinkage: the external conversation ID has to
// travel with the reservation, or a RESTARTED `plumb serve` — new proxy secret,
// new internal session ID, same conversation — has no way to prove it is the
// party the name is being held for, and the reservation locks the name away
// from its only rightful claimant.
func TestReservations_CarryTheExternalLinkage(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveIdentity("p1", Identity{Name: "calm-stag", SessionID: "id-1", ExternalID: "conv-1"}); err != nil {
		t.Fatal(err)
	}
	records, err := s.Reservations()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ExternalID != "conv-1" {
		t.Fatalf("reservations = %+v, want the external linkage carried through", records)
	}
}

func TestLegacyNameConflicts_ReportsRatherThanResolves(t *testing.T) {
	s := newTestStore(t)
	// Two retained records claiming one name — legal before retention existed,
	// since a name was unique only among LIVE sessions.
	if err := s.SaveIdentity("p1", Identity{Name: "calm-stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentity("p2", Identity{Name: "Calm-Stag", SessionID: "id-2"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIdentity("p3", Identity{Name: "lone-dingo", SessionID: "id-3"}); err != nil {
		t.Fatal(err)
	}

	conflicts, err := s.LegacyNameConflicts()
	if err != nil {
		t.Fatal(err)
	}
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly the one contested name", conflicts)
	}
	if len(conflicts[0].ProxySessionIDs) != 2 {
		t.Errorf("conflict names %v, want both claimants", conflicts[0].ProxySessionIDs)
	}

	// Reporting must not repair: both records are intact afterwards, and the
	// unaffected one is untouched. Every candidate repair is worse than the
	// ambiguity — renaming breaks mail, deleting forks an identity, and picking
	// by updated_at silently hands one session's mailbox to another.
	for _, p := range []string{"p1", "p2", "p3"} {
		if _, ok, err := s.LoadIdentity(p); err != nil || !ok {
			t.Errorf("record %s was removed or damaged by the conflict report", p)
		}
	}
}

// TestMigrateV7_BackfillsWithoutLosingLegacyRows: an upgrade must add the new
// columns to an existing v6 database and leave every recorded identity readable,
// with the new fields at their documented defaults.
func TestMigrateV7_BackfillsWithoutLosingLegacyRows(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/session_state.db"

	// Build a database as v6 left it, then close it.
	old, err := openAt(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.SaveIdentity("p1", Identity{Name: "calm-stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	// Rewind the stamp and drop the v7 columns so the next open genuinely
	// migrates rather than no-opping.
	if _, err := old.db.Exec(`ALTER TABLE session_names DROP COLUMN external_id`); err != nil {
		t.Fatalf("simulating a v6 database: %v", err)
	}
	if _, err := old.db.Exec(`ALTER TABLE session_names DROP COLUMN name_revision`); err != nil {
		t.Fatalf("simulating a v6 database: %v", err)
	}
	if _, err := old.db.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	upgraded, err := openAt(path)
	if err != nil {
		t.Fatalf("opening a v6 database with the v7 code: %v", err)
	}
	defer upgraded.Close()

	rec, ok, err := upgraded.LoadIdentity("p1")
	if err != nil || !ok {
		t.Fatalf("the legacy identity did not survive the migration: (%+v, %v, %v)", rec, ok, err)
	}
	if rec.Name != "calm-stag" || rec.SessionID != "id-1" {
		t.Fatalf("migrated record = %+v, want the original name and ID", rec)
	}
	if rec.ExternalID != "" {
		t.Errorf("external linkage = %q on a back-filled row, want empty (UNKNOWN)", rec.ExternalID)
	}
	if rec.NameRevision != 0 {
		t.Errorf("revision = %d on a back-filled row, want 0", rec.NameRevision)
	}
	if v, err := readVersion(upgraded.db); err != nil || v != SchemaVersion {
		t.Errorf("schema version = (%d, %v), want %d", v, err, SchemaVersion)
	}

	// The row remains writable and its revision starts moving from the default.
	if err := upgraded.SaveIdentity("p1", Identity{Name: "renamed-stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := upgraded.LoadIdentity("p1"); rec.NameRevision != 1 {
		t.Errorf("revision = %d after the first post-migration rename, want 1", rec.NameRevision)
	}
}

// TestPrune_LeavesIdentityRowsAtEveryAge states the retention rule directly
// against the table, so a future DELETE added to Prune fails here rather than in
// a distant reconnect test.
func TestPrune_LeavesIdentityRowsAtEveryAge(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveIdentity("p", Identity{Name: "calm-stag", SessionID: "id-1"}); err != nil {
		t.Fatal(err)
	}
	// A year old, and no live exemption offered — the shape of the startup
	// sweep, which runs before any connection exists.
	old := time.Now().Add(-365 * 24 * time.Hour).UnixMilli()
	if _, err := s.db.Exec(`UPDATE session_names SET updated_at=?`, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Prune(time.Now()); err != nil {
		t.Fatal(err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM session_names`).Scan(&n); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("session_names holds %d rows after a sweep past their age, want 1 — an identity "+
			"is the proof of who a reconnecting serve is, and age is no evidence that a serve died", n)
	}
}
