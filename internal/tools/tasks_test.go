package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
		return TaskCommand{}, fmt.Errorf("untrusted: run `plumb trust`")
	})
	_, err := runTask(t, tool, `{"slot":"build"}`)
	if err == nil || !strings.Contains(err.Error(), "plumb trust") {
		t.Errorf("expected the resolver's trust error, got %v", err)
	}
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
