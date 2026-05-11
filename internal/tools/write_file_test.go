package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callWriteFile(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewWriteFile(WriteDeps{}).Execute(context.Background(), raw)
}

func TestWriteFile_Create(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.txt")
	out, err := callWriteFile(t, map[string]any{"path": path, "content": "hello world\n"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "created") {
		t.Errorf("expected 'created' in output, got: %q", out)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "hello world\n" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestWriteFile_Overwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("old"), 0o644)
	out, err := callWriteFile(t, map[string]any{"path": path, "content": "new content"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "updated") {
		t.Errorf("expected 'updated' in output, got: %q", out)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new content" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestWriteFile_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "c.txt")
	_, err := callWriteFile(t, map[string]any{"path": path, "content": "deep"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "deep" {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestWriteFile_PreservesPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exec.sh")
	_ = os.WriteFile(path, []byte("old"), 0o755)
	_, err := callWriteFile(t, map[string]any{"path": path, "content": "#!/bin/sh\necho hi\n"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o755 {
		t.Errorf("expected 0755, got %o", info.Mode().Perm())
	}
}

func TestWriteFile_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	_, err := callWriteFile(t, map[string]any{"path": dir, "content": "oops"})
	if err == nil {
		t.Fatal("expected error for directory path")
	}
}

func TestWriteFile_MissingPath(t *testing.T) {
	_, err := callWriteFile(t, map[string]any{"content": "x"})
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestWriteFile_PreservesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not supported on this filesystem: %v", err)
	}
	if _, err := callWriteFile(t, map[string]any{"path": link, "content": "new"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The link must still be a symlink, not a regular file.
	linfo, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if linfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	// The target must contain the new content.
	got, _ := os.ReadFile(target)
	if string(got) != "new" {
		t.Errorf("target content = %q, want %q", got, "new")
	}
}

func TestWriteFile_AtomicTmpCleanedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	_, err := callWriteFile(t, map[string]any{"path": path, "content": "data"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No .plumb.tmp sibling should remain next to the target.
	if _, err := os.Stat(path + ".plumb.tmp"); !os.IsNotExist(err) {
		t.Error("sibling tmp file should not exist after successful write")
	}
	// The primary temp file is in os.TempDir() and has already been renamed away.
	// We can't check it directly, but we can confirm the target file is correct.
	data, _ := os.ReadFile(path)
	if string(data) != "data" {
		t.Errorf("unexpected content: %q", data)
	}
}
