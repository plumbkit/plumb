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

// TestSubstituteTarget_DefaultedPlaceholder is PLAN-326's contract, in both
// directions and in the same test, because either half alone is satisfiable by a
// wrong implementation: substituting always would pass the scoped case, and
// substituting never would pass the unscoped one.
func TestSubstituteTarget_DefaultedPlaceholder(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		target  string
		want    string
		wantErr bool
	}{
		{
			name: "no target uses the declared default",
			argv: []string{"go", "test", "{target:./...}"}, want: "go test ./...",
		},
		{
			name: "a target replaces the default",
			argv: []string{"go", "test", "{target:./...}"}, target: "./internal/tools",
			want: "go test ./internal/tools",
		},
		{
			// "Everything" for cargo/swift is the ABSENCE of the argument, so an
			// empty default has to drop the element rather than pass "".
			name: "an empty default omits the argument",
			argv: []string{"cargo", "test", "{target:}"}, want: "cargo test",
		},
		{
			name: "an empty default still accepts a target",
			argv: []string{"cargo", "test", "{target:}"}, target: "my_test",
			want: "cargo test my_test",
		},
		{
			// The strictness that must NOT have changed: a bare placeholder in a
			// command the user wrote still refuses to guess.
			name: "a bare placeholder with no target is still an error",
			argv: []string{"go", "test", "-run", "{target}"}, wantErr: true,
		},
		{
			name: "a bare placeholder with a target still substitutes",
			argv: []string{"go", "test", "-run", "{target}"}, target: "TestFoo",
			want: "go test -run TestFoo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := substituteTarget(tc.argv, tc.target)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %q", strings.Join(got, " "))
				}
				return
			}
			if err != nil {
				t.Fatalf("substituteTarget: %v", err)
			}
			if joined := strings.Join(got, " "); joined != tc.want {
				t.Errorf("substituteTarget = %q, want %q", joined, tc.want)
			}
		})
	}
}

// TestBuildTaskSteps_ShippedGoDefaultScopesBothWays runs the SHIPPED default
// through the real path, which is the claim PLAN-326 actually makes: not "the
// substituter can do this" but "a user who has configured nothing can scope a
// run, and still gets the whole suite when they ask for nothing".
func TestBuildTaskSteps_ShippedGoDefaultScopesBothWays(t *testing.T) {
	tc := config.Defaults().Tasks["go"]

	steps, err := buildTaskSteps(tc, "test", "")
	if err != nil {
		t.Fatalf("a bare run_task on the shipped default was refused: %v", err)
	}
	if got := strings.Join(steps[0], " "); got != "go test ./..." {
		t.Errorf("unscoped shipped default = %q, want the whole suite", got)
	}

	steps, err = buildTaskSteps(tc, "test", "./internal/tools")
	if err != nil {
		t.Fatalf("scoping the shipped default was refused — this is the defect PLAN-326 is about: %v", err)
	}
	if got := strings.Join(steps[0], " "); got != "go test ./internal/tools" {
		t.Errorf("scoped shipped default = %q", got)
	}
}

// TestTaskProvenance_ProjectWorkingDirGatesEverySlot closes the hole the
// working_dir feature would otherwise open in run_task's trust gate.
//
// The gate asks "did this project influence what is about to execute?" and used
// to answer it by looking for an overridden COMMAND. working_dir is not a
// command, so a project that overrides nothing still gets to say where the
// SHIPPED DEFAULT `go build ./...` runs — pointing it at a directory whose
// contents it also controls. Choosing the directory is influence; the argv is
// only half of what runs.
//
// The build slot is the one asserted precisely because this fixture does not
// override it. If the gate only fired for slots the project spelled out, this
// call would report "config" and the command would run ungated.
func TestTaskProvenance_ProjectWorkingDirGatesEverySlot(t *testing.T) {
	ws := t.TempDir()
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "working_dir"}, "sub"); err != nil {
		t.Fatal(err)
	}

	for _, slot := range []string{"build", "test", "lint", "e2e", "verify"} {
		label, fromProject := taskProvenance(ws, "go", slot)
		if !fromProject {
			t.Errorf("slot %q reported provenance %q / fromProject=false: this project sets no %s command, "+
				"but it does choose the directory the default one runs in, and that runs UNGATED", slot, label, slot)
		}
	}

	// The other direction, so the assertion above cannot be satisfied by a gate
	// that simply always fires: a language the project said nothing about is
	// unaffected.
	if _, fromProject := taskProvenance(ws, "python", "test"); fromProject {
		t.Error("a working_dir set for go marked python project-supplied too — the gate is not reading the language")
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
