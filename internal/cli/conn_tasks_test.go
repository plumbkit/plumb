package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

func TestBuildTaskSteps_VerifyIsBuildThenTest(t *testing.T) {
	tc := config.TasksConfig{Build: "go build ./...", Test: "go test ./..."}
	steps, err := buildTaskSteps(tc, "verify", "")
	if err != nil {
		t.Fatalf("buildTaskSteps: %v", err)
	}
	if len(steps) != 2 || steps[0][0] != "go" || steps[0][1] != "build" || steps[1][1] != "test" {
		t.Errorf("verify steps = %v, want build then test", steps)
	}
}

func TestBuildTaskSteps_TargetSubstitution(t *testing.T) {
	tc := config.TasksConfig{Test: "go test -run {target} ./..."}
	steps, err := buildTaskSteps(tc, "test", "TestFoo")
	if err != nil {
		t.Fatalf("buildTaskSteps: %v", err)
	}
	if got := strings.Join(steps[0], " "); got != "go test -run TestFoo ./..." {
		t.Errorf("target substitution = %q", got)
	}
}

func TestBuildTaskSteps_TargetWithoutPlaceholder(t *testing.T) {
	tc := config.TasksConfig{Test: "go test ./..."}
	if _, err := buildTaskSteps(tc, "test", "TestFoo"); err == nil {
		t.Error("expected an error: a target was given but the command has no {target}")
	}
}

func TestBuildTaskSteps_EmptySlot(t *testing.T) {
	steps, err := buildTaskSteps(config.TasksConfig{}, "lint", "")
	if err != nil {
		t.Fatalf("buildTaskSteps: %v", err)
	}
	if len(steps) != 0 {
		t.Errorf("an unset slot should yield no steps, got %v", steps)
	}
}

func TestTaskProvenance_ProjectOverride(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "test"}, "go test ./..."); err != nil {
		t.Fatal(err)
	}
	if _, fromProject := taskProvenance(ws, "go", "test"); !fromProject {
		t.Error("a project-overridden slot should report fromProject=true")
	}
	if _, fromProject := taskProvenance(ws, "go", "build"); fromProject {
		t.Error("a non-overridden slot should report fromProject=false (global/default)")
	}
}
