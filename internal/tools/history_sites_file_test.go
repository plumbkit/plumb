package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
	"github.com/plumbkit/plumb/internal/textdiff"
)

func assertMatchesDisk(t *testing.T, c history.Change) {
	t.Helper()
	if c.After.Exists {
		disk, err := os.ReadFile(c.Path)
		if err != nil || !bytes.Equal(disk, c.After.Content) {
			t.Fatalf("After != disk for %s", c.Path)
		}
	}
	got, err := textdiff.Apply(string(c.Before.Content), textdiff.Unified(string(c.Before.Content), string(c.After.Content)))
	if err != nil || got != string(c.After.Content) {
		t.Fatalf("diff of recorded sides does not reapply: %v", err)
	}
}

func TestWriteFileRecordsCreateThenUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	wf := NewWriteFile(deps)

	_, err := wf.Execute(context.Background(), mustJSON(map[string]any{"file_path": path, "content": "hello\n"}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = wf.Execute(context.Background(), mustJSON(map[string]any{"file_path": path, "content": "hello world\n"}))
	if err != nil {
		t.Fatal(err)
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("got %d changes, want 2", len(cs))
	}
	if cs[0].Op != history.OpCreate || cs[0].Before.Exists || !cs[0].After.Exists || cs[0].Tool != "write_file" {
		t.Errorf("create change mismatch: %+v", cs[0])
	}
	if string(cs[0].After.Content) != "hello\n" {
		t.Errorf("cs[0].After.Content = %q, want %q", cs[0].After.Content, "hello\n")
	}

	if cs[1].Op != history.OpUpdate || !cs[1].Before.Exists || !cs[1].After.Exists || cs[1].Tool != "write_file" {
		t.Errorf("update change mismatch: %+v", cs[1])
	}
	assertMatchesDisk(t, cs[1])
}

func TestWriteFileHugeIsWithheldNotQueued(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.txt")
	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	wf := NewWriteFile(deps)

	huge := strings.Repeat("x", 9<<20) // 9 MiB
	_, err := wf.Execute(context.Background(), mustJSON(map[string]any{"file_path": path, "content": huge}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if c.After.Content != nil || c.After.Size != 9<<20 {
		t.Fatalf("huge write carried content: %+v", c.After)
	}
}

func TestEditFileRecordsUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	ef := NewEditFile(deps)

	_, err := ef.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": path,
		"edits":     []map[string]string{{"old_string": "line2", "new_string": "line2 modified"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if c.Op != history.OpUpdate || c.Tool != "edit_file" {
		t.Fatalf("edit change mismatch: %+v", c)
	}
	assertMatchesDisk(t, c)
}

func TestEditFilePartialRecordsOnlyWhenSomethingApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("line1\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	ef := NewEditFile(deps)

	// Partial with 0 applied
	_, err := ef.Execute(context.Background(), mustJSON(map[string]any{
		"file_path":     path,
		"apply_partial": true,
		"edits":         []map[string]string{{"old_string": "nonexistent", "new_string": "foo"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.all()) != 0 {
		t.Fatalf("expected 0 changes for 0 applied, got %d", len(f.all()))
	}

	// Partial with 1 applied
	_, err = ef.Execute(context.Background(), mustJSON(map[string]any{
		"file_path":     path,
		"apply_partial": true,
		"edits": []map[string]string{
			{"old_string": "nonexistent", "new_string": "foo"},
			{"old_string": "line1", "new_string": "line1 changed"},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if c.Op != history.OpUpdate || c.Tool != "edit_file" {
		t.Fatalf("expected 1 update, got %+v", c)
	}
	assertMatchesDisk(t, c)
}

func TestFindReplaceRecordsEachFileWithItsFullBefore(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "f1.txt")
	p2 := filepath.Join(dir, "f2.txt")
	c1 := "small TARGET text\n"
	c2 := strings.Repeat("a", 300*1024) + " TARGET end\n"
	if err := os.WriteFile(p1, []byte(c1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, []byte(c2), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record, WorkspaceFn: func(_ context.Context) string { return dir }}
	fr := NewFindReplace(deps)

	_, err := fr.Execute(context.Background(), mustJSON(map[string]any{
		"path":        dir,
		"pattern":     "TARGET",
		"replacement": "REPLACED",
		"dry_run":     false,
		"dirty_ok":    true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(cs))
	}
	for _, c := range cs {
		if c.Op != history.OpUpdate || c.Tool != "find_replace" {
			t.Errorf("change mismatch: %+v", c)
		}
		if len(c.Before.Content) == 0 {
			t.Errorf("expected full before content for %s, got empty", c.Path)
		}
		assertMatchesDisk(t, c)
	}
}

func TestCopyFileRecordsCopyWithSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("copy content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	cf := NewCopyFile(deps)

	_, err := cf.Execute(context.Background(), mustJSON(map[string]any{"from": src, "to": dst, "dirty_ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if c.Op != history.OpCopy || c.From != src || c.Before.Exists || !c.After.Exists {
		t.Fatalf("copy change mismatch: %+v", c)
	}
	assertMatchesDisk(t, c)
}

func TestCopyFileOverExistingRecordsTheLostDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("new content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old destination\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	cf := NewCopyFile(deps)

	_, err := cf.Execute(context.Background(), mustJSON(map[string]any{"from": src, "to": dst, "overwrite": true, "dirty_ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if string(c.Before.Content) != "old destination\n" {
		t.Fatalf("expected Before.Content == 'old destination\\n', got %q", string(c.Before.Content))
	}
	assertMatchesDisk(t, c)
}

func TestRenameFileRecordsRename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("rename content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	rf := NewRenameFile(deps)

	_, err := rf.Execute(context.Background(), mustJSON(map[string]any{"from": src, "to": dst, "dirty_ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if c.Op != history.OpRename || c.From != src || !bytes.Equal(c.Before.SHA(), c.After.SHA()) {
		t.Fatalf("rename change mismatch: %+v", c)
	}
}

func TestRenameOverExistingRecordsDeleteThenRename(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	if err := os.WriteFile(src, []byte("src content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("overwritten destination\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	rf := NewRenameFile(deps)

	_, err := rf.Execute(context.Background(), mustJSON(map[string]any{"from": src, "to": dst, "overwrite": true, "dirty_ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(cs))
	}
	if cs[0].Op != history.OpDelete || string(cs[0].Before.Content) != "overwritten destination\n" {
		t.Errorf("delete change mismatch: %+v", cs[0])
	}
	if cs[1].Op != history.OpRename || cs[1].From != src {
		t.Errorf("rename change mismatch: %+v", cs[1])
	}
}

func TestDeleteFileRecordsFullBefore(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "del.txt")
	if err := os.WriteFile(p, []byte("to be deleted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	df := NewDeleteFile(deps)

	_, err := df.Execute(context.Background(), mustJSON(map[string]any{"file_path": p, "dirty_ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if c.Op != history.OpDelete || string(c.Before.Content) != "to be deleted\n" || c.After.Exists {
		t.Fatalf("delete change mismatch: %+v", c)
	}
}

func TestDeleteEmptyFileRecordsExistingEmptySide(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	df := NewDeleteFile(deps)

	_, err := df.Execute(context.Background(), mustJSON(map[string]any{"file_path": p, "dirty_ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if !c.Before.Exists || len(c.Before.Content) != 0 || c.After.Exists {
		t.Fatalf("empty delete change mismatch: %+v", c)
	}
}

func TestDeleteEmptyDirRecordsKindDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "emptydir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := WriteDeps{HistoryFn: f.record}
	df := NewDeleteFile(deps)

	_, err := df.Execute(context.Background(), mustJSON(map[string]any{"file_path": sub, "allow_dir": true}))
	if err != nil {
		t.Fatal(err)
	}
	c := f.only(t)
	if c.Op != history.OpDelete || c.Kind != history.KindDir {
		t.Fatalf("delete dir change mismatch: %+v", c)
	}
}

func TestUndoEditRecordsRevertWithWrittenSHA(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "u.txt")
	if err := os.WriteFile(p, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var f fakeHistory
	deps := undoDeps()
	deps.HistoryFn = f.record
	wf := NewWriteFile(deps)

	_, err := wf.Execute(context.Background(), mustJSON(map[string]any{"file_path": p, "content": "v2\n"}))
	if err != nil {
		t.Fatal(err)
	}

	// External edit modifies the file to v3
	if err := os.WriteFile(p, []byte("v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ue := NewUndoEdit(deps)
	_, err = ue.Execute(context.Background(), mustJSON(map[string]any{"file_path": p, "force": true}))
	if err != nil {
		t.Fatal(err)
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(cs))
	}
	v2SHA := sha256.Sum256([]byte("v2\n"))
	rev := cs[1]
	if rev.Op != history.OpRevert || rev.Reason != "undo_edit" || !bytes.Equal(rev.RevertsSHA, v2SHA[:]) ||
		string(rev.Before.Content) != "v3\n" || string(rev.After.Content) != "v1\n" {
		t.Fatalf("undo revert mismatch: %+v", rev)
	}
}

func TestUndoOfCreateRecordsRevertDeletingTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "create_and_undo.txt")

	var f fakeHistory
	deps := undoDeps()
	deps.HistoryFn = f.record
	wf := NewWriteFile(deps)

	_, err := wf.Execute(context.Background(), mustJSON(map[string]any{"file_path": p, "content": "fresh\n"}))
	if err != nil {
		t.Fatal(err)
	}

	ue := NewUndoEdit(deps)
	_, err = ue.Execute(context.Background(), mustJSON(map[string]any{"file_path": p}))
	if err != nil {
		t.Fatal(err)
	}

	cs := f.all()
	if len(cs) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(cs))
	}
	if cs[1].Op != history.OpRevert || cs[1].After.Exists {
		t.Fatalf("undo create mismatch: %+v", cs[1])
	}
}

func TestHistoryOffRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "off.txt")

	var f fakeHistory
	deps := WriteDeps{
		HistoryFn:        f.record,
		HistoryEnabledFn: func() bool { return false },
		Reads:            NewReadTracker(),
		Writes:           NewWriteTracker(),
		Undo:             NewUndoStore(),
	}

	wf := NewWriteFile(deps)
	_, _ = wf.Execute(context.Background(), mustJSON(map[string]any{"file_path": p, "content": "test\n"}))

	ef := NewEditFile(deps)
	_, _ = ef.Execute(context.Background(), mustJSON(map[string]any{
		"file_path": p,
		"edits":     []map[string]string{{"old_string": "test", "new_string": "updated"}},
	}))

	cf := NewCopyFile(deps)
	_, _ = cf.Execute(context.Background(), mustJSON(map[string]any{"from": p, "to": filepath.Join(dir, "p2.txt"), "dirty_ok": true}))

	df := NewDeleteFile(deps)
	_, _ = df.Execute(context.Background(), mustJSON(map[string]any{"file_path": filepath.Join(dir, "p2.txt"), "dirty_ok": true}))

	ue := NewUndoEdit(deps)
	_, _ = ue.Execute(context.Background(), mustJSON(map[string]any{"file_path": p}))

	if len(f.all()) != 0 {
		t.Fatalf("expected 0 changes when history off, got %d", len(f.all()))
	}
}

func TestConcurrentWritesRecordInLockOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.txt")
	if err := os.WriteFile(path, []byte("init\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	type recordedItem struct {
		seq int64
		c   history.Change
	}
	var (
		mu       sync.Mutex
		recorded []recordedItem
		seqGen   atomic.Int64
	)

	record := func(_ context.Context, c history.Change) {
		seq := seqGen.Add(1)
		mu.Lock()
		recorded = append(recorded, recordedItem{seq: seq, c: c})
		mu.Unlock()
	}

	deps := WriteDeps{HistoryFn: record}
	wf := NewWriteFile(deps)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(idx int) {
			defer wg.Done()
			content := fmt.Sprintf("write %d\n", idx)
			_, _ = wf.Execute(context.Background(), mustJSON(map[string]any{
				"file_path": path,
				"content":   content,
			}))
		}(i)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(recorded) != n {
		t.Fatalf("recorded %d writes, want %d", len(recorded), n)
	}

	// Verify At is non-decreasing in sequence order, and each change's After.SHA equals next's Before.SHA
	for i := range len(recorded) - 1 {
		cur := recorded[i]
		next := recorded[i+1]
		if next.c.At.Before(cur.c.At) {
			t.Errorf("timestamp decreased between seq %d and %d", cur.seq, next.seq)
		}
		if !bytes.Equal(cur.c.After.SHA(), next.c.Before.SHA()) {
			t.Errorf("broken sha chain between seq %d and %d", cur.seq, next.seq)
		}
	}
}
