package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTracker_RecordWrote(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x.go")
	if err := os.WriteFile(f, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := NewWriteTracker()
	if w.Wrote(f) {
		t.Fatal("a fresh tracker should report nothing written")
	}
	w.Record(f)
	if !w.Wrote(f) {
		t.Fatal("Wrote should be true after Record")
	}
	// The file:// spelling canonicalises to the same key as the bare path.
	if !w.Wrote("file://" + f) {
		t.Fatal("a file:// spelling should match the recorded path")
	}
}

func TestWriteTracker_NilSafe(t *testing.T) {
	var w *WriteTracker
	w.Record("/tmp/does-not-matter") // must not panic
	if w.Wrote("/tmp/does-not-matter") {
		t.Fatal("a nil tracker should report nothing written")
	}
}

// TestDirtyBlocksWrite_SessionAware is the core of the session-aware guard: a
// dirty file blocks a destructive write unless plumb wrote it this session.
func TestDirtyBlocksWrite_SessionAware(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "f.txt")
	gitExec(t, dir, "commit", "-m", "add f")
	// Dirty it as if an external edit (plumb did not write it).
	if err := os.WriteFile(f, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	w := NewWriteTracker()
	if !dirtyBlocksWrite(ctx, w, f) {
		t.Fatal("a dirty file plumb did not write should block")
	}
	w.Record(f)
	if dirtyBlocksWrite(ctx, w, f) {
		t.Fatal("a dirty file plumb wrote this session should not block")
	}
	// A nil tracker falls back to the strict dirty check.
	if !dirtyBlocksWrite(ctx, nil, f) {
		t.Fatal("a nil tracker should still block a dirty file")
	}
}

// TestDirtyBlocksMove_SessionAware covers the move/copy variant: untracked
// sources never block, a tracked-modified source blocks unless plumb wrote it.
func TestDirtyBlocksMove_SessionAware(t *testing.T) {
	dir := initGitRepo(t)
	ctx := context.Background()

	// Untracked source: a move/copy preserves content, so it never blocks.
	u := filepath.Join(dir, "u.txt")
	if err := os.WriteFile(u, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirtyBlocksMove(ctx, NewWriteTracker(), u) {
		t.Fatal("an untracked source should not block a move")
	}

	// Tracked + modified source: blocks unless plumb wrote it this session.
	f := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "f.txt")
	gitExec(t, dir, "commit", "-m", "add f")
	if err := os.WriteFile(f, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewWriteTracker()
	if !dirtyBlocksMove(ctx, w, f) {
		t.Fatal("a tracked, modified source should block a move")
	}
	w.Record(f)
	if dirtyBlocksMove(ctx, w, f) {
		t.Fatal("a source plumb wrote this session should not block a move")
	}
}

// TestEditFile_SessionAwareDirtyGuard proves the wiring end-to-end: editing a
// clean committed file then re-editing it (now dirty, but written by plumb this
// session) is allowed without dirty_ok, because edit_file records the write.
func TestEditFile_SessionAwareDirtyGuard(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "foo.txt")
	if err := os.WriteFile(f, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "foo.txt")
	gitExec(t, dir, "commit", "-m", "add foo")

	tool := NewEditFile(WriteDeps{Writes: NewWriteTracker()})

	if _, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": f,
		"edits":     []map[string]string{{"old_string": "hello", "new_string": "hi"}},
	})); err != nil {
		t.Fatalf("first edit on a clean file should be allowed: %v", err)
	}
	// The file is now dirty, but plumb wrote it this session.
	if _, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": f,
		"edits":     []map[string]string{{"old_string": "world", "new_string": "there"}},
	})); err != nil {
		t.Fatalf("second edit on a plumb-dirtied file should be allowed: %v", err)
	}
}

// TestEditFile_DirtyGuardBlocksPreExistingDirt confirms the protection survives:
// a file dirtied outside plumb is still blocked without dirty_ok.
func TestEditFile_DirtyGuardBlocksPreExistingDirt(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "bar.txt")
	if err := os.WriteFile(f, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "bar.txt")
	gitExec(t, dir, "commit", "-m", "add bar")
	// Dirty the file outside plumb — the tracker never sees this write.
	if err := os.WriteFile(f, []byte("one two\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := NewEditFile(WriteDeps{Writes: NewWriteTracker()})
	_, err := tool.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": f,
		"edits":     []map[string]string{{"old_string": "one", "new_string": "X"}},
	}))
	if err == nil {
		t.Fatal("editing a pre-existing dirty file should be blocked without dirty_ok")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") {
		t.Errorf("want an uncommitted-changes error, got: %v", err)
	}
}
