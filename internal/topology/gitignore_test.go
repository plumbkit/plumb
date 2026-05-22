package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureGitignore_CreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := ensureGitignore(dir); err != nil {
		t.Fatalf("ensureGitignore: %v", err)
	}
	path := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"topology.db", "topology.db-wal", "topology.db-shm"} {
		if !strings.Contains(string(data), want) {
			t.Errorf(".gitignore missing %q:\n%s", want, data)
		}
	}

	// A second call must be a no-op: no duplicate entries, identical content.
	if err := ensureGitignore(dir); err != nil {
		t.Fatalf("ensureGitignore (2nd): %v", err)
	}
	data2, _ := os.ReadFile(path)
	if string(data2) != string(data) {
		t.Errorf("second call changed file:\nbefore:\n%s\nafter:\n%s", data, data2)
	}
	if n := strings.Count(string(data2), "topology.db-wal"); n != 1 {
		t.Errorf("topology.db-wal appears %d times, want 1", n)
	}
}

func TestEnsureGitignore_PreservesExistingEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(path, []byte("*.log\ntopology.db\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ensureGitignore(dir); err != nil {
		t.Fatalf("ensureGitignore: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "*.log") {
		t.Errorf("existing unrelated entry lost:\n%s", data)
	}
	// topology.db was already present and must not be duplicated.
	bare := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "topology.db" {
			bare++
		}
	}
	if bare != 1 {
		t.Errorf("bare topology.db line appears %d times, want 1:\n%s", bare, data)
	}
	// The missing sidecar entries were still appended.
	for _, want := range []string{"topology.db-wal", "topology.db-shm"} {
		if !strings.Contains(string(data), want) {
			t.Errorf(".gitignore missing %q after merge:\n%s", want, data)
		}
	}
}

func TestOpenDB_WritesGitignore(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(DBPath(dir))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	data, err := os.ReadFile(filepath.Join(dir, ".plumb", ".gitignore"))
	if err != nil {
		t.Fatalf("expected .plumb/.gitignore after openDB: %v", err)
	}
	if !strings.Contains(string(data), "topology.db") {
		t.Errorf(".gitignore does not exclude topology.db:\n%s", data)
	}
}
