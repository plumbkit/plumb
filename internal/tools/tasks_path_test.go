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
// command keeps its argv, language, provenance and trust, and only the working
// directory moves — inside the workspace, through the same boundary every other
// tool uses (review round 1, B1: a work-tree of the same repository OUTSIDE the
// workspace is refused).

func callRunTask(t *testing.T, tool *Tasks, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return tool.Execute(context.Background(), raw)
}

// pathFixture builds a workspace holding a repository with one commit and a
// linked work-tree INSIDE that workspace, and a Tasks tool whose boundary is the
// workspace — the shape every other plumb tool is tested against.
func pathFixture(t *testing.T) (ws, repo, wt string, tool *Tasks) {
	t.Helper()
	requireGit(t)
	ws = t.TempDir()
	repo = filepath.Join(ws, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "init", "-q")
	gitRun(t, repo, "config", "user.email", "t@example.com")
	gitRun(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "f.txt")
	gitRun(t, repo, "commit", "-qm", "init")
	wt = filepath.Join(ws, "wt")
	gitRun(t, repo, "worktree", "add", "-b", "topic", wt)
	tool = NewTasks(WriteDeps{
		Boundary:    testBoundaryGuard(ws),
		WorkspaceFn: func(context.Context) string { return ws },
	}, nil)
	return ws, repo, wt, tool
}

func TestRerootForPath_LinkedWorktree(t *testing.T) {
	ws, repo, wt, tool := pathFixture(t)
	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "build", "./..."}}, WorkingDir: repo, Root: repo, Provenance: "default"}

	got, err := tool.rerootForPath(context.Background(), ws, cmd, wt)
	if err != nil {
		t.Fatalf("re-rooting into a linked work-tree: %v", err)
	}
	if want := paths.Canonical(wt); got.WorkingDir != want {
		t.Errorf("WorkingDir = %q, want %q", got.WorkingDir, want)
	}
	if got.Root != paths.Canonical(wt) {
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

// TestRerootForPath_RefusesAWorktreeOutsideTheWorkspace is B1's regression: a
// work-tree of the SAME repository is still refused when it lives outside the
// workspace, because every other plumb tool confines itself to the workspace and
// a stored trusted command is not an exception.
func TestRerootForPath_RefusesAWorktreeOutsideTheWorkspace(t *testing.T) {
	ws, repo, _, tool := pathFixture(t)
	outside := filepath.Join(t.TempDir(), "far")
	gitRun(t, repo, "worktree", "add", "-b", "far", outside)

	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: repo, Root: repo}
	_, err := tool.rerootForPath(context.Background(), ws, cmd, outside)
	if err == nil {
		t.Fatal("a work-tree outside the workspace must be refused, same repository or not")
	}
	if !IsWorkspaceBoundaryError(err) {
		t.Errorf("want a workspace-boundary refusal, got: %v", err)
	}
}

func TestRerootForPath_RefusesADirectoryOutsideTheWorkspace(t *testing.T) {
	ws, repo, _, tool := pathFixture(t)
	elsewhere := t.TempDir() // a real directory, just not inside the workspace

	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: repo, Root: repo}
	if _, err := tool.rerootForPath(context.Background(), ws, cmd, elsewhere); err == nil || !IsWorkspaceBoundaryError(err) {
		t.Fatalf("a directory outside the workspace = %v, want a boundary refusal", err)
	}
}

// TestRerootForPath_AcceptsADirectoryInsideTheWorkspace pins the breadth the
// boundary rule buys: any directory inside the workspace is runnable, whether or
// not it is its own repository — the same breadth a [tasks.<lang>] working_dir
// already has.
func TestRerootForPath_AcceptsADirectoryInsideTheWorkspace(t *testing.T) {
	ws, repo, _, tool := pathFixture(t)
	nested := filepath.Join(ws, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: repo, Root: repo}
	got, err := tool.rerootForPath(context.Background(), ws, cmd, nested)
	if err != nil {
		t.Fatalf("a directory inside the workspace must be runnable: %v", err)
	}
	if want := paths.Canonical(nested); got.WorkingDir != want {
		t.Errorf("WorkingDir = %q, want %q", got.WorkingDir, want)
	}
}

