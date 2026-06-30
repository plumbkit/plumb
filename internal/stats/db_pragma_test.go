package stats

import "testing"

// TestOpen_AppliesBusyTimeoutAndWAL verifies the stats DB actually applies
// busy_timeout and WAL. The mattn-style `?_busy_timeout=…&_journal_mode=…` DSN
// params are SILENTLY IGNORED by the modernc driver this project uses, so the
// `_pragma=` form is required. Regression test for the stats/sessionstate DSN bug.
func TestOpen_AppliesBusyTimeoutAndWAL(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var bt int
	if err := db.db.QueryRow("PRAGMA busy_timeout").Scan(&bt); err != nil {
		t.Fatal(err)
	}
	if bt != 5000 {
		t.Errorf("busy_timeout = %d, want 5000 (the mattn-style DSN param is ignored by modernc)", bt)
	}
	var jm string
	if err := db.db.QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil {
		t.Fatal(err)
	}
	if jm != "wal" {
		t.Errorf("journal_mode = %q, want wal", jm)
	}
}
