package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func deleteBatchTool() *DeleteFile { return NewDeleteFile(WriteDeps{}) }

func runDelete(t *testing.T, tool *DeleteFile, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw)
}

// TestDeleteFile_BatchRemovesTreeInOneCall is the F4(a) ergonomics fix: removing
// a 40-file tree previously cost 41+ calls because delete_file took one path.
// Naming the files and their directories in one batch must work, with the
// directories removed last, deepest first.
func TestDeleteFile_BatchRemovesTreeInOneCall(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "pkg")
	deep := filepath.Join(tree, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, 4)
	for _, rel := range []string{"pkg/a.go", "pkg/b.go", "pkg/deep/c.go"} {
		p := filepath.Join(root, rel)
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	// Directories listed BEFORE their contents on purpose: ordering is the
	// tool's job, not the caller's.
	paths = append([]string{tree, deep}, paths...)

	out, err := runDelete(t, deleteBatchTool(), map[string]any{
		"paths": paths, "allow_dir": true, "dirty_ok": true,
	})
	if err != nil {
		t.Fatalf("batch delete of a whole tree should succeed, got: %v\n%s", err, out)
	}
	if !strings.Contains(out, "deleted 5 path(s)") {
		t.Errorf("expected a 5-path report, got:\n%s", out)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Errorf("the tree root should be gone, stat err = %v", err)
	}
}

// TestDeleteFile_BatchValidatesBeforeRemoving pins the all-or-nothing guarantee:
// one bad path in the batch must leave every other path untouched.
func TestDeleteFile_BatchValidatesBeforeRemoving(t *testing.T) {
	root := t.TempDir()
	good := filepath.Join(root, "good.txt")
	if err := os.WriteFile(good, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "does-not-exist.txt")

	_, err := runDelete(t, deleteBatchTool(), map[string]any{
		"paths": []string{good, missing}, "dirty_ok": true,
	})
	if err == nil {
		t.Fatal("expected the batch to be refused for the missing path")
	}
	if _, statErr := os.Stat(good); statErr != nil {
		t.Errorf("a refused batch must not have deleted anything, but good.txt is gone: %v", statErr)
	}
}

// TestDeleteFile_BatchRefusesDirWithoutAllowDir: the per-path rules are
// unchanged by batching — a directory still needs allow_dir, and the refusal
// must happen before any file in the batch is removed.
func TestDeleteFile_BatchRefusesDirWithoutAllowDir(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	dir := filepath.Join(root, "sub")
	if err := os.WriteFile(file, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runDelete(t, deleteBatchTool(), map[string]any{
		"paths": []string{file, dir}, "dirty_ok": true,
	})
	if err == nil || !strings.Contains(err.Error(), "allow_dir") {
		t.Fatalf("expected an allow_dir refusal, got: %v", err)
	}
	if _, statErr := os.Stat(file); statErr != nil {
		t.Errorf("the refused batch must not have deleted the file: %v", statErr)
	}
}

// TestDeleteFile_NonEmptyDirStillRefused pins that batching did NOT introduce a
// recursive delete: a non-empty directory whose contents are not also named is
// still refused by os.Remove.
func TestDeleteFile_NonEmptyDirStillRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sub")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runDelete(t, deleteBatchTool(), map[string]any{
		"paths": []string{dir}, "allow_dir": true, "dirty_ok": true,
	})
	if err == nil {
		t.Fatal("a non-empty directory must still be refused — there is no recursive delete")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "kept.txt")); statErr != nil {
		t.Errorf("the contained file must survive: %v", statErr)
	}
}

// TestDeleteFile_BatchArgValidation covers the request-shape rules.
func TestDeleteFile_BatchArgValidation(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := deleteBatchTool()

	if _, err := runDelete(t, tool, map[string]any{}); err == nil {
		t.Error("expected a refusal when neither file_path nor paths is given")
	}
	if _, err := runDelete(t, tool, map[string]any{"file_path": p, "paths": []string{p}}); err == nil {
		t.Error("expected a refusal when both file_path and paths are given")
	}
	if _, err := runDelete(t, tool, map[string]any{"paths": []string{p, ""}}); err == nil {
		t.Error("expected a refusal for an empty string in paths")
	}
	tooMany := make([]string, maxDeletePaths+1)
	for i := range tooMany {
		tooMany[i] = filepath.Join(root, fmt.Sprintf("f%d.txt", i))
	}
	if _, err := runDelete(t, tool, map[string]any{"paths": tooMany}); err == nil ||
		!strings.Contains(err.Error(), "too many paths") {
		t.Errorf("expected a batch-cap refusal, got: %v", err)
	}
	if _, statErr := os.Stat(p); statErr != nil {
		t.Errorf("no validation failure should have deleted anything: %v", statErr)
	}
}

