package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callGitAdd(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewGitAdd(WriteDeps{}).Execute(context.Background(), raw)
}

func TestGitAdd_MissingFiles(t *testing.T) {
	requireGit(t)
	_, err := callGitAdd(t, map[string]any{"repo": "."})
	if err == nil {
		t.Fatal("expected error for missing files")
	}
	if !strings.Contains(err.Error(), "at least one file path is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGitAdd_EmptyFiles(t *testing.T) {
	requireGit(t)
	_, err := callGitAdd(t, map[string]any{"files": []string{}})
	if err == nil {
		t.Fatal("expected error for empty files slice")
	}
	if !strings.Contains(err.Error(), "at least one file path is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGitAdd_StagesNewFile(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	newFile := filepath.Join(dir, "added.txt")
	_ = os.WriteFile(newFile, []byte("new\n"), 0o644)
	out, err := callGitAdd(t, map[string]any{
		"files": []string{newFile},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "staged 1 file") {
		t.Errorf("expected staged count in output, got: %q", out)
	}
	if !strings.Contains(out, "added.txt") {
		t.Errorf("expected filename in output, got: %q", out)
	}
}

func TestGitAdd_StagesModifiedFile(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	_ = os.WriteFile(filepath.Join(dir, "init.txt"), []byte("modified\n"), 0o644)
	out, err := callGitAdd(t, map[string]any{
		"files": []string{filepath.Join(dir, "init.txt")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "staged 1 file") {
		t.Errorf("expected staged count in output, got: %q", out)
	}
}

func TestGitAdd_MultipleFiles(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	for _, name := range []string{"a.txt", "b.txt"} {
		_ = os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644)
	}
	out, err := callGitAdd(t, map[string]any{
		"files": []string{
			filepath.Join(dir, "a.txt"),
			filepath.Join(dir, "b.txt"),
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "staged 2 file") {
		t.Errorf("expected 2 files staged, got: %q", out)
	}
}

func TestGitAdd_DerivesRepoFromFile(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)
	newFile := filepath.Join(dir, "derived.txt")
	_ = os.WriteFile(newFile, []byte("test\n"), 0o644)
	// No repo arg — should derive from the file path.
	out, err := callGitAdd(t, map[string]any{
		"files": []string{newFile},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "staged") {
		t.Errorf("expected staged output, got: %q", out)
	}
}
