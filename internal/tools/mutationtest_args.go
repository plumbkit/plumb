package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// mutationtest_args.go is mutation_test's input half: the argument shape, its
// defaults, and the validation that runs before anything is resolved or touched.
// Split from mutationtest.go, which keeps the tool surface and the run.

type mutantSpec struct {
	Path  string `json:"file_path"`
	Old   string `json:"old_string"`
	New   string `json:"new_string"`
	Label string `json:"label"`
}

type mutationTestArgs struct {
	Mutants     []mutantSpec `json:"mutants"`
	TestTask    string       `json:"test_task"`
	TestTarget  string       `json:"test_target"`
	TestRun     string       `json:"test_run"`
	CompileTask string       `json:"compile_task"`
	TimeoutSecs int          `json:"timeout_seconds"`
}

// withDefaults returns a copy with the unset slots filled. A value receiver
// keeps every method on mutationTestArgs consistent (recvcheck), and the
// defaults stay stated in one readable place.
func (a mutationTestArgs) withDefaults() mutationTestArgs {
	if a.TestTask == "" {
		a.TestTask = "test"
	}
	if a.CompileTask == "" {
		a.CompileTask = "build"
	}
	if a.TimeoutSecs == 0 {
		a.TimeoutSecs = int(defaultTaskTimeout / time.Second)
	}
	return a
}

func (a mutationTestArgs) validate() error {
	if len(a.Mutants) == 0 {
		return errors.New("mutation_test: mutants is required (at least one {file_path, old_string, new_string})")
	}
	if len(a.Mutants) > maxMutants {
		return fmt.Errorf("mutation_test: %d mutants exceeds the limit of %d — each costs a full compile+test cycle", len(a.Mutants), maxMutants)
	}
	if !taskSlotName.MatchString(a.TestTask) {
		return fmt.Errorf("mutation_test: test_task %q is not a valid slot name; the built-ins are build, lint, test, e2e, verify", a.TestTask)
	}
	if !taskSlotName.MatchString(a.CompileTask) {
		return fmt.Errorf("mutation_test: compile_task %q is not a valid slot name; the built-ins are build, lint, test, e2e, verify", a.CompileTask)
	}
	if a.TestTarget != "" && !targetPattern.MatchString(a.TestTarget) {
		return fmt.Errorf("mutation_test: test_target %q is not a single shell-safe argument ([A-Za-z0-9._/:@-])", a.TestTarget)
	}
	if err := validateRunFilter("mutation_test: test_run", a.TestRun); err != nil {
		return err
	}
	if a.TimeoutSecs < 0 || a.TimeoutSecs > maxMutationStepSeconds {
		return fmt.Errorf("mutation_test: timeout_seconds must be between 1 and %d; got %d", maxMutationStepSeconds, a.TimeoutSecs)
	}
	return validateMutantSpecs(a.Mutants)
}

func validateMutantSpecs(specs []mutantSpec) error {
	for i, m := range specs {
		switch {
		case strings.TrimSpace(m.Path) == "":
			return fmt.Errorf("mutation_test: mutant %d: file_path is required", i+1)
		case m.Old == "":
			return fmt.Errorf("mutation_test: mutant %d: old_string is required (the exact text to mutate)", i+1)
		case m.Old == m.New:
			return fmt.Errorf("mutation_test: mutant %d: new_string equals old_string — that mutates nothing and would report a meaningless survival", i+1)
		}
	}
	return nil
}

func parseMutationTestArgs(raw json.RawMessage) (mutationTestArgs, error) {
	var a mutationTestArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("mutation_test: invalid arguments: %w", err)
	}
	return a.withDefaults(), nil
}
