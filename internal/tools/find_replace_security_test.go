package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindReplace_SkipsSymlinkEscapingWorkspace verifies find_replace will not
// read or write THROUGH an in-tree symlink whose target lies outside the
// workspace boundary, while still modifying genuine in-tree files. Regression
// test for the symlink-boundary-escape finding (toolsfs-1).
func TestFindReplace_SkipsSymlinkEscapingWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file OUTSIDE the workspace, plus an in-tree symlink pointing at it.
	outside := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(outside, []byte("TODO secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(ws, "link.txt")); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "real.go"), []byte("TODO real"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A real, symlink-resolving boundary guard (mirrors the production policy).
	pol := NewPathPolicy(ws, []AllowedRoot{{Path: ws, Access: AccessReadWrite, Label: "workspace"}})
	guard := BoundaryGuard(func(path string) error { _, err := pol.Check(path, AccessReadWrite); return err })
	tool := NewFindReplace(WriteDeps{Boundary: guard})

	args, _ := json.Marshal(map[string]any{
		"path": ws, "pattern": "TODO", "replacement": "DONE", "dry_run": false,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// The out-of-workspace target must be untouched (no write through the symlink).
	if data, _ := os.ReadFile(outside); string(data) != "TODO secret" {
		t.Errorf("find_replace wrote through an in-tree symlink to an out-of-workspace file: got %q", data)
	}
	// The genuine in-tree file must still be modified.
	if data, _ := os.ReadFile(filepath.Join(ws, "real.go")); !strings.Contains(string(data), "DONE") {
		t.Errorf("in-tree file was not modified: got %q", data)
	}
}

// TestFindReplace_WarnsOnMaxFilesTruncation verifies that when more matching
// files exist than max_files, the response says so rather than silently
// reporting a partial result (toolsfs-3).
func TestFindReplace_WarnsOnMaxFilesTruncation(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d.txt", i)), []byte("TODO"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool := NewFindReplace()
	args, _ := json.Marshal(map[string]any{
		"path": dir, "pattern": "TODO", "replacement": "DONE", "max_files": 1, "dry_run": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "reached max_files=1") {
		t.Errorf("expected a max_files truncation warning, got:\n%s", out)
	}
}
