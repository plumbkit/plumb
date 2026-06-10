package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func callWriteFile(t *testing.T, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return NewWriteFile(WriteDeps{}).Execute(context.Background(), raw)
}

func TestWriteFile_Create(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hello.txt")
	out, err := callWriteFile(t, map[string]any{"file_path": path, "content": "hello world\n"})
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
	out, err := callWriteFile(t, map[string]any{"file_path": path, "content": "new content"})
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
	_, err := callWriteFile(t, map[string]any{"file_path": path, "content": "deep"})
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
	_, err := callWriteFile(t, map[string]any{"file_path": path, "content": "#!/bin/sh\necho hi\n"})
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
	_, err := callWriteFile(t, map[string]any{"file_path": dir, "content": "oops"})
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
	if _, err := callWriteFile(t, map[string]any{"file_path": link, "content": "new"}); err != nil {
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

func TestWriteFile_DirtyCheck_RefusesDirtyFile(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "file.txt")
	gitExec(t, dir, "commit", "-m", "add")
	// Modify after commit to make dirty.
	if err := os.WriteFile(f, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := callWriteFile(t, map[string]any{"file_path": f, "content": "new"})
	if err == nil {
		t.Fatal("expected error for dirty file")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("unexpected error message: %v", err)
	}
	// File must be unchanged.
	got, _ := os.ReadFile(f)
	if string(got) != "modified" {
		t.Errorf("file content changed unexpectedly: %q", got)
	}
}

func TestWriteFile_DirtyOk_OverwritesDirtyFile(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "file.txt")
	gitExec(t, dir, "commit", "-m", "add")
	if err := os.WriteFile(f, []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := callWriteFile(t, map[string]any{"file_path": f, "content": "new content", "dirty_ok": true})
	if err != nil {
		t.Fatalf("unexpected error with dirty_ok=true: %v", err)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "new content" {
		t.Errorf("unexpected content: %q", got)
	}
}

func TestWriteFile_DirtyCheck_AllowsNewFile(t *testing.T) {
	dir := initGitRepo(t)
	// A file that doesn't exist yet is not dirty — creation must succeed.
	f := filepath.Join(dir, "brand-new.txt")
	_, err := callWriteFile(t, map[string]any{"file_path": f, "content": "hello"})
	if err != nil {
		t.Fatalf("unexpected error creating new file: %v", err)
	}
}

func TestWriteFile_ShowWriteDiff_IncludesDiff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("old content\n"), 0o644)

	out, err := NewWriteFile(WriteDeps{ShowWriteDiff: true}).Execute(
		context.Background(),
		mustJSON(map[string]any{"file_path": path, "content": "new content\n"}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "-old content") {
		t.Errorf("expected deletion line in diff; got:\n%s", out)
	}
	if !strings.Contains(out, "+new content") {
		t.Errorf("expected addition line in diff; got:\n%s", out)
	}
}

func TestWriteFile_ShowWriteDiff_NewFileEmitsNewFileMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brand-new.txt")

	out, err := NewWriteFile(WriteDeps{ShowWriteDiff: true}).Execute(
		context.Background(),
		mustJSON(map[string]any{"file_path": path, "content": "hello\n"}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "new file") {
		t.Errorf("expected 'new file' marker for a new file; got:\n%s", out)
	}
}

func TestWriteFile_AtomicTmpCleanedOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	_, err := callWriteFile(t, map[string]any{"file_path": path, "content": "data"})
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

func TestWriteFile_ExpectedMtimeGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	good := info.ModTime().Format(time.RFC3339Nano)

	// Matching expected_mtime → the write proceeds and bumps the mtime.
	if _, err := callWriteFile(t, map[string]any{
		"file_path": path, "content": "v2", "expected_mtime": good,
	}); err != nil {
		t.Fatalf("matching expected_mtime should write: %v", err)
	}

	// The original mtime is now stale (the write above superseded it) → refused.
	_, err := callWriteFile(t, map[string]any{
		"file_path": path, "content": "v3", "expected_mtime": good,
	})
	if err == nil || !strings.Contains(err.Error(), "modified since you read it") {
		t.Fatalf("stale expected_mtime should be refused, got: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "v2" {
		t.Errorf("a refused write must not change the file, got: %q", data)
	}
}

func TestWriteFile_ExpectedShaGuard(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	sha, _ := fileSHA256(path)

	// Matching expected_sha → the write proceeds.
	if _, err := callWriteFile(t, map[string]any{
		"file_path": path, "content": "v2", "expected_sha": sha,
	}); err != nil {
		t.Fatalf("matching expected_sha should write: %v", err)
	}
	// The old sha no longer matches the new content → refused.
	_, err := callWriteFile(t, map[string]any{
		"file_path": path, "content": "v3", "expected_sha": sha,
	})
	if err == nil || !strings.Contains(err.Error(), "content has changed since you read it") {
		t.Fatalf("stale expected_sha should be refused, got: %v", err)
	}
}

func TestWriteFile_RefusesStaleOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	reads := NewReadTracker()
	info, _ := os.Stat(path)
	reads.Record(path, info.ModTime()) // this session read v1
	tool := NewWriteFile(WriteDeps{Reads: reads})

	// A peer edits the file after our read (advance the mtime past the read).
	future := time.Now().Add(2 * time.Second)
	_ = os.WriteFile(path, []byte("peer change"), 0o644)
	_ = os.Chtimes(path, future, future)

	raw, _ := json.Marshal(map[string]any{"file_path": path, "content": "my overwrite"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil || !strings.Contains(err.Error(), "changed on disk since you read it") {
		t.Fatalf("a stale overwrite should be refused, got: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "peer change" {
		t.Errorf("refused write must not clobber the peer change, got: %q", data)
	}

	// overwrite_changed:true proceeds.
	raw2, _ := json.Marshal(map[string]any{"file_path": path, "content": "my overwrite", "overwrite_changed": true})
	if _, err := tool.Execute(context.Background(), raw2); err != nil {
		t.Fatalf("overwrite_changed:true should write: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "my overwrite" {
		t.Errorf("override write should apply, got: %q", data)
	}
}

func TestWriteFile_NewFileNeverFlagged(t *testing.T) {
	// A brand-new file the session never read must not trip the session-aware
	// guard, even with a ReadTracker wired.
	path := filepath.Join(t.TempDir(), "brand-new.txt")
	tool := NewWriteFile(WriteDeps{Reads: NewReadTracker()})
	raw, _ := json.Marshal(map[string]any{"file_path": path, "content": "hello"})
	if _, err := tool.Execute(context.Background(), raw); err != nil {
		t.Fatalf("creating an unread new file should never be flagged: %v", err)
	}
}
