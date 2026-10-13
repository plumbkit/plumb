package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// O_EXCL is what keeps git_init from writing through a .plumb/context.md that is a
// dangling symlink: a plain create would follow the link and make its target. The
// existing-file case has its own test (TestGitInit_InitPlumbDoesNotOverwriteExistingContext);
// this one pins the link case, which only O_EXCL covers (#600).
func TestGitInit_DoesNotWriteThroughADanglingContextMdLink(t *testing.T) {
	dir := t.TempDir()
	plumbDir := filepath.Join(dir, ".plumb")
	if err := os.MkdirAll(plumbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "elsewhere.md")
	if err := os.Symlink(target, filepath.Join(plumbDir, "context.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	var f fakeHistory
	gi := NewGitInit(WriteDeps{HistoryFn: f.record, WorkspaceFn: func(context.Context) string { return dir }})
	args, _ := json.Marshal(map[string]any{"path": dir, "init_plumb": true})
	if _, err := gi.Execute(context.Background(), args); err != nil {
		t.Fatalf("git_init: %v", err)
	}
	// Control: the init itself ran, so the absences below are not a refusal.
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Fatalf("control: git_init did not initialise the repository: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("git_init wrote through the dangling context.md link and created %s (err %v)", target, err)
	}
	if n := len(f.all()); n != 0 {
		t.Errorf("a create was recorded for a context.md that git_init did not create: %d row(s)", n)
	}
}
