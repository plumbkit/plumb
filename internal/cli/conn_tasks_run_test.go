package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// TestScopedSteps_ShippedDefaults pins #538 against the commands plumb ships: an
// unscoped run builds exactly the argv it built before {run} and {verbose}
// existed, and each asked-for value lands where its runner reads it.
func TestScopedSteps_ShippedDefaults(t *testing.T) {
	for _, tc := range []struct {
		lang string
		sc   taskScope
		want string
	}{
		{"go", taskScope{}, "go test ./..."},
		{"go", taskScope{run: "TestA|TestB"}, "go test -run TestA|TestB ./..."},
		{"go", taskScope{verbose: true}, "go test -v ./..."},
		{"go", taskScope{target: "./internal/cli", run: "^TestX$", verbose: true}, "go test -v -run ^TestX$ ./internal/cli"},
		{"python", taskScope{}, "pytest"},
		{"python", taskScope{run: "slow and not db", verbose: true}, "pytest -v -k slow and not db"},
		{"rust", taskScope{}, "cargo test"},
		{"rust", taskScope{run: "parses"}, "cargo test -- parses"},
	} {
		tasks := config.TasksConfig{Test: config.DefaultTaskCommand(tc.lang, "test")}
		steps, err := buildScopedTaskSteps(tasks, tc.lang, "test", tc.sc)
		if err != nil || len(steps) != 1 {
			t.Errorf("%s %+v: steps %v, err %v", tc.lang, tc.sc, steps, err)
			continue
		}
		if got := strings.Join(steps[0], " "); got != tc.want {
			t.Errorf("%s %+v built %q, want %q", tc.lang, tc.sc, got, tc.want)
		}
	}
	// The filter is ONE element whatever it contains: no shell ever splits it.
	steps, _ := buildScopedTaskSteps(config.TasksConfig{Test: config.DefaultTaskCommand("python", "test")},
		"python", "test", taskScope{run: "a or b"})
	if len(steps) != 1 || steps[0][len(steps[0])-1] != "a or b" {
		t.Errorf("the run filter must stay one argv element, got %q", steps)
	}
}

