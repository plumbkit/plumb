package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func callEditFile(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewEditFile(nil, nil).Execute(context.Background(), raw)
}

func TestEditFile_BasicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	_ = os.WriteFile(path, []byte("package main\n\nfunc Hello() {}\n"), 0o644)

	out, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "func Hello() {}", "new_str": "func Hello() { return }"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "applied 1 edit") {
		t.Errorf("unexpected output: %q", out)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "func Hello() { return }") {
		t.Errorf("edit not applied: %q", data)
	}
}

func TestEditFile_MultipleEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("foo\nbar\nbaz\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "foo", "new_str": "FOO"},
			{"old_str": "baz", "new_str": "BAZ"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "FOO\nbar\nBAZ\n" {
		t.Errorf("unexpected content after multi-edit: %q", data)
	}
}

func TestEditFile_RejectsAbsentString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("hello world\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "goodbye world", "new_str": "hi"},
		},
	})
	if err == nil {
		t.Fatal("expected error for absent old_str")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found' in error, got: %v", err)
	}
}

func TestEditFile_RejectsAmbiguousString(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("foo\nfoo\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "foo", "new_str": "bar"},
		},
	})
	if err == nil {
		t.Fatal("expected error for ambiguous old_str")
	}
	if !strings.Contains(err.Error(), "appears 2 times") {
		t.Errorf("expected count in error, got: %v", err)
	}
}

func TestEditFile_RejectsEmptyOldStr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("content\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "", "new_str": "something"},
		},
	})
	if err == nil {
		t.Fatal("expected error for empty old_str")
	}
}

func TestEditFile_RejectsMissingFile(t *testing.T) {
	_, err := callEditFile(t, map[string]any{
		"path": "/nonexistent/file.txt",
		"edits": []map[string]string{
			{"old_str": "x", "new_str": "y"},
		},
	})
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestEditFile_AtomicOnSuccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("original content\n"), 0o644)

	_, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "original content", "new_str": "new content"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No .plumb.tmp sibling should remain next to the target.
	if _, err := os.Stat(path + ".plumb.tmp"); !os.IsNotExist(err) {
		t.Error("sibling tmp file should not exist after successful write")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "new content") {
		t.Errorf("unexpected content: %q", data)
	}
}

func TestEditFile_FileUnchangedOnEditFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	original := "line1\nline2\nline3\n"
	_ = os.WriteFile(path, []byte(original), 0o644)

	// Second edit references something that doesn't exist after first edit.
	_, err := callEditFile(t, map[string]any{
		"path": path,
		"edits": []map[string]string{
			{"old_str": "line1", "new_str": "LINE1"},
			{"old_str": "NONEXISTENT", "new_str": "oops"},
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	// File must be unchanged — no partial write.
	data, _ := os.ReadFile(path)
	if string(data) != original {
		t.Errorf("file should be unchanged after failed multi-edit, got: %q", data)
	}
}
