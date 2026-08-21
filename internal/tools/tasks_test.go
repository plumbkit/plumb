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

// TestRunTask_ValidateSlot covers the two rejections that are now different
// things. A MALFORMED name is refused by the tool as input hygiene; a
// well-formed name with no command is refused by the resolver, which can say
// what IS configured. Before project-defined slots existed both were one
// closed-set check, and that check is what made the vocabulary Go-shaped.
func TestRunTask_ValidateSlot(t *testing.T) {
	tool := NewTasks(WriteDeps{}, func(string, string) (TaskCommand, error) { return TaskCommand{}, nil })

	for _, bad := range []string{"Deploy", "9deploy", "de ploy", "deploy!", ""} {
		args, _ := json.Marshal(map[string]any{"slot": bad})
		if _, err := runTask(t, tool, string(args)); err == nil {
			t.Errorf("slot %q is malformed and must be refused", bad)
		}
	}

	// Well formed but unconfigured: still an error, from the resolver.
	if _, err := runTask(t, tool, `{"slot":"deploy"}`); err == nil {
		t.Error("expected an error for a slot with no command")
	}
}

// TestRunTask_ProjectDefinedSlotReachesResolver is the point of the change: a
// slot the project named, not one of the built-in five, must reach the resolver
// rather than being refused by a closed set in this package.
func TestRunTask_ProjectDefinedSlotReachesResolver(t *testing.T) {
	var got string
	tool := NewTasks(WriteDeps{}, func(slot, _ string) (TaskCommand, error) {
		got = slot
		return TaskCommand{Slot: slot, Provenance: "project", Steps: [][]string{{"true"}}}, nil
	})
	if _, err := runTask(t, tool, `{"slot":"check"}`); err != nil {
		t.Fatalf("a project-defined slot must run: %v", err)
	}
	if got != "check" {
		t.Errorf("resolver saw slot %q, want \"check\"", got)
	}
}

// TestRunTask_SchemaHasNoSlotEnum pins the trade this change makes. An MCP
// client enforces an enum on its side, so a static enum and a project-defined
// vocabulary cannot both exist; the resolver's "configured slots" message is
// what replaces the client-side constraint.
func TestRunTask_SchemaHasNoSlotEnum(t *testing.T) {
	var schema struct {
		Properties struct {
			Slot map[string]any `json:"slot"`
		} `json:"properties"`
	}
	if err := json.Unmarshal((&Tasks{}).InputSchema(), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	if _, ok := schema.Properties.Slot["enum"]; ok {
		t.Error("slot must not declare an enum — it would re-close the vocabulary client-side")
	}
	if !strings.Contains(schema.Properties.Slot["description"].(string), "[tasks.<lang>]") {
		t.Error("with no enum, the description must say where the vocabulary comes from")
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

	tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return root }},
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
	for line := range strings.SplitSeq(out, "\n") {
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
	tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return root }},
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
