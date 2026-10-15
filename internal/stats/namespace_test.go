package stats

import (
	"testing"
	"time"
)

func openNSTestDB(t *testing.T) *DB {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func recordNS(t *testing.T, db *DB, ns, sid, tool string) {
	t.Helper()
	if err := db.Record(Call{Namespace: ns, SessionID: sid, SessionName: sid, Workspace: "/w", Tool: tool, CalledAt: time.Now(), Success: true, InputJSON: `{"file_path":"/w/a.go"}`}); err != nil {
		t.Fatal(err)
	}
}

// TestStatsNamespace_ReadsSeeOneNamespace: every Filter-based read and the
// recent-writes feed see only their own namespace; the zero Filter is the
// default namespace, so a namespaced run never shows in the user's stats.
func TestStatsNamespace_ReadsSeeOneNamespace(t *testing.T) {
	db := openNSTestDB(t)
	recordNS(t, db, "", "sid-d", "write_file")
	recordNS(t, db, "ns-1", "sid-n", "write_file")
	recordNS(t, db, "ns-1", "sid-n", "edit_file")

	if got := db.TotalCalls(Filter{}); got != 1 {
		t.Fatalf("default TotalCalls = %d, want 1", got)
	}
	if got := db.TotalCalls(Filter{Namespace: "ns-1"}); got != 2 {
		t.Fatalf("ns-1 TotalCalls = %d, want 2 (positive control)", got)
	}
	if got := db.TotalSessions(Filter{}); got != 1 {
		t.Fatalf("default TotalSessions = %d, want 1", got)
	}
	for _, tc := range []struct {
		ns   string
		want string
	}{{"", "sid-d"}, {"ns-1", "sid-n"}} {
		writes, err := db.RecentWritesByWorkspace("/w", tc.ns, []string{"write_file", "edit_file"}, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(writes) == 0 {
			t.Fatalf("namespace %q: no writes (positive control)", tc.ns)
		}
		for _, w := range writes {
			if w.SessionID != tc.want {
				t.Fatalf("namespace %q feed shows %s's write", tc.ns, w.SessionID)
			}
		}
	}
}

// TestStatsNamespace_EpisodicAndPurge: a summary is seen only in its namespace,
// and a purge removes the namespace's calls and summaries and nothing else.
func TestStatsNamespace_EpisodicAndPurge(t *testing.T) {
	db := openNSTestDB(t)
	recordNS(t, db, "", "sid-d", "read_file")
	recordNS(t, db, "ns-1", "sid-n", "read_file")
	for _, e := range []Episodic{
		{Workspace: "/w", SessionID: "sid-d", Summary: "default work", GeneratedAt: time.Now()},
		{Namespace: "ns-1", Workspace: "/w", SessionID: "sid-n", Summary: "e2e work", GeneratedAt: time.Now().Add(time.Second)},
	} {
		if err := db.recordEpisodic(e); err != nil {
			t.Fatal(err)
		}
	}
	if e, ok, _ := db.LatestEpisodic("/w", ""); !ok || e.Summary != "default work" {
		t.Fatalf("default sees %q, want its own summary", e.Summary)
	}
	if e, ok, _ := db.LatestEpisodic("/w", "ns-1"); !ok || e.Summary != "e2e work" {
		t.Fatalf("ns-1 sees %q, want its own summary", e.Summary)
	}
	if _, err := db.PurgeNamespace(""); err == nil {
		t.Fatal("purging the default namespace was allowed")
	}
	n, err := db.PurgeNamespace("ns-1")
	if err != nil || n != 1 {
		t.Fatalf("purge = %d, %v; want 1 call", n, err)
	}
	if got := db.TotalCalls(Filter{Namespace: "ns-1"}); got != 0 {
		t.Fatalf("%d ns-1 calls survived the purge", got)
	}
	if _, ok, _ := db.LatestEpisodic("/w", "ns-1"); ok {
		t.Fatal("ns-1 summary survived the purge")
	}
	if got := db.TotalCalls(Filter{}); got != 1 {
		t.Fatalf("the purge touched the default namespace: %d calls left, want 1", got)
	}
}

// TestStatsNamespace_HealthIsTheDefaultNamespaces: health metrics count the
// user's work, not a namespaced end-to-end run's.
func TestStatsNamespace_HealthIsTheDefaultNamespaces(t *testing.T) {
	where, args := dayWhere(0, 1, "")
	if len(args) != 2 || !contains(where, "namespace = ''") {
		t.Fatalf("dayWhere = %q %v, want the default-namespace condition", where, args)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestStatsNamespace_FirstCallIsTheDefaults: the TUI's lifetime start is the
// user's first call, not an earlier namespaced run's.
func TestStatsNamespace_FirstCallIsTheDefaults(t *testing.T) {
	db := openNSTestDB(t)
	early := time.Now().Add(-time.Hour)
	if err := db.Record(Call{Namespace: "ns-1", SessionID: "n", Workspace: "/w", Tool: "read_file", CalledAt: early, Success: true}); err != nil {
		t.Fatal(err)
	}
	recordNS(t, db, "", "d", "read_file")
	if got := db.FirstCallAt(); !got.After(early) {
		t.Fatalf("FirstCallAt = %v, the namespaced call's time; want the default namespace's first call", got)
	}
}

// TestStatsNamespace_SessionFilterFindsItsRows: a filter naming one session
// finds that session's rows in whichever namespace it lives (daemon_info's
// per-session latency for a namespaced session).
func TestStatsNamespace_SessionFilterFindsItsRows(t *testing.T) {
	db := openNSTestDB(t)
	recordNS(t, db, "ns-1", "sid-n", "read_file")
	if got := db.TotalCalls(Filter{SessionID: "sid-n"}); got != 1 {
		t.Fatalf("a session filter found %d of its namespaced session's calls, want 1", got)
	}
}
