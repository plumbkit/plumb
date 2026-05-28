package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testBoundaryGuard(workspace string) BoundaryGuard {
	return func(path string) error {
		if PathWithinWorkspace(workspace, path) {
			return nil
		}
		return NewWorkspaceBoundaryError(workspace, path)
	}
}

func TestPathWithinWorkspaceRejectsSibling(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside.txt")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if PathWithinWorkspace(ws, outside) {
		t.Fatalf("outside sibling path was accepted")
	}
	if !PathWithinWorkspace(ws, filepath.Join(ws, "inside.txt")) {
		t.Fatalf("inside path was rejected")
	}
}

func TestPathWithinWorkspaceRejectsSymlinkEscapeForNewFile(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(ws, "link", "new.txt")
	if PathWithinWorkspace(ws, target) {
		t.Fatalf("symlink escape path %s was accepted", target)
	}
}

func TestReadFileBoundaryRejectsOutsideWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside.txt")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewReadFile(nil).WithBoundary(testBoundaryGuard(ws))
	_, err := tool.Execute(context.Background(), mustBoundaryJSON(t, map[string]string{"file_path": outside}))
	if err == nil {
		t.Fatal("expected boundary error")
	}
	var boundaryErr WorkspaceBoundaryError
	if !errors.As(err, &boundaryErr) {
		t.Fatalf("expected WorkspaceBoundaryError, got %v", err)
	}
}

func TestFindReplaceBoundaryRejectsOutsideWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "workspace")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outside, "file.txt")
	if err := os.WriteFile(path, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewFindReplace(WriteDeps{Boundary: testBoundaryGuard(ws)})
	_, err := tool.Execute(context.Background(), mustBoundaryJSON(t, map[string]any{
		"path":        outside,
		"pattern":     "before",
		"replacement": "after",
		"dry_run":     false,
	}))
	if err == nil || !strings.Contains(err.Error(), "workspace boundary violation") {
		t.Fatalf("expected boundary error, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "before" {
		t.Fatalf("outside file was modified: %q", got)
	}
}

func mustBoundaryJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
