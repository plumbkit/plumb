package tools

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// undoDeps returns a WriteDeps wired with the per-session trackers undo needs.
func undoDeps() WriteDeps {
	return WriteDeps{
		Reads:  NewReadTracker(),
		Writes: NewWriteTracker(),
		Undo:   NewUndoStore(),
	}
}

func TestUndoEdit_RestoresEditFileChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := undoDeps()

	if _, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "original", "new_string": "changed"}},
	})); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "changed\n" {
		t.Fatalf("edit not applied: %q", b)
	}

	out, err := NewUndoEdit(deps).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if err != nil {
		t.Fatalf("undo: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "original\n" {
		t.Fatalf("undo did not restore: %q", b)
	}
	if !strings.Contains(out, "restored") {
		t.Errorf("unexpected undo output: %s", out)
	}

	// Single-level: a second undo has nothing to revert.
	if _, err := NewUndoEdit(deps).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err == nil {
		t.Error("expected error on second undo")
	}
}

func TestUndoEdit_RemovesCreatedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.txt")
	deps := undoDeps()
	if _, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path, "content": "hello\n",
	})); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}

	if _, err := NewUndoEdit(deps).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected created file removed, stat err = %v", err)
	}
}

func TestUndoEdit_RestoresOverwrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := undoDeps()
	if _, err := NewWriteFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path, "content": "v2\n",
	})); err != nil {
		t.Fatalf("write: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "v2\n" {
		t.Fatalf("overwrite failed: %q", b)
	}
	if _, err := NewUndoEdit(deps).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "v1\n" {
		t.Fatalf("undo restore failed: %q", b)
	}
}

func TestUndoEdit_NothingToUndo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("x"), 0o644)
	_, err := NewUndoEdit(undoDeps()).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if err == nil || !strings.Contains(err.Error(), "nothing to undo") {
		t.Fatalf("want nothing-to-undo error, got %v", err)
	}
}

func TestUndoEdit_RefusesWhenChangedExternally(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("original\n"), 0o644)
	deps := undoDeps()
	if _, err := NewEditFile(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "original", "new_string": "changed"}},
	})); err != nil {
		t.Fatalf("edit: %v", err)
	}
	// A peer overwrites the file after plumb's write.
	_ = os.WriteFile(path, []byte("peer-edit\n"), 0o644)

	_, err := NewUndoEdit(deps).Execute(context.Background(), mustJSON(map[string]any{"file_path": path}))
	if err == nil || !strings.Contains(err.Error(), "refusing to undo") {
		t.Fatalf("want refusal, got %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "peer-edit\n" {
		t.Errorf("peer edit must be left intact, got %q", b)
	}

	// force overrides the guard and restores the pre-write content.
	if _, err := NewUndoEdit(deps).Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path, "force": true,
	})); err != nil {
		t.Fatalf("force undo: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "original\n" {
		t.Fatalf("force undo restore failed: %q", b)
	}
}

func TestUndoEdit_NilStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	_ = os.WriteFile(path, []byte("x"), 0o644)
	if _, err := NewUndoEdit(WriteDeps{}).Execute(context.Background(), mustJSON(map[string]any{"file_path": path})); err == nil {
		t.Fatal("expected nothing-to-undo with a nil undo store")
	}
}

func TestUndoStore_RecordTakePeekReset(t *testing.T) {
	u := NewUndoStore()
	u.Record("/a", undoSnapshot{before: "x", existedBefore: true, afterSHA: "h"})
	if _, ok := u.Peek("/a"); !ok {
		t.Fatal("peek miss after record")
	}
	s, ok := u.Take("/a")
	if !ok || s.before != "x" {
		t.Fatalf("take = %+v, %v", s, ok)
	}
	if _, ok := u.Take("/a"); ok {
		t.Fatal("entry should be gone after take")
	}
	u.Record("/b", undoSnapshot{})
	u.Reset()
	if _, ok := u.Peek("/b"); ok {
		t.Fatal("reset did not clear the store")
	}
}

func TestUndoStore_EvictsOldest(t *testing.T) {
	u := NewUndoStore()
	for i := 0; i < maxUndoEntries+5; i++ {
		u.Record("/p"+strconv.Itoa(i), undoSnapshot{})
	}
	u.mu.Lock()
	n := len(u.snaps)
	u.mu.Unlock()
	if n > maxUndoEntries {
		t.Fatalf("store grew to %d entries, cap is %d", n, maxUndoEntries)
	}
	if _, ok := u.Peek("/p0"); ok {
		t.Error("oldest entry should have been evicted")
	}
}

func TestUndoStore_NilSafe(t *testing.T) {
	var u *UndoStore
	u.Record("/a", undoSnapshot{}) // must not panic
	if _, ok := u.Peek("/a"); ok {
		t.Error("nil store should peek false")
	}
	if _, ok := u.Take("/a"); ok {
		t.Error("nil store should take false")
	}
	u.Reset()
}
