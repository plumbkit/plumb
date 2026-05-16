package cli

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/golimpio/plumb/internal/stats"
)

func TestRunStats_ShowsRowsAfterWorkspaceMove(t *testing.T) {
	parent := t.TempDir()
	oldWorkspace := filepath.Join(parent, "old", "plumb")
	newWorkspace := filepath.Join(parent, "new", "plumb")

	db, err := stats.Open(stats.DBPathFor(oldWorkspace))
	if err != nil {
		t.Fatalf("Open old workspace DB: %v", err)
	}
	db.Close()
	seedOldWorkspaceRow(t, stats.DBPathFor(oldWorkspace), oldWorkspace)

	if err := os.MkdirAll(filepath.Dir(newWorkspace), 0o755); err != nil {
		t.Fatalf("MkdirAll new parent: %v", err)
	}
	if err := os.Rename(oldWorkspace, newWorkspace); err != nil {
		t.Fatalf("Rename workspace: %v", err)
	}

	oldWorkspaceFlag, oldLimit := statsFlagWorkspace, statsFlagLimit
	statsFlagWorkspace, statsFlagLimit = newWorkspace, 5
	defer func() {
		statsFlagWorkspace, statsFlagLimit = oldWorkspaceFlag, oldLimit
	}()

	out := captureStdout(t, func() {
		if err := runStats(nil, nil); err != nil {
			t.Fatalf("runStats: %v", err)
		}
	})

	if strings.Contains(out, "No statistics") {
		t.Fatalf("runStats reported no statistics after workspace move:\n%s", out)
	}
	if !strings.Contains(out, "read_file") {
		t.Fatalf("runStats output did not include moved DB row:\n%s", out)
	}
}

func seedOldWorkspaceRow(t *testing.T, dbPath, oldWorkspace string) {
	t.Helper()

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite DB: %v", err)
	}
	defer raw.Close()

	if _, err := raw.Exec(`ALTER TABLE tool_calls ADD COLUMN workspace TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("add old workspace column: %v", err)
	}

	_, err = raw.Exec(
		`INSERT INTO tool_calls
		 (session_id, workspace, tool, called_at, duration_ms, input_bytes, output_bytes, success, error_msg, input_json, output_text)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sess-1", oldWorkspace, "read_file", time.Now().UnixMilli(), 1, 0, 0, 1, "", "", "",
	)
	if err != nil {
		t.Fatalf("insert old workspace row: %v", err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = orig
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("closing stdout pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading stdout pipe: %v", err)
	}
	return string(out)
}
