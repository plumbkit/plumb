package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/paths"
)

// tasks_path_test.go covers run_task's `path` re-root (PLAN-494): the resolved
// command keeps its argv, language and provenance, and only the working
// directory moves — into a work-tree of the same repository, or a directory the
// workspace contains, and nowhere else.

func canonicalForTest(p string) string { return paths.Canonical(p) }

func callRunTask(t *testing.T, tool *Tasks, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw)
}

// linkedWorktreeFixture builds a repository with one committed file and a
// `git worktree add` checkout of a topic branch beside it.
func linkedWorktreeFixture(t *testing.T) (ws, wt string) {
	t.Helper()
	requireGit(t)
	ws = initTestRepo(t)
	wt = filepath.Join(t.TempDir(), "wt")
	gitRun(t, ws, "worktree", "add", "-b", "topic", wt)
	return ws, wt
}

func TestRerootForPath_LinkedWorktree(t *testing.T) {
	ws, wt := linkedWorktreeFixture(t)
	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "build", "./..."}}, WorkingDir: ws, Root: ws, Provenance: "default"}

	got, err := rerootForPath(context.Background(), ws, cmd, wt)
	if err != nil {
		t.Fatalf("re-rooting into a linked work-tree: %v", err)
	}
	if want := canonicalForTest(wt); got.WorkingDir != want {
		t.Errorf("WorkingDir = %q, want %q", got.WorkingDir, want)
	}
	if got.Root != canonicalForTest(wt) {
		t.Errorf("Root = %q, want the work-tree root: {workspace} must follow the command", got.Root)
	}
	if len(got.Notes) == 0 {
		t.Error("a moved run must say where it ran")
	}
	if len(got.Steps) != 1 || got.Steps[0][2] != "./..." {
		t.Errorf("the argv must not change, got %v", got.Steps)
	}
	if got.Provenance != "default" {
		t.Errorf("provenance = %q, want it unchanged: trust binds to the workspace's config", got.Provenance)
	}
}

func TestRerootForPath_AcceptsARepositoryInsideTheWorkspace(t *testing.T) {
	ws, _ := linkedWorktreeFixture(t)
	nested := filepath.Join(ws, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, nested, "init", "-q")

	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: ws, Root: ws}
	got, err := rerootForPath(context.Background(), ws, cmd, nested)
	if err != nil {
		t.Fatalf("a repository inside the workspace must be runnable inside it: %v", err)
	}
	if want := canonicalForTest(nested); got.WorkingDir != want {
		t.Errorf("WorkingDir = %q, want %q", got.WorkingDir, want)
	}
}

func TestRerootForPath_RefusesAnotherRepositoryOutsideTheWorkspace(t *testing.T) {
	ws, _ := linkedWorktreeFixture(t)
	elsewhere := initTestRepo(t) // its own temporary repository, outside ws

	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: ws, Root: ws}
	_, err := rerootForPath(context.Background(), ws, cmd, elsewhere)
	if err == nil {
		t.Fatal("running the workspace's command in an unrelated repository must be refused")
	}
	for _, want := range []string{"another repository", "not inside this workspace", "Nothing was run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q; got: %v", want, err)
		}
	}
}

func TestRerootForPath_RefusesAPathThatIsNotThere(t *testing.T) {
	ws, _ := linkedWorktreeFixture(t)
	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: ws, Root: ws}

	if _, err := rerootForPath(context.Background(), ws, cmd, filepath.Join(ws, "missing")); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing path = %v, want a refusal naming it", err)
	}
	file := filepath.Join(ws, "init.txt")
	if _, err := rerootForPath(context.Background(), ws, cmd, file); err == nil ||
		!strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a file as path = %v, want a refusal naming it", err)
	}
}

// TestRerootForPath_RefusesAnArgumentNamingTheOldTree is the guard that keeps a
// stored command honest: an argv element holding an absolute path inside the
// tree being left would point at the wrong checkout once the command moves.
func TestRerootForPath_RefusesAnArgumentNamingTheOldTree(t *testing.T) {
	ws, wt := linkedWorktreeFixture(t)
	cmd := TaskCommand{
		Slot: "test", WorkingDir: ws, Root: ws,
		Steps: [][]string{{"go", "test", filepath.Join(ws, "internal", "tools")}},
	}
	_, err := rerootForPath(context.Background(), ws, cmd, wt)
	if err == nil {
		t.Fatal("an argument naming the tree being left must be refused")
	}
	for _, want := range []string{"work-tree being left", "relative to the working directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal is missing %q; got: %v", want, err)
		}
	}
}

// TestRunTask_PathRunsTheCommandInTheWorktree is the end-to-end proof: the
// command actually runs with the work-tree as its working directory, and the
// report says so.
func TestRunTask_PathRunsTheCommandInTheWorktree(t *testing.T) {
	ws, wt := linkedWorktreeFixture(t)
	sawPath := false
	tool := NewTasks(WriteDeps{}, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		sawPath = req.Path == wt
		return TaskCommand{
			Slot: "test", Steps: [][]string{{"git", "rev-parse", "--show-toplevel"}},
			WorkingDir: ws, Root: ws, Provenance: "default",
		}, nil
	})

	out, err := callRunTask(t, tool, map[string]any{"slot": "test", "path": wt})
	if err != nil {
		t.Fatalf("run_task with path: %v", err)
	}
	if !sawPath {
		t.Error("the resolver must receive the caller's path")
	}
	if want := canonicalForTest(wt); !strings.Contains(out, want) {
		t.Errorf("the command did not run in %s:\n%s", want, out)
	}
	if !strings.Contains(out, "running in ") {
		t.Errorf("the response must report the move:\n%s", out)
	}
}

// TestNoCommandError_RendersTheMakefileRemedy pins the rendering half of the
// PLAN-494 remedy: the resolver supplies the sentence, the refusal carries it,
// and an unconfigured slot with nothing workspace-specific to say stays as it
// was.
func TestNoCommandError_RendersTheMakefileRemedy(t *testing.T) {
	withRemedy := noCommandError(TaskCommand{
		Language: "go", Configured: []string{"build"},
		Remedy: `This workspace's Makefile defines a "vuln" target`,
	}, "vuln")
	if !strings.Contains(withRemedy.Error(), `"vuln" target`) {
		t.Errorf("the remedy is missing from the refusal:\n%v", withRemedy)
	}
	bare := noCommandError(TaskCommand{Language: "go", Configured: []string{"build"}}, "vuln")
	if strings.Contains(bare.Error(), "target") {
		t.Errorf("a refusal with no remedy must not invent one:\n%v", bare)
	}
}
