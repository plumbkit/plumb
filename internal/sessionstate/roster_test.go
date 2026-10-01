package sessionstate

// roster_test.go — the session row a logical agent holds on a shared connection,
// recorded so it survives a restart and its name stays reserved meanwhile (#526).

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func logicalAgentColumns(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	rows, err := s.db.Query("PRAGMA table_info(logical_agent)")
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		cols[name] = true
	}
	return cols
}

func TestRosterIdentity_RoundTripsUnderTheProxySessionAndAgent(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordRosterIdentity("proxyX", "conv/agent", "calm-stag", "id-1"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.RosterIdentityFor("proxyX", "conv/agent")
	if err != nil || !ok || got.Name != "calm-stag" || got.SessionID != "id-1" {
		t.Fatalf("RosterIdentityFor = %+v ok=%v err=%v, want calm-stag/id-1", got, ok, err)
	}
	// The key is BOTH halves: the same agent id under another proxy session, and
	// another agent under this one, find nothing.
	for _, k := range [][2]string{{"proxyY", "conv/agent"}, {"proxyX", "conv/other"}, {"", "conv/agent"}} {
		if got, ok, _ := s.RosterIdentityFor(k[0], k[1]); ok {
			t.Errorf("RosterIdentityFor(%q, %q) = %+v, want nothing", k[0], k[1], got)
		}
	}
	// A half-recorded identity would reserve a name nobody could claim.
	for _, r := range [][4]string{{"proxyZ", "a", "", "id"}, {"proxyZ", "a", "name", ""}, {"proxyZ", "", "name", "id"}, {"", "a", "name", "id"}} {
		if err := s.RecordRosterIdentity(r[0], r[1], r[2], r[3]); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := s.Reservations(); err != nil || len(res) != 1 {
		t.Fatalf("Reservations = %+v err=%v, want only the one whole identity", res, err)
	}
}

// RecordLogicalAgent runs on every stamped call, and its upsert must refresh the
// declaration without wiping the identity recorded beside it.
func TestRecordLogicalAgentDoesNotClobberTheRosterIdentity(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordRosterIdentity("proxyX", "a", "calm-stag", "id-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordLogicalAgent("proxyX", "a"); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := s.RosterIdentityFor("proxyX", "a"); !ok || got.Name != "calm-stag" || got.SessionID != "id-1" {
		t.Fatalf("a re-observed agent lost its roster identity: %+v ok=%v", got, ok)
	}
	// And the other direction: recording an identity on an observed row keeps the row.
	if err := s.RecordLogicalAgent("proxyX", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordRosterIdentity("proxyX", "b", "lone-dingo", "id-2"); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.LogicalAgentIDsFor("proxyX", time.Now().Add(-time.Hour))
	if len(ids) != 2 {
		t.Fatalf("LogicalAgentIDsFor = %v, want both agents", ids)
	}
}

func TestReservations_IncludeTheRosterNamesOfAgentsAway(t *testing.T) {
	s := newTestStore(t)
	if err := s.RecordRosterIdentity("proxyX", "conv/agent", "Calm-Stag", "id-1"); err != nil {
		t.Fatal(err)
	}
	// An observed-only row holds no identity and so reserves nothing.
	if err := s.RecordLogicalAgent("proxyX", "conv/silent"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Reservations()
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !strings.EqualFold(res[0].Name, "calm-stag") || res[0].SessionID != "id-1" || res[0].ExternalID != "" {
		t.Fatalf("Reservations = %+v, want one: calm-stag held for id-1, no external id", res)
	}
}

// The roster names are held for as long as the agent can come back and no longer:
// the pool is finite, so the TTL prune that reclaims the row releases the name,
// unless the connection it belongs to is live.
func TestReservations_ARosterNameIsReleasedByThePrune(t *testing.T) {
	s := newTestStore(t)
	for _, proxy := range []string{"gone", "live"} {
		if err := s.RecordRosterIdentity(proxy, "a", "name-"+proxy, "id-"+proxy); err != nil {
			t.Fatal(err)
		}
		if err := s.BackdateSession(proxy, time.Now().Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(time.Now().Add(-24*time.Hour), "live"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Reservations()
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Name != "name-live" {
		t.Fatalf("Reservations after the prune = %+v, want only the live connection's name", res)
	}
	if _, ok, _ := s.RosterIdentityFor("gone", "a"); ok {
		t.Error("the pruned connection's roster identity is still restorable")
	}
}

// openV9 builds a database in the v9 shape: a current one with the v10 columns
// dropped again and the version put back, holding one observed agent. With
// keepColumns the columns stay, which is a step that ran and was never stamped.
func openV9(t *testing.T, path string, keepColumns bool) {
	t.Helper()
	s, err := openAt(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.RecordLogicalAgent("legacy", "conv/agent"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	stmts := []string{`PRAGMA user_version = 9`}
	if !keepColumns {
		stmts = append(stmts,
			`ALTER TABLE logical_agent DROP COLUMN roster_name`,
			`ALTER TABLE logical_agent DROP COLUMN roster_session_id`)
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestMigrateV9ToV10_AddsTheRosterColumnsAndKeepsTheRows(t *testing.T) {
	for _, keepColumns := range []bool{false, true} {
		name := "a v9 database"
		if keepColumns {
			name = "a v9 database whose step already added the columns"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			openV9(t, path, keepColumns)
			for i := range 2 { // the second open is the idempotence check
				s, err := openAt(path)
				if err != nil {
					t.Fatalf("open #%d: %v", i+1, err)
				}
				if got := userVersion(t, s); got != SchemaVersion || SchemaVersion != 10 {
					t.Fatalf("user_version = %d, SchemaVersion = %d, want 10", got, SchemaVersion)
				}
				cols := logicalAgentColumns(t, s)
				if !cols["roster_name"] || !cols["roster_session_id"] {
					t.Fatalf("logical_agent columns = %v, want roster_name and roster_session_id", cols)
				}
				if ids, _ := s.LogicalAgentIDsFor("legacy", time.UnixMilli(0)); len(ids) != 1 {
					t.Fatalf("the pre-v10 row did not survive: %v", ids)
				}
				if _, ok, _ := s.RosterIdentityFor("legacy", "conv/agent"); ok != (i == 1) {
					t.Fatalf("open #%d: a roster identity present = %v", i+1, ok)
				}
				if res, _ := s.Reservations(); len(res) != i {
					t.Fatalf("open #%d: reservations = %+v", i+1, res)
				}
				// The first open adds nothing the second must undo, and a recorded
				// identity survives the re-open.
				if i == 0 {
					if err := s.RecordRosterIdentity("legacy", "conv/agent", "calm-stag", "id-1"); err != nil {
						t.Fatal(err)
					}
				}
				s.Close()
			}
		})
	}
}

func TestMigrateV10_FreshDatabaseHasTheColumns(t *testing.T) {
	s := newTestStore(t)
	cols := logicalAgentColumns(t, s)
	if !cols["roster_name"] || !cols["roster_session_id"] {
		t.Fatalf("a fresh database lacks the roster columns: %v", cols)
	}
	if got := userVersion(t, s); got != 10 {
		t.Fatalf("user_version = %d, want 10", got)
	}
}
