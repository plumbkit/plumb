package cli

import (
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestConfiguredSlots_AgreesWithBuildTaskSteps is the regression test for two
// disagreements an independent review found between the session_start report and
// the code that actually runs a slot. A report that contradicts the tool it
// describes is worse than no report, so this asserts agreement directly rather
// than re-stating either predicate.
func TestConfiguredSlots_AgreesWithBuildTaskSteps(t *testing.T) {
	cases := []struct {
		name string
		tc   config.TasksConfig
	}{
		{"fully configured", config.TasksConfig{Build: "b", Lint: "l", Test: "t", E2E: "e"}},
		{"build only", config.TasksConfig{Build: "b"}},
		{"test only", config.TasksConfig{Test: "t"}},
		{"nothing configured", config.TasksConfig{}},
		{"whitespace-only command", config.TasksConfig{Test: "   "}},
		{"whitespace build with real test", config.TasksConfig{Build: "  ", Test: "t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reported := map[string]bool{}
			for _, s := range configuredSlots(tc.tc) {
				reported[s] = true
			}
			for _, slot := range []string{"build", "lint", "test", "e2e", "verify"} {
				steps, err := buildTaskSteps(tc.tc, slot, "")
				runnable := err == nil && len(steps) > 0
				if reported[slot] != runnable {
					t.Errorf("slot %q: session_start reports configured=%v but run_task runnable=%v",
						slot, reported[slot], runnable)
				}
			}
		})
	}
}

// TestConfiguredSlots_BuildOnlyStillRunsVerify pins the specific case the old
// predicate got wrong: verify's branch runs whichever of build/test IS set, so a
// build-only config must NOT be told verify is unconfigured.
func TestConfiguredSlots_BuildOnlyStillRunsVerify(t *testing.T) {
	tc := config.TasksConfig{Build: "echo build"}
	steps, err := buildTaskSteps(tc, "verify", "")
	if err != nil || len(steps) == 0 {
		t.Skip("verify no longer runs on a build-only config; the disagreement is moot")
	}
	got := strings.Join(configuredSlots(tc), ",")
	if !strings.Contains(got, "verify") {
		t.Errorf("verify runs for a build-only config but was reported unconfigured (got %q)", got)
	}
}

// TestConfiguredSlots_WhitespaceCommandIsNotConfigured pins the other direction:
// ParseTaskCommand trims, so a whitespace-only command is unset and the call is
// refused — reporting it as configured would send the agent into that refusal.
func TestConfiguredSlots_WhitespaceCommandIsNotConfigured(t *testing.T) {
	got := strings.Join(configuredSlots(config.TasksConfig{Test: "   "}), ",")
	if strings.Contains(got, "test") {
		t.Errorf("a whitespace-only command must not be reported as configured, got %q", got)
	}
}