// TestScopedSteps_RunWithoutPlaceholderIsRefused mirrors the {target} rule: a run
// filter on a command with nowhere to put it would run every test and report
// green, so it is refused with the stored command and the placeholder to add.
func TestScopedSteps_RunWithoutPlaceholderIsRefused(t *testing.T) {
	ws := t.TempDir()
	tc := config.TasksConfig{Test: "gotestsum ./..."}
	_, err := taskStepsOrRefusal(ws, tc, "go", "test", taskScope{run: "TestA"})
	if err == nil {
		t.Fatal("a run filter on a command with no {run} placeholder must be refused")
	}
	for _, want := range []string{"no {run} placeholder", "gotestsum ./...", "{run:-run}"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	// Other direction: the same command without a filter runs untouched.
	steps, err := taskStepsOrRefusal(ws, tc, "go", "test", taskScope{})
	if err != nil || strings.Join(steps[0], " ") != "gotestsum ./..." {
		t.Errorf("unfiltered run = %v, %v", steps, err)
	}
}

// TestScopedSteps_OlderDefaultsReconcile covers the configs written before #538:
// the previous shipped default (placeholder kept) and the one before that
// (placeholder written out) both reach the new placeholders, so a user whose
// config holds either is not refused a run filter.
func TestScopedSteps_OlderDefaultsReconcile(t *testing.T) {
	for _, stored := range []string{"go test {target:./...}", "go test ./..."} {
		tc := config.TasksConfig{Test: stored}
		steps, err := buildScopedTaskSteps(tc, "go", "test", taskScope{run: "TestA", verbose: true})
		if err != nil {
			t.Errorf("stored %q refused a run filter: %v", stored, err)
			continue
		}
		if got := strings.Join(steps[0], " "); got != "go test -v -run TestA ./..." {
			t.Errorf("stored %q built %q", stored, got)
		}
		notes := strings.Join(taskNotes(tc, "go", "test", taskScope{run: "TestA"}), "\n")
		if !strings.Contains(notes, stored) || !strings.Contains(notes, "placeholders") {
			t.Errorf("stored %q: running it through the shipped placeholders must be disclosed; notes: %s", stored, notes)
		}
	}
	// A command plumb never wrote is not reconciled.
	if _, err := buildScopedTaskSteps(config.TasksConfig{Test: "go test -race ./..."}, "go", "test", taskScope{run: "X"}); err == nil {
		t.Error("a command plumb never shipped must not gain a {run} placeholder")
	}
}

func TestTaskNotes_VerboseWithNowhereToGo(t *testing.T) {
	tc := config.TasksConfig{Test: "gotestsum ./..."}
	notes := strings.Join(taskNotes(tc, "go", "test", taskScope{verbose: true}), "\n")
	if !strings.Contains(notes, "verbose was NOT applied") {
		t.Errorf("verbose on a command without {verbose:<flag>} must be noted; notes: %q", notes)
	}
	withPlaceholder := config.TasksConfig{Test: config.DefaultTaskCommand("go", "test")}
	if n := taskNotes(withPlaceholder, "go", "test", taskScope{verbose: true}); len(n) != 0 {
		t.Errorf("verbose that landed needs no note: %v", n)
	}
}

func TestTaskNotes_CompositeSaysTheRunFilterWasDropped(t *testing.T) {
	tc := config.TasksConfig{Build: "go build ./...", Test: config.DefaultTaskCommand("go", "test")}
	notes := strings.Join(taskNotes(tc, "go", "verify", taskScope{run: "TestA"}), "\n")
	for _, want := range []string{`the run filter "TestA"`, "NOT applied", `slot "test"`} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes %q lack %q", notes, want)
		}
	}
}

