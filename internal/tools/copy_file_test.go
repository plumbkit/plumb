package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func execCopyFile(t *testing.T, raw json.RawMessage) (string, error) {
	t.Helper()
	return NewCopyFile(WriteDeps{}).Execute(context.Background(), raw)
}

func TestCopyFile_BasicCopy(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	content := []byte("hello copy")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := execCopyFile(t, jsonArgs(map[string]any{"from": src, "to": dst, "dirty_ok": true}))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out == "" {
		t.Error("expected non-empty result")
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("reading dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q want %q", got, content)
	}
}

func TestCopyFile_PreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "exec.sh")
	dst := filepath.Join(dir, "exec_copy.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := execCopyFile(t, jsonArgs(map[string]any{"from": src, "to": dst, "dirty_ok": true})); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("permissions: got %o want 755", info.Mode().Perm())
	}
}

func TestCopyFile_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "nested", "deep", "dst.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := execCopyFile(t, jsonArgs(map[string]any{"from": src, "to": dst, "dirty_ok": true})); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dst not created: %v", err)
	}
}

func TestCopyFile_RefusesExistingWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("src"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := execCopyFile(t, jsonArgs(map[string]any{"from": src, "to": dst, "dirty_ok": true}))
	if err == nil {
		t.Error("expected error when destination exists without overwrite=true")
	}
}

func TestCopyFile_OverwriteExisting(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("new content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := execCopyFile(t, jsonArgs(map[string]any{
		"from": src, "to": dst, "overwrite": true, "dirty_ok": true,
	})); err != nil {
		t.Fatalf("Execute with overwrite: %v", err)
	}

	got, _ := os.ReadFile(dst)
	if string(got) != "new content" {
		t.Errorf("overwrite did not apply: got %q", got)
	}
}

func TestCopyFile_RefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.txt")
	_, err := execCopyFile(t, jsonArgs(map[string]any{"from": dir, "to": dst, "dirty_ok": true}))
	if err == nil {
		t.Error("expected error when source is a directory")
	}
}

func TestCopyFile_SamePathError(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	_, err := execCopyFile(t, jsonArgs(map[string]any{"from": f, "to": f}))
	if err == nil {
		t.Error("expected error when from == to")
	}
}

func TestCopyFile_MissingFromError(t *testing.T) {
	_, err := execCopyFile(t, json.RawMessage(`{"to": "/tmp/dst.txt"}`))
	if err == nil {
		t.Error("expected error when 'from' is missing")
	}
}

// jsonArgs marshals a map to json.RawMessage for test helpers.
func jsonArgs(m map[string]any) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}
