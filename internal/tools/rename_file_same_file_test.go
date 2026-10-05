package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/history"
)

// rename(2) does nothing when both names are hard links to one file, so a
// rename_file that reported success would record a delete and a rename that
// never happened.
func TestRenameOntoAHardLinkOfTheSameFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	dst := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(src, []byte("linked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(src, dst); err != nil {
		t.Skipf("hard links unsupported here: %v", err)
	}
	var f fakeHistory
	rf := NewRenameFile(WriteDeps{HistoryFn: f.record, ShowWriteDiff: true})
	_, err := rf.Execute(context.Background(), mustJSON(map[string]any{"from": src, "to": dst, "overwrite": true, "dirty_ok": true}))
	if err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("want a hard-link refusal, got %v", err)
	}
	if cs := f.all(); len(cs) != 0 {
		t.Fatalf("a refused rename recorded %d changes: %+v", len(cs), cs)
	}
	for _, p := range []string{src, dst} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s should still exist: %v", p, err)
		}
	}
}

// A case-only rename on a case-insensitive volume finds `to` already present —
// it is the source itself. Nothing is destroyed, so history must not record a
// delete of it and the response must not show its content as lost.
func TestCaseOnlyRenameDestroysNoDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "case.txt")
	dst := filepath.Join(dir, "CASE.txt")
	if err := os.WriteFile(src, []byte("case-only-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dst); err != nil {
		t.Skip("case-sensitive filesystem: a case-only rename has no existing destination")
	}
	var f fakeHistory
	rf := NewRenameFile(WriteDeps{HistoryFn: f.record, ShowWriteDiff: true})
	out, err := rf.Execute(context.Background(), mustJSON(map[string]any{"from": src, "to": dst, "overwrite": true, "dirty_ok": true}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "case-only-marker") {
		t.Fatalf("response shows the renamed file as a destroyed destination:\n%s", out)
	}
	c := f.only(t)
	if c.Op != history.OpRename || c.From != src || c.Path != dst {
		t.Fatalf("want one rename row, got %+v", c)
	}
	names, err := os.ReadDir(dir)
	if err != nil || len(names) != 1 || names[0].Name() != "CASE.txt" {
		t.Fatalf("directory should hold only CASE.txt: %v %v", names, err)
	}
}