// TestTaskResolver_EnvIsCarriedAndTrustGated is #537 through the resolver: the
// language's env reaches the command (sorted, unexpanded, with the root its
// {workspace} expands to), and a PROJECT env makes even the shipped default
// command project-supplied, so it is refused until `plumb trust` and runs after.
func TestTaskResolver_EnvIsCarriedAndTrustGated(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	for k, v := range map[string]string{"GOTMPDIR": "{workspace}/.testcache", "GOFLAGS": "-count=1"} {
		if err := config.SetProjectValue(ws, []string{"tasks", "go", "env", k}, v); err != nil {
			t.Fatal(err)
		}
	}
	tasks := map[string]config.TasksConfig{"go": {
		Build: config.DefaultTaskCommand("go", "build"),
		Env:   map[string]string{"GOTMPDIR": "{workspace}/.testcache", "GOFLAGS": "-count=1", "A_FIRST": "1"},
	}}
	s := newTaskTrustSession(t, ws, tasks)

	// GOFLAGS steers what runs, so the shipped default needs trust — and the
	// refusal must say which setting made it so.
	_, err := s.taskResolver(context.Background(), tools.TaskRequest{Slot: "build"})
	if err == nil || !strings.Contains(err.Error(), "not trusted") || !strings.Contains(err.Error(), "env sets GOFLAGS") {
		t.Fatalf("a project env must gate the shipped default command until trusted, naming the setting; got %v", err)
	}
	cmds, err := config.ProjectTaskCommands(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.NewTrustStore().SetTrustedForProject(ws, cmds, nil); err != nil {
		t.Fatal(err)
	}
	cmd, err := s.taskResolver(context.Background(), tools.TaskRequest{Slot: "build"})
	if err != nil {
		t.Fatalf("trusted: %v", err)
	}
	if strings.Join(cmd.Env, ",") != "A_FIRST=1,GOFLAGS=-count=1,GOTMPDIR={workspace}/.testcache" || cmd.Root != ws {
		t.Errorf("Env %v Root %q, want the sorted templates and the workspace root", cmd.Env, cmd.Root)
	}
	if cmd.Provenance != "project" {
		t.Errorf("provenance = %q, want project (the env is the project's)", cmd.Provenance)
	}
}

// TestTaskResolver_AScratchDirInsideTheWorkspaceNeedsNoTrust: a checked-in
// GOTMPDIR = "{workspace}/.testcache" only moves temporary files inside the
// workspace. Requiring `plumb trust` for it broke every shipped default in every
// fresh clone and worktree. It is carried without trust; anything that could
// leave the workspace, or a key that steers execution, is still gated.
func TestTaskResolver_AScratchDirInsideTheWorkspaceNeedsNoTrust(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	for value, exempt := range map[string]bool{
		"{workspace}/.testcache": true,
		"{workspace}":            true,
		"{workspace}/a/b":        true,
		"{workspace}/../outside": false,
		"/tmp/elsewhere":         false,
		"{working_dir}/.tmp":     false,
	} {
		ws := t.TempDir()
		if err := config.SetProjectValue(ws, []string{"tasks", "go", "env", "GOTMPDIR"}, value); err != nil {
			t.Fatal(err)
		}
		tasks := map[string]config.TasksConfig{"go": {Build: config.DefaultTaskCommand("go", "build"), Env: map[string]string{"GOTMPDIR": value}}}
		s := newTaskTrustSession(t, ws, tasks)
		cmd, err := s.taskResolver(context.Background(), tools.TaskRequest{Slot: "build"})
		switch {
		case exempt && err != nil:
			t.Errorf("GOTMPDIR=%s stays inside the workspace and must not need trust; got %v", value, err)
		case exempt && strings.Join(cmd.Env, ",") != "GOTMPDIR="+value:
			t.Errorf("GOTMPDIR=%s: the env must still be carried; got %v", value, cmd.Env)
		case !exempt && (err == nil || !strings.Contains(err.Error(), "not trusted")):
			t.Errorf("GOTMPDIR=%s may leave the workspace and must be trust-gated; got %v", value, err)
		}
	}
	// Any other key is gated even with a workspace value: HOME steers config lookup
	// (.gitconfig hooks), and TMPDIR reaches every tool of every language.
	for _, key := range []string{"HOME", "TMPDIR"} {
		ws := t.TempDir()
		if err := config.SetProjectValue(ws, []string{"tasks", "go", "env", key}, "{workspace}/.x"); err != nil {
			t.Fatal(err)
		}
		s := newTaskTrustSession(t, ws, map[string]config.TasksConfig{"go": {
			Build: config.DefaultTaskCommand("go", "build"),
			Env:   map[string]string{key: "{workspace}/.x"},
		}})
		if _, err := s.taskResolver(context.Background(), tools.TaskRequest{Slot: "build"}); err == nil {
			t.Errorf("%s is not GOTMPDIR; a project setting it must be trust-gated", key)
		}
	}
}

// TestTaskProvenance_NamesTheSameSettingEveryTime: the project specs come from map
// iteration, so a project with several gated settings once named a different one
// from run to run. The refusal must be stable, and an exempt GOTMPDIR next to them
// must not end the scan.
func TestTaskProvenance_NamesTheSameSettingEveryTime(t *testing.T) {
	ws := t.TempDir()
	// Both gated keys sort AFTER GOTMPDIR, so an exempt entry that ended the scan
	// would leave the command ungated.
	for k, v := range map[string]string{"GOTMPDIR": "{workspace}/.testcache", "ZED": "1", "MMM": "3"} {
		if err := config.SetProjectValue(ws, []string{"tasks", "go", "env", k}, v); err != nil {
			t.Fatal(err)
		}
	}
	for range 20 {
		_, fromProject, why := taskProvenance(ws, "go", "build")
		if !fromProject || !strings.Contains(why, "env sets MMM") {
			t.Fatalf("want the gated setting named deterministically (MMM, first in order); got fromProject=%v why=%q", fromProject, why)
		}
	}
}
