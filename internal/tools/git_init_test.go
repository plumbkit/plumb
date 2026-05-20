package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callGitInit(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewGitInit(WriteDeps{}).Execute(context.Background(), raw)
}

func TestGitInit_MissingPath(t *testing.T) {
	requireGit(t)
	_, err := callGitInit(t, map[string]any{})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGitInit_CreatesRepo(t *testing.T) {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "newrepo")
	out, err := callGitInit(t, map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "initialised git repository") {
		t.Errorf("expected confirmation in output, got: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf(".git directory not created: %v", err)
	}
}

func TestGitInit_CreatesDirectoryIfAbsent(t *testing.T) {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "deep", "nested", "repo")
	_, err := callGitInit(t, map[string]any{"path": dir})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf(".git directory not created: %v", err)
	}
}

func TestGitInit_InitPlumb(t *testing.T) {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "plumbrepo")
	out, err := callGitInit(t, map[string]any{"path": dir, "init_plumb": true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, ".plumb/") {
		t.Errorf("expected .plumb mention in output, got: %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, ".plumb")); err != nil {
		t.Errorf(".plumb/ not created: %v", err)
	}
	contextMd := filepath.Join(dir, ".plumb", "context.md")
	if _, err := os.Stat(contextMd); err != nil {
		t.Errorf("context.md not created: %v", err)
	}
	data, _ := os.ReadFile(contextMd)
	if !strings.Contains(string(data), "Project Context") {
		t.Errorf("context.md has unexpected content: %q", string(data))
	}
}

func TestGitInit_InitPlumbDoesNotOverwriteExistingContext(t *testing.T) {
	requireGit(t)
	dir := filepath.Join(t.TempDir(), "existing")
	plumbDir := filepath.Join(dir, ".plumb")
	_ = os.MkdirAll(plumbDir, 0o755)
	contextMd := filepath.Join(plumbDir, "context.md")
	_ = os.WriteFile(contextMd, []byte("existing content"), 0o644)
	_, err := callGitInit(t, map[string]any{"path": dir, "init_plumb": true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(contextMd)
	if string(data) != "existing content" {
		t.Errorf("expected existing content preserved, got: %q", string(data))
	}
}
