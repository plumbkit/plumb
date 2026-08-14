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

// dirSynced reports whether one of the recorded fsyncs hit the DIRECTORY dir
// names, comparing by identity (os.SameFile) rather than by spelling.
//
// The guarantee under test is "the entry's parent directory reached the disk",
// which is a statement about an inode, not about a string. A write path is free
// to name that directory however it likes — it resolves symlinks on the way to
// a canonical lock key, and the kernel resolves them again on open — so two
// spellings of one directory are the same fsync. Comparing strings turns that
// freedom into a failure: on macOS t.TempDir() hands back /var/... while any
// resolved spelling is /private/var/..., the same inode reached two ways.
//
// Identity is also the STRICTER assertion for the property that matters: a
// write path that fsynced a genuinely different directory — the grandparent,
// the temp file's staging dir, a stale path — fails here, where a string
// comparison against a resolved spelling could be satisfied by any path that
// happened to resolve alike.
func (r *syncSeamRecorder) dirSynced(dir string) bool {
	want, err := os.Stat(dir)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.dirSyncs {
		if got, err := os.Stat(d); err == nil && os.SameFile(want, got) {
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

// TestWriteFile_NewDirectoryTree_SyncsCreatedDirs closes the one-level-up hole:
// fsyncing the file and its immediate parent is not enough when that parent was
// itself just created — the new directory's entry sits in its own parent, so a
// crash could lose the whole subtree despite the acknowledgement.
func TestWriteFile_NewDirectoryTree_SyncsCreatedDirs(t *testing.T) {
	r := stubSyncSeams(t)
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(a, "b")
	path := filepath.Join(b, "f.txt")
	if _, err := callWriteFile(t, map[string]any{"file_path": path, "content": "x"}); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	// root holds the new entry "a"; a holds the new entry "b"; b holds the file.
	for _, dir := range []string{root, a, b} {
		if !r.dirSynced(dir) {
			t.Errorf("write_file into a fresh tree never fsynced %s (got %v)", dir, r.dirSyncs)
		}
	}
}

// An existing parent must not cost any extra directory syncs beyond the single
// post-rename one — the fresh-tree walk is for new directories only.
func TestWriteFile_ExistingDirectory_SyncsOnlyOnce(t *testing.T) {
	r := stubSyncSeams(t)
	dir := t.TempDir()
	if _, err := callWriteFile(t, map[string]any{"file_path": filepath.Join(dir, "f.txt"), "content": "x"}); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if got := r.dirSyncCount(); got != 1 {
		t.Errorf("write_file into an existing dir fired %d directory syncs, want 1: %v", got, r.dirSyncs)
	}
}

// TestWriteFile_SymlinkedParent_SyncsTheDirectoryTheCallerNamed pins the answer
// to a question the fsync seams cannot answer on their own: when the write path
// canonicalises a path through symlinks (safeWrite routes through
// paths.Canonical so the write lands where the write LOCK was taken), does it
// still fsync the directory the caller was talking about?
//
// It must, and "must" here is about an inode, not a spelling. Writing
// link/f.txt creates the entry in real/, so real/ is the directory whose
// contents changed and the only one whose fsync makes the entry durable —
// and opening the link spelling for fsync reaches that same inode anyway.
//
// This test is the counterpart to the string comparison dirSynced used to make.
// That comparison failed on macOS the moment safeWrite began resolving
// symlinks, which left two readings open: a stale assertion, or a write path
// that had quietly started fsyncing somewhere else. The dirSynced assertions
// below fail under the second reading and pass under the first, so the decision
// is pinned rather than implied by a test that was only ever comparing strings.
// Both were confirmed to fail against a write path mutated to fsync the
// grandparent, and against one with the directory fsync removed.
func TestWriteFile_SymlinkedParent_SyncsTheDirectoryTheCallerNamed(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	r := stubSyncSeams(t)
	if _, err := callWriteFile(t, map[string]any{
		"file_path": filepath.Join(link, "f.txt"), "content": "x",
	}); err != nil {
		t.Fatalf("write_file through a symlinked parent: %v", err)
	}

	// Canonicalising the path did not RETARGET the write: the entry is in real/,
	// which is where the caller's own spelling resolves to, and the link is still
	// a link. The kernel would land it there too — that is the point, since it is
	// what makes the resolved spelling the same place rather than a second one.
	// (The distinct guarantee that a symlinked FILE is written through rather
	// than replaced is TestWriteFile_PreservesSymlink's.)
	if _, err := os.Lstat(filepath.Join(realDir, "f.txt")); err != nil {
		t.Errorf("the write did not land in the real directory: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the write replaced the parent symlink instead of writing under it (err=%v)", err)
	}

	// Both spellings name one directory, so a correct write path fsyncs a
	// directory that IS that one, whichever spelling it recorded.
	for _, spelling := range []string{link, realDir} {
		if !r.dirSynced(spelling) {
			t.Errorf("write_file never fsynced the directory named by %s (got %v)", spelling, r.dirSyncs)
		}
	}

	// And it must not have settled for an ANCESTOR: root holds the link, not
	// the new entry, so fsyncing it would leave f.txt undurable.
	if r.dirSynced(root) {
		t.Errorf("write_file fsynced the grandparent %s instead of the entry's own directory (got %v)", root, r.dirSyncs)
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
