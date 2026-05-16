package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if err := db.Record(stats.Call{
		SessionID: "sess-1",
		Workspace: oldWorkspace,
		Tool:      "read_file",
		CalledAt:  time.Now(),
		Success:   true,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	db.Close()

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
