package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// fsync_seams_test.go proves the write tools honour the fsync-before-ack
// contract: every acknowledged write fsyncs the staged temp file before the
// rename and the parent directory after it — and that disabling the [edits]
// fsync knob skips both.

// syncSeamRecorder stubs syncFileHook / syncDirHook and records the calls.
// Not safe for parallel tests (it swaps package-level vars).
type syncSeamRecorder struct {
	mu        sync.Mutex
	fileSyncs int
	dirSyncs  []string
}

func stubSyncSeams(t *testing.T) *syncSeamRecorder {
	t.Helper()
	r := &syncSeamRecorder{}
	origFile, origDir := syncFileHook, syncDirHook
	syncFileHook = func(f *os.File) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.fileSyncs++
		return nil
	}
	syncDirHook = func(dir string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.dirSyncs = append(r.dirSyncs, dir)
		return nil
	}
	t.Cleanup(func() { syncFileHook, syncDirHook = origFile, origDir })
	return r
}

func (r *syncSeamRecorder) fileSyncCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.fileSyncs
}

func (r *syncSeamRecorder) dirSynced(dir string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.dirSyncs {
		if d == dir {
			return true
		}
	}
	return false
}

func (r *syncSeamRecorder) dirSyncCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dirSyncs)
}

func TestWriteFile_InvokesSyncSeams(t *testing.T) {
	r := stubSyncSeams(t)
	path := filepath.Join(t.TempDir(), "f.txt")
	if _, err := callWriteFile(t, map[string]any{"file_path": path, "content": "x"}); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if r.fileSyncCount() == 0 {
		t.Error("write_file never fsynced the staged temp file")
	}
	if !r.dirSynced(filepath.Dir(path)) {
		t.Errorf("write_file never fsynced the parent dir %s (got %v)", filepath.Dir(path), r.dirSyncs)
	}
}

func TestEditFile_InvokesSyncSeams(t *testing.T) {
	r := stubSyncSeams(t)
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"old_string": "old", "new_string": "new"}},
	}); err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if r.fileSyncCount() == 0 {
		t.Error("edit_file never fsynced the staged temp file")
	}
	if !r.dirSynced(filepath.Dir(path)) {
		t.Errorf("edit_file never fsynced the parent dir %s (got %v)", filepath.Dir(path), r.dirSyncs)
	}
}

func TestRenameFile_InvokesSyncSeams(t *testing.T) {
	r := stubSyncSeams(t)
	dir := t.TempDir()
	from := filepath.Join(dir, "a.txt")
	to := filepath.Join(dir, "sub", "b.txt")
	if err := os.WriteFile(from, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"from": from, "to": to})
	if _, err := NewRenameFile(WriteDeps{}).Execute(context.Background(), raw); err != nil {
		t.Fatalf("rename_file: %v", err)
	}
	// The move crosses directories, so BOTH parents must be synced.
	if !r.dirSynced(dir) {
		t.Errorf("rename_file never fsynced the source parent dir %s (got %v)", dir, r.dirSyncs)
	}
	if !r.dirSynced(filepath.Dir(to)) {
		t.Errorf("rename_file never fsynced the destination parent dir %s (got %v)", filepath.Dir(to), r.dirSyncs)
	}
}

func TestDeleteFile_InvokesSyncSeams(t *testing.T) {
	r := stubSyncSeams(t)
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"file_path": path})
	if _, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw); err != nil {
		t.Fatalf("delete_file: %v", err)
	}
	if !r.dirSynced(filepath.Dir(path)) {
		t.Errorf("delete_file never fsynced the parent dir %s (got %v)", filepath.Dir(path), r.dirSyncs)
	}
}

func TestTransactionApply_InvokesSyncSeams(t *testing.T) {
	r := stubSyncSeams(t)
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a.txt")
	p2 := filepath.Join(dir, "b.txt")
	for _, p := range []string{p1, p2} {
		if err := os.WriteFile(p, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := callTransaction(t, map[string]any{
		"operations": []map[string]any{
			{"file_path": p1, "edits": []map[string]any{{"old_string": "old", "new_string": "new"}}},
			{"file_path": p2, "edits": []map[string]any{{"old_string": "old", "new_string": "new"}}},
		},
	}); err != nil {
		t.Fatalf("transaction_apply: %v", err)
	}
	if r.fileSyncCount() < 2 {
		t.Errorf("transaction_apply fsynced %d temp files, want >= 2", r.fileSyncCount())
	}
	if !r.dirSynced(dir) {
		t.Errorf("transaction_apply never fsynced the parent dir %s (got %v)", dir, r.dirSyncs)
	}
}

// TestWriteTools_FsyncDisabled_SkipsSeams: with the [edits] fsync knob off,
// neither the temp-file sync nor the directory sync fires — the knob restores
// the pre-contract behaviour wholesale.
func TestWriteTools_FsyncDisabled_SkipsSeams(t *testing.T) {
	r := stubSyncSeams(t)
	SetFsyncFunc(func() bool { return false })
	t.Cleanup(func() { SetFsyncFunc(nil) })

	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if _, err := callWriteFile(t, map[string]any{"file_path": path, "content": "x"}); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if _, err := callEditFile(t, map[string]any{
		"file_path": path,
		"edits":     []map[string]any{{"old_string": "x", "new_string": "y"}},
	}); err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	renameTo := filepath.Join(dir, "g.txt")
	raw, _ := json.Marshal(map[string]any{"from": path, "to": renameTo})
	if _, err := NewRenameFile(WriteDeps{}).Execute(context.Background(), raw); err != nil {
		t.Fatalf("rename_file: %v", err)
	}
	raw, _ = json.Marshal(map[string]any{"file_path": renameTo})
	if _, err := NewDeleteFile(WriteDeps{}).Execute(context.Background(), raw); err != nil {
		t.Fatalf("delete_file: %v", err)
	}

	if got := r.fileSyncCount(); got != 0 {
		t.Errorf("fsync knob off: %d temp-file syncs fired, want 0", got)
	}
	if got := r.dirSyncCount(); got != 0 {
		t.Errorf("fsync knob off: %d directory syncs fired, want 0", got)
	}
}

// TestWriteTools_DirSyncFailureIsNonFatal: a directory-fsync error must not
// fail a write that already landed (FUSE and some network mounts refuse
// directory fsyncs with EINVAL).
func TestWriteTools_DirSyncFailureIsNonFatal(t *testing.T) {
	origFile, origDir := syncFileHook, syncDirHook
	syncFileHook = func(f *os.File) error { return nil }
	syncDirHook = func(dir string) error { return os.ErrInvalid }
	t.Cleanup(func() { syncFileHook, syncDirHook = origFile, origDir })

	path := filepath.Join(t.TempDir(), "f.txt")
	if _, err := callWriteFile(t, map[string]any{"file_path": path, "content": "x"}); err != nil {
		t.Fatalf("write_file should succeed despite dir-fsync failure: %v", err)
	}
	if data, _ := os.ReadFile(path); string(data) != "x" {
		t.Errorf("content not written: %q", data)
	}
}
