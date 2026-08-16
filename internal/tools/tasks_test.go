package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTask(t *testing.T, tool *Tasks, args string) (string, error) {
	t.Helper()
	return tool.Execute(context.Background(), json.RawMessage(args))
}

func TestRunTask_ValidateSlot(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(string, string) (TaskCommand, error) { return TaskCommand{}, nil })
	if _, err := runTask(t, tool, `{"slot":"deploy"}`); err == nil {
		t.Error("expected an error for an unknown slot")
	}
}

func TestRunTask_TargetMustBeShellSafe(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(string, string) (TaskCommand, error) { return TaskCommand{}, nil })
	if _, err := runTask(t, tool, `{"slot":"test","target":"foo; rm -rf /"}`); err == nil {
		t.Error("expected a target with shell metacharacters to be refused")
	}
}

func TestRunTask_NoResolver(t *testing.T) {
	tool := NewTasks(WriteDeps{}, nil)
	if _, err := runTask(t, tool, `{"slot":"build"}`); err == nil {
		t.Error("expected an error when no resolver is wired")
	}
}

func TestRunTask_ResolverErrorPropagates(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{}, errors.New("untrusted: run `plumb trust`")
	})
	_, err := runTask(t, tool, `{"slot":"build"}`)
	if err == nil || !strings.Contains(err.Error(), "plumb trust") {
		t.Errorf("expected the resolver's trust error, got %v", err)
	}
}

// TestRunTask_RunsInTheResolvedWorkingDir is PLAN-325's acceptance criterion,
// asserted where it is decided.
//
// The holder-repository shape: the workspace root holds no module (only a
// go.work), the module sits in a subdirectory, and running from the root fails
// instantly while the tree compiles perfectly. Every Go task command in such a
// repository — plumb-ops itself — exited 1 in 0.02s, and mutation_test was
// unusable there because its compile gate could never pass.
//
// It asserts the cwd by having the command PRINT it, rather than by inspecting
// the TaskCommand: the question is where the process ran, and only the process
// can answer that. WorkspaceFn deliberately returns the root, so a regression
// that ignores WorkingDir has somewhere wrong to fall back to and the assertion
// can tell the two apart.
func TestRunTask_RunsInTheResolvedWorkingDir(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "module")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}

	tool := NewTasks(WriteDeps{WorkspaceFn: func() string { return root }},
		func(slot, _ string) (TaskCommand, error) {
			return TaskCommand{
				Slot:       slot,
				Provenance: "project",
				Steps:      [][]string{{"/bin/pwd"}},
				WorkingDir: module,
			}, nil
		})
	out, err := runTask(t, tool, `{"slot":"build"}`)
	if err != nil {
		t.Fatalf("run_task: %v", err)
	}

	got := reportedCwd(t, out)
	assertSameDir(t, got, module, "the command did not run in the resolved working_dir")
	if sameDir(got, root) {
		t.Errorf("the command ran in the workspace ROOT (%s), the holder directory that cannot build; output:\n%s", root, out)
	}
}

// reportedCwd pulls the directory /bin/pwd printed out of a run_task report.
func reportedCwd(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "/") && !strings.HasPrefix(line, "$") {
			return line
		}
	}
	t.Fatalf("no directory line in the run_task report:\n%s", out)
	return ""
}

// assertSameDir compares directories by IDENTITY (the package's sameDir, which
// is os.SameFile-based), never by spelling.
//
// A string comparison here is a macOS-only time bomb: t.TempDir() hands back a
// path under /var, which is a symlink to /private/var, so the path the test
// holds and the path the child process reports are two spellings of one
// directory — green on Linux CI, red locally, for a reason that has nothing to
// do with the behaviour under test.
func assertSameDir(t *testing.T, got, want, msg string) {
	t.Helper()
	if !sameDir(got, want) {
		t.Errorf("%s: ran in %s, want %s", msg, got, want)
	}
}

// TestRunTask_FallsBackToTheWorkspaceRoot is the other direction: with no
// working_dir configured, behaviour is exactly what it was before the field
// existed. Without this, the test above would pass against a runner that had
// simply stopped consulting the workspace at all.
func TestRunTask_FallsBackToTheWorkspaceRoot(t *testing.T) {
	root := t.TempDir()
	tool := NewTasks(WriteDeps{WorkspaceFn: func() string { return root }},
		func(slot, _ string) (TaskCommand, error) {
			return TaskCommand{Slot: slot, Provenance: "default", Steps: [][]string{{"/bin/pwd"}}}, nil
		})
	out, err := runTask(t, tool, `{"slot":"build"}`)
	if err != nil {
		t.Fatalf("run_task: %v", err)
	}
	assertSameDir(t, reportedCwd(t, out), root,
		"with no working_dir the command must still run in the workspace root")
}

func TestRunTask_RunsStepsAndStopsOnFailure(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{
			Slot:       "verify",
			Provenance: "default",
			Steps:      [][]string{{"echo", "building"}, {"false"}, {"echo", "should-not-run"}},
		}, nil
	})
	out, err := runTask(t, tool, `{"slot":"verify"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "building") || !strings.Contains(out, "stopped") {
		t.Errorf("expected the first step to run and the failing step to stop the chain:\n%s", out)
	}
	if strings.Contains(out, "should-not-run") {
		t.Error("a step after the failed one should not run")
	}
}

func TestRunTask_AllStepsOK(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		return TaskCommand{Slot: "build", Provenance: "global", Steps: [][]string{{"true"}}}, nil
	})
	out, err := runTask(t, tool, `{"slot":"build"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "→ ok") {
		t.Errorf("expected an ok result:\n%s", out)
	}
}
