package sessionstate

import (
	"path/filepath"
	"testing"
)

// TestOpenAt_AppliesBusyTimeoutAndWAL verifies the session-state DB actually
// applies busy_timeout and WAL — the mattn-style `?_busy_timeout=…&_journal_mode=…`
// DSN params are silently ignored by the modernc driver, so the `_pragma=` form
// is required. Regression test for the stats/sessionstate DSN bug.
func TestOpenAt_AppliesBusyTimeoutAndWAL(t *testing.T) {
	s, err := openAt(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var bt int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&bt); err != nil {
		t.Fatal(err)
	}
	if bt != 5000 {
		t.Errorf("busy_timeout = %d, want 5000", bt)
	}
	var jm string
	if err := s.db.QueryRow("PRAGMA journal_mode").Scan(&jm); err != nil {
		t.Fatal(err)
	}
	if jm != "wal" {
		t.Errorf("journal_mode = %q, want wal", jm)
	}
}
