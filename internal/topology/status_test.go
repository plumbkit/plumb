package topology

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReport_EmptyDB(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "status.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	s := Report(db, dir, nil)
	if s.IndexerState != "stopped" {
		t.Errorf("state = %q, want stopped", s.IndexerState)
	}
	if s.IndexedFiles != 0 {
		t.Errorf("IndexedFiles = %d, want 0", s.IndexedFiles)
	}
	if s.TotalNodes != 0 {
		t.Errorf("TotalNodes = %d, want 0", s.TotalNodes)
	}
}

func TestReport_WithData(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, ".plumb", "topo.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	fileID := insertTestFile(t, db, "x.go")
	insertTestNode(t, db, fileID, "x.go", Node{Kind: KindFunction, Name: "X", Language: "go"})

	s := Report(db, dir, nil)
	if s.IndexedFiles != 1 {
		t.Errorf("IndexedFiles = %d, want 1", s.IndexedFiles)
	}
	if s.TotalNodes != 1 {
		t.Errorf("TotalNodes = %d, want 1", s.TotalNodes)
	}
}

func TestIndexedLanguages(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, "langs.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	// Insert files with different languages.
	db.Exec(`INSERT INTO topology_files(path, language, mtime_ns, error_msg) VALUES ('a.go', 'go', 0, '')`)     //nolint:errcheck // fixture insert; a failure surfaces as the assertion below reading no rows
	db.Exec(`INSERT INTO topology_files(path, language, mtime_ns, error_msg) VALUES ('b.py', 'python', 0, '')`) //nolint:errcheck // fixture insert; a failure surfaces as the assertion below reading no rows
	db.Exec(`INSERT INTO topology_files(path, language, mtime_ns, error_msg) VALUES ('c.go', 'go', 0, '')`)     //nolint:errcheck // fixture insert; a failure surfaces as the assertion below reading no rows
	db.Exec(`INSERT INTO topology_files(path, language, mtime_ns, error_msg) VALUES ('err.go', '', 0, 'oops')`) //nolint:errcheck // fixture insert; a failure surfaces as the assertion below reading no rows

	langs := indexedLanguages(db)
	langSet := map[string]bool{}
	for _, l := range langs {
		langSet[l] = true
	}
	if !langSet["go"] {
		t.Error("expected 'go' in languages")
	}
	if !langSet["python"] {
		t.Error("expected 'python' in languages")
	}
	// Error files and files with empty language should not appear.
	if langSet[""] {
		t.Error("empty language should not be in languages")
	}
	// No duplicates.
	seen := map[string]int{}
	for _, l := range langs {
		seen[l]++
	}
	for lang, count := range seen {
		if count > 1 {
			t.Errorf("language %q appears %d times, want 1", lang, count)
		}
	}
}

func TestFormatStatus_ContainsKeyFields(t *testing.T) {
	s := Status{
		IndexerState: "idle",
		IndexedFiles: 42,
		TotalNodes:   100,
		TotalEdges:   50,
		DBSizeBytes:  1024,
		LastSync:     time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
		Languages:    []string{"go", "python"},
		LastError:    "",
	}
	out := FormatStatus(s, "/my/workspace")
	for _, want := range []string{
		"idle",
		"42",
		"100",
		"50",
		"1.0 KiB",
		"/my/workspace",
		"go",
		"python",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatStatus output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatStatus_ShowsLastError(t *testing.T) {
	s := Status{IndexerState: "error", LastError: "disk full"}
	out := FormatStatus(s, "/ws")
	if !strings.Contains(out, "disk full") {
		t.Errorf("FormatStatus should include LastError; got:\n%s", out)
	}
}

// Byte formatting itself is covered by internal/textfmt; what matters here is
// that FormatStatus actually routes the DB size through it.
func TestFormatStatus_DBSizeUsesBinaryUnits(t *testing.T) {
	out := FormatStatus(Status{DBSizeBytes: 2048}, "/ws")
	if !strings.Contains(out, "2.0 KiB") {
		t.Errorf("FormatStatus should render db size as 2.0 KiB; got:\n%s", out)
	}
}
