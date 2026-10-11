package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
)

// conn_tasks_override_test.go is #634's wiring half. The rule itself is
// config.ValidateTaskEnvOverrides, and the tools package tests run it directly;
// what only this package can answer is whether the run_task the cli REGISTERS
// carries it. An unwired session fails closed — Tasks.Execute refuses every
// override — which would look like the feature simply not existing.

// newTaskOverrideSession is a session whose only task command is the shipped go
// test default, plus a fast one for calls that are meant to run.
func newTaskOverrideSession(t *testing.T, ws, test string) *connSession {
	t.Helper()
	return newTaskLanguageSession(t, ws, "go", map[string]config.TasksConfig{"go": {Test: test}})
}

// TestRegisteredRunTask_CarriesTheEnvRule runs the tool the way registerAllTools
// builds it, and checks both halves of the wiring: a steering name is refused
// through the registered tool, and an ordinary override is accepted — so a
// missing TaskEnvRefusal cannot pass as "the rule works" by refusing everything.
func TestRegisteredRunTask_CarriesTheEnvRule(t *testing.T) {
	s := newTaskOverrideSession(t, t.TempDir(), "true")
	if s.buildWriteDeps().TaskEnvRefusal == nil {
		t.Fatal("buildWriteDeps wired no per-call env validator, so run_task would refuse every env/unset argument")
	}
	task := tools.NewTasks(s.buildWriteDeps(), s.taskResolver)

	_, err := task.Execute(context.Background(),
		json.RawMessage(`{"slot":"test","unset":["GOFLAGS"]}`))
	if err == nil || !strings.Contains(err.Error(), "changes what the stored command would run") {
		t.Errorf("removing a configured steering name = %v, want the refusal the registered tool must carry", err)
	}
	if _, err := task.Execute(context.Background(),
		json.RawMessage(`{"slot":"test","env":{"CORPUS":"/tmp/corpus"}}`)); err != nil {
		t.Errorf("an ordinary per-call env must be accepted: %v", err)
	}
}

// TestTaskResolver_AcceptsAPlainEnvOverride is the whole point of #634: a test
// that reads its corpus path or mode from the environment runs through the stored
// command instead of a native shell — and the stored command itself is unchanged,
// so run_task's no-agent-command-line promise still holds.
func TestTaskResolver_AcceptsAPlainEnvOverride(t *testing.T) {
	s := newTaskOverrideSession(t, t.TempDir(), "go test ./...")
	cmd, err := s.taskResolver(context.Background(), tools.TaskRequest{
		Slot:  "test",
		Env:   map[string]string{"CORPUS": "/tmp/corpus", "MODE": "experiment"},
		Unset: []string{"GOTMPDIR"},
	})
	if err != nil {
		t.Fatalf("a plain per-call env must be accepted: %v", err)
	}
	if len(cmd.Steps) == 0 {
		t.Fatal("the command must still resolve")
	}
	if got := strings.Join(cmd.Steps[0], " "); got != "go test ./..." {
		t.Errorf("argv = %q, want the stored command untouched by the override", got)
	}
}