// TestDeleteFile_SinglePathResponseUnchanged: batching must not alter the
// single-path response shape that existing callers and tests rely on.
func TestDeleteFile_SinglePathResponseUnchanged(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runDelete(t, deleteBatchTool(), map[string]any{"file_path": p, "dirty_ok": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "path(s)") {
		t.Errorf("single-path response should not use the batch report form, got: %q", out)
	}
	if !strings.HasPrefix(out, "deleted "+p) {
		t.Errorf("unexpected single-path response: %q", out)
	}
}

// TestDeleteFile_BatchHoldsLocksAcrossValidateAndRemove is the regression test
// for a data-loss defect an independent review found. The batch refactor moved
// lockPath from covering stat + dirty-check + remove down to covering only the
// remove, so a peer's edit_file could write uncommitted content into a path
// already judged clean and have it deleted anyway despite dirty_ok:false.
//
// Proved by contention rather than by timing: a peer goroutine tries to take the
// same path's lock while the batch is mid-flight. If the batch holds the lock
// from validation through removal, the peer cannot acquire it until the batch is
// done — which is exactly the guarantee that closes the window.
func TestDeleteFile_BatchHoldsLocksAcrossValidateAndRemove(t *testing.T) {
	root := t.TempDir()
	paths := make([]string, 0, 4)
	for i := range 4 {
		p := filepath.Join(root, fmt.Sprintf("f%d.txt", i))
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	// Hold the LAST path's lock. lockPaths acquires in sorted order, so the batch
	// gets the earlier paths and then blocks here — with none of them yet removed,
	// which is the property under test.
	unlock := lockPath(paths[len(paths)-1])

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = runDelete(t, deleteBatchTool(), map[string]any{"paths": paths, "dirty_ok": true})
	}()

	// Give the batch time to get as far as it can, then assert on STATE rather
	// than on a select race: nothing may be gone while the lock is held.
	time.Sleep(250 * time.Millisecond)
	for _, p := range paths {
		if _, err := os.Stat(p); err != nil {
			unlock()
			<-done
			t.Fatalf("batch removed %s while a peer held a lock it needs — validation and removal are not both covered (%v)", p, err)
		}
	}
	select {
	case <-done:
		t.Fatal("batch completed while a peer held one of its path locks")
	default:
	}

	unlock()
	<-done
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %s removed once the lock was free, stat err = %v", p, err)
		}
	}
}

// TestDeleteFile_CaseVariantSpellingsAreOneTarget: on a case-insensitive
// filesystem two spellings of one file resolve to different strings but the same
// file. Deduping by the raw string let both through as separate targets, so the
// second removal failed "no such file" after the first succeeded — a partial
// failure for a batch that did exactly what was asked. Skipped where the
// filesystem is genuinely case-sensitive, since there the two ARE distinct files.
func TestDeleteFile_CaseVariantSpellingsAreOneTarget(t *testing.T) {
	root := t.TempDir()
	lower := filepath.Join(root, "casetest.txt")
	upper := filepath.Join(root, "CASETEST.txt")
	if err := os.WriteFile(lower, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upper); err != nil {
		t.Skip("case-sensitive filesystem: the two spellings are different files")
	}

	out, err := runDelete(t, deleteBatchTool(), map[string]any{
		"paths": []string{lower, upper}, "dirty_ok": true,
	})
	if err != nil {
		t.Fatalf("two spellings of one file must collapse to one target, got: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(lower); !os.IsNotExist(statErr) {
		t.Errorf("the file should be gone, stat err = %v", statErr)
	}
}

// TestDeleteFile_DuplicatePathIsNotAnError: naming the same path twice is
// redundant, not fatal — the second occurrence would otherwise fail its stat
// after the first removal and abort a batch that did exactly what was asked.
func TestDeleteFile_DuplicatePathIsNotAnError(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.txt")
	if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runDelete(t, deleteBatchTool(), map[string]any{
		"paths": []string{p, p}, "dirty_ok": true,
	})
	if err != nil {
		t.Fatalf("a duplicated path should be tolerated, got: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(p); !os.IsNotExist(statErr) {
		t.Errorf("the path should be gone, stat err = %v", statErr)
	}
}
