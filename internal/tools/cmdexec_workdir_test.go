package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A working directory that does not exist used to surface as Go's
// `fork/exec /opt/homebrew/bin/go: no such file or directory`: chdir fails in
// the child with ENOENT and os/exec reports it against the BINARY, which
// exists. The caller went hunting for a PATH problem (#522). Every exec path —
// run_task and mutation_test (RunTaskArgv), run_command (RunArgv) — must name
// the directory instead.
func TestRunArgv_MissingWorkingDirIsNamed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "plumb")
	for name, run := range map[string]func() (ExecResult, error){
		"RunArgv": func() (ExecResult, error) {
			return RunArgv(context.Background(), missing, []string{"echo", "hi"}, time.Minute)
		},
		"RunTaskArgv": func() (ExecResult, error) {
			return RunTaskArgv(context.Background(), missing, []string{"echo", "hi"}, nil, time.Minute)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run()
			if err == nil {
				t.Fatal("expected an error for a working directory that does not exist")
			}
			var wd *WorkingDirError
			if !errors.As(err, &wd) || wd.Dir != missing {
				t.Fatalf("error %q is not a *WorkingDirError for %s", err, missing)
			}
			msg := err.Error()
			if !strings.Contains(msg, missing) || !strings.Contains(msg, "does not exist") {
				t.Errorf("error must name the missing directory %s, got %q", missing, msg)
			}
			if strings.Contains(msg, "fork/exec") {
				t.Errorf("error still blames the binary: %q", msg)
			}
		})
	}
}

// A regular file where the directory should be fails the same chdir, with
// ENOTDIR, and was misreported the same way.
func TestRunArgv_WorkingDirThatIsAFileIsNamed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plumb")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RunArgv(context.Background(), file, []string{"echo", "hi"}, time.Minute)
	var wd *WorkingDirError
	if !errors.As(err, &wd) || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("want a *WorkingDirError saying %s is not a directory, got %v", file, err)
	}
}

// Positive control: an existing working directory still runs, and an empty one
// still means "inherit", so the check cannot pass by refusing everything.
func TestRunArgv_ExistingOrEmptyWorkingDirRuns(t *testing.T) {
	for _, dir := range []string{"", t.TempDir()} {
		res, err := RunArgv(context.Background(), dir, []string{"echo", "hi"}, time.Minute)
		if err != nil || res.ExitCode != 0 {
			t.Errorf("dir %q: exit=%d err=%v, want a clean run", dir, res.ExitCode, err)
		}
	}
}

// run_task adds WHERE the directory came from, so the remedy names the setting
// to fix rather than only the path it produced.
func TestRunTask_MissingWorkingDirNamesItsSetting(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "plumb")
	source := `[tasks.go] working_dir = "plumb" in /a/.plumb/config.toml`
	tool := NewTasks(WriteDeps{WorkspaceFn: func(context.Context) string { return root }},
		func(_ context.Context, req TaskRequest) (TaskCommand, error) {
			return TaskCommand{
				Slot: req.Slot, Provenance: "project", Steps: [][]string{{"echo", "hi"}},
				WorkingDir: missing, WorkingDirSource: source,
			}, nil
		})
	_, err := runTask(t, tool, `{"slot":"build"}`)
	if err == nil {
		t.Fatal("expected run_task to fail for a missing working directory")
	}
	for _, want := range []string{"run_task build", missing, "does not exist", source} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must contain %q, got %q", want, err)
		}
	}
}

// mutation_test reports a step that could not start through its output; the
// same origin must reach it.
func TestMutationRunStep_MissingWorkingDirNamesItsSetting(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "plumb")
	source := `[tasks.go] working_dir = "plumb" in /a/.plumb/config.toml`
	mt := NewMutationTest(WriteDeps{}, nil)
	out := mt.runStep(context.Background(), TaskCommand{
		Steps: [][]string{{"echo", "hi"}}, WorkingDir: missing, WorkingDirSource: source,
	}, time.Minute)
	if !out.startErr {
		t.Fatalf("a missing working directory must be a start error, got %+v", out)
	}
	for _, want := range []string{missing, "does not exist", source} {
		if !strings.Contains(out.output, want) {
			t.Errorf("step output must contain %q, got %q", want, out.output)
		}
	}
}