// TestRerootForPath_RelativePathIsWorkspaceRelative pins the rule dogfooding
// corrected: `path` resolves like every other plumb path argument — against the
// pinned WORKSPACE — and not against the directory the command would run in. With
// plumb-ops's own shape (`[tasks.go] working_dir = "plumb"`), the old rule sent
// `path: "plumb-wt-497"` to plumb/plumb-wt-497 and refused it, while the caller
// meant the work-tree beside the submodule checkout.
func TestRerootForPath_RelativePathIsWorkspaceRelative(t *testing.T) {
	ws, repo, wt, tool := pathFixture(t)
	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: repo, Root: repo}

	got, err := tool.rerootForPath(context.Background(), ws, cmd, filepath.Base(wt))
	if err != nil {
		t.Fatalf("a workspace-relative path must resolve: %v", err)
	}
	if want := paths.Canonical(wt); got.WorkingDir != want {
		t.Errorf("WorkingDir = %q, want %q — relative to the workspace, not to the run directory", got.WorkingDir, want)
	}
}

func TestRerootForPath_RefusesAPathThatIsNotThere(t *testing.T) {
	ws, repo, _, tool := pathFixture(t)
	cmd := TaskCommand{Slot: "test", Steps: [][]string{{"go", "version"}}, WorkingDir: repo, Root: repo}

	if _, err := tool.rerootForPath(context.Background(), ws, cmd, filepath.Join(ws, "missing")); err == nil ||
		!strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing path = %v, want a refusal naming it", err)
	} else if !strings.Contains(err.Error(), "relative to the workspace") {
		t.Errorf("the refusal must name the base it resolved against: %v", err)
	}
	file := filepath.Join(repo, "f.txt")
	if _, err := tool.rerootForPath(context.Background(), ws, cmd, file); err == nil ||
		!strings.Contains(err.Error(), "not a directory") {
		t.Errorf("a file as path = %v, want a refusal naming it", err)
	}
}

// TestRerootForPath_RefusesAnArgumentNamingTheOldTree is the guard that keeps a
// stored command honest: an argv element holding an absolute path inside the
// tree being left would point at the wrong checkout once the command moves.
func TestRerootForPath_RefusesAnArgumentNamingTheOldTree(t *testing.T) {
	ws, repo, wt, tool := pathFixture(t)
	cmd := TaskCommand{
		Slot: "test", WorkingDir: repo, Root: repo,
		Steps: [][]string{{"go", "test", filepath.Join(repo, "internal", "tools")}},
	}
	_, err := tool.rerootForPath(context.Background(), ws, cmd, wt)
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
	ws, repo, wt, _ := pathFixture(t)
	sawPath := false
	tool := NewTasks(WriteDeps{
		Boundary:    testBoundaryGuard(ws),
		WorkspaceFn: func(context.Context) string { return ws },
	}, func(_ context.Context, req TaskRequest) (TaskCommand, error) {
		sawPath = req.Path == wt
		return TaskCommand{
			Slot: "test", Steps: [][]string{{"git", "rev-parse", "--show-toplevel"}},
			WorkingDir: repo, Root: repo, Provenance: "default",
		}, nil
	})

	out, err := callRunTask(t, tool, map[string]any{"slot": "test", "path": wt})
	if err != nil {
		t.Fatalf("run_task with path: %v", err)
	}
	if !sawPath {
		t.Error("the resolver must receive the caller's path")
	}
	if want := paths.Canonical(wt); !strings.Contains(out, want) {
		t.Errorf("the command did not run in %s:\n%s", want, out)
	}
	if !strings.Contains(out, "running in ") {
		t.Errorf("the response must report the move:\n%s", out)
	}
}

// TestNoCommandError_RendersTheMakefileRemedy pins the rendering half of the
// PLAN-494 remedy: the tool supplies the sentence, the refusal carries it, and a
// slot with nothing workspace-specific to say stays as it was.
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
