package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureGitignoreEntries_CreatesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	header := "# demo (do not commit)"
	entries := []string{"demo.db", "demo.db-wal"}
	if err := EnsureGitignoreEntries(dir, header, entries); err != nil {
		t.Fatalf("EnsureGitignoreEntries: %v", err)
	}
	path := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range append([]string{header}, entries...) {
		if !strings.Contains(string(data), want) {
			t.Errorf(".gitignore missing %q:\n%s", want, data)
		}
	}

	// A second call must be a no-op: identical content, no duplicates.
	if err := EnsureGitignoreEntries(dir, header, entries); err != nil {
		t.Fatalf("EnsureGitignoreEntries (2nd): %v", err)
	}
	data2, _ := os.ReadFile(path)
	if string(data2) != string(data) {
		t.Errorf("second call changed file:\nbefore:\n%s\nafter:\n%s", data, data2)
	}
}

func TestEnsureGitignoreEntries_MergesIntoExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	// No trailing newline on purpose: the append must insert one first.
	if err := os.WriteFile(path, []byte("*.log\ndemo.db"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnsureGitignoreEntries(dir, "# hdr", []string{"demo.db", "demo.db-wal"}); err != nil {
		t.Fatalf("EnsureGitignoreEntries: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "*.log\n") {
		t.Errorf("existing unrelated entry lost or newline not inserted:\n%s", data)
	}
	bare := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "demo.db" {
			bare++
		}
	}
	if bare != 1 {
		t.Errorf("demo.db appears %d times, want 1 (already present, must not duplicate):\n%s", bare, data)
	}
	if !strings.Contains(string(data), "demo.db-wal") {
		t.Errorf("missing entry not appended:\n%s", data)
	}
}

// TestEnsureGitignoreEntries_SubstringLineDoesNotSuppress pins the property
// that distinguishes this helper from the hand-rolled copies it replaced:
// entries match by exact trimmed line, so a line that merely CONTAINS an
// entry — a commented-out copy, a negation, a longer path — must not count
// as the entry being present.
func TestEnsureGitignoreEntries_SubstringLineDoesNotSuppress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	seed := "#demo.db\n!demo.db\nsub/demo.db\ndemo.db.bak\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := EnsureGitignoreEntries(dir, "", []string{"demo.db"}); err != nil {
		t.Fatalf("EnsureGitignoreEntries: %v", err)
	}
	data, _ := os.ReadFile(path)
	bare := 0
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == "demo.db" {
			bare++
		}
	}
	if bare != 1 {
		t.Errorf("demo.db must be appended as its own line exactly once, got %d:\n%s", bare, data)
	}
	if !strings.HasPrefix(string(data), seed) {
		t.Errorf("pre-existing lines must be preserved verbatim:\n%s", data)
	}
}

func TestEnsureGitignoreEntries_EmptyHeaderAppendsNoBanner(t *testing.T) {
	dir := t.TempDir()
	if err := EnsureGitignoreEntries(dir, "", []string{"demo.db"}); err != nil {
		t.Fatalf("EnsureGitignoreEntries: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if strings.Contains(string(data), "#") {
		t.Errorf("no banner expected with an empty header:\n%s", data)
	}
	if got := string(data); got != "demo.db\n" {
		t.Errorf("want exactly %q, got %q", "demo.db\n", got)
	}
}
