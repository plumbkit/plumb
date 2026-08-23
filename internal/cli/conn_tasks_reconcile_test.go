package cli

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// expandShippedDefault returns the shipped default for (lang, slot) written the
// way a user writes it when they drop the placeholder: with {target:<D>}
// replaced by D, or with the element removed when D is empty.
//
// Derived from config.Defaults() rather than spelled as a literal, so a change
// to a shipped command is caught by these tests instead of silently
// invalidating them.
func expandShippedDefault(t *testing.T, lang, slot string) string {
	t.Helper()
	argv, err := config.ParseTaskCommand(config.DefaultTaskCommand(lang, slot))
	if err != nil || argv == nil {
		t.Fatalf("no shipped %s command for %s", slot, lang)
	}
	out := make([]string, 0, len(argv))
	found := false
	for _, a := range argv {
		def, ok := targetPlaceholder(a)
		if !ok {
			out = append(out, a)
			continue
		}
		found = true
		if def != nil && *def != "" {
			out = append(out, *def)
		}
	}
	if !found {
		t.Fatalf("the shipped %s command for %s carries no placeholder to expand: %q",
			slot, lang, config.DefaultTaskCommand(lang, slot))
	}
	return strings.Join(out, " ")
}

// TestReconcile_ShippedDefaultSpelledOutScopesAgain is the card's acceptance
// criterion, and the defect it is about.
//
// A config that sets `[tasks.go] test = "go test ./..."` — the command plumb
// itself shipped before the placeholder existed — made every scoped run_task
// call fail permanently, which is the single largest non-policy run_task failure
// family in 90 days of telemetry (13/41) and every one of them slot:"test" with
// a Go package path. The advertised topology_affected -> run_task(target:)
// handoff is exactly what was breaking.
//
// Both directions are asserted in the same test on purpose: an implementation
// that reconciled EVERYTHING would pass the accept half while quietly rewriting
// commands plumb never wrote, and one that reconciled NOTHING would pass the
// refuse half while leaving the defect in place.
func TestReconcile_ShippedDefaultSpelledOutScopesAgain(t *testing.T) {
	accept := []struct {
		name, lang, slot, stored, target, want string
	}{
		{
			name: "the go test default with its placeholder spelled out",
			lang: "go", slot: "test", stored: expandShippedDefault(t, "go", "test"),
			target: "./internal/cli", want: "go test ./internal/cli",
		},
		{
			// An empty default spells "everything" as the ABSENCE of the operand, so
			// the expanded form is one element SHORTER than the shipped command.
			name: "the python test default with the operand omitted",
			lang: "python", slot: "test", stored: expandShippedDefault(t, "python", "test"),
			target: "tests/test_x.py", want: "pytest tests/test_x.py",
		},
		{
			name: "the rust test default with the operand omitted",
			lang: "rust", slot: "test", stored: expandShippedDefault(t, "rust", "test"),
			target: "my_test", want: "cargo test my_test",
		},
		{
			// Language matching follows go-toml's case-insensitive table binding: a
			// config written [TASKS.Go] reaches the runner, so it must reach this.
			name: "language matched case-insensitively",
			lang: "GO", slot: "test", stored: expandShippedDefault(t, "go", "test"),
			target: "./internal/tools", want: "go test ./internal/tools",
		},
	}
	for _, tc := range accept {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			steps, err := buildTaskSteps(config.TasksConfig{Test: tc.stored}, tc.lang, tc.slot, tc.target)
			if err != nil {
				t.Fatalf("a target was refused for stored command %q: %v", tc.stored, err)
			}
			if got := strings.Join(steps[0], " "); got != tc.want {
				t.Errorf("argv = %q, want %q", got, tc.want)
			}
		})
	}

	refuse := []struct {
		name, lang, slot string
		tc               config.TasksConfig
	}{
		{
			// The other direction that matters most: passing a target to a linter is
			// meaningless, and reconciliation must not have made it legal.
			name: "the shipped go lint command takes no target",
			lang: "go", slot: "lint", tc: config.TasksConfig{Lint: config.DefaultTaskCommand("go", "lint")},
		},
		{
			name: "the shipped go build command takes no target",
			lang: "go", slot: "build", tc: config.TasksConfig{Build: config.DefaultTaskCommand("go", "build")},
		},
		{
			// Not an expansion of any shipped default, so there is nothing to prove
			// an equivalence against and rewriting it would be a guess.
			name: "a placeholder-less command plumb never shipped",
			lang: "go", slot: "test", tc: config.TasksConfig{Test: "go test -count=1 ./..."},
		},
		{
			name: "a different runner entirely",
			lang: "go", slot: "test", tc: config.TasksConfig{Test: "gotestsum ./..."},
		},
		{
			name: "a language with no shipped test placeholder",
			lang: "typescript", slot: "test", tc: config.TasksConfig{Test: config.DefaultTaskCommand("typescript", "test")},
		},
		{
			name: "an extra slot the project named itself",
			lang: "go", slot: "audit", tc: config.TasksConfig{Extra: map[string]string{"audit": "govulncheck ./..."}},
		},
	}
	for _, tc := range refuse {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			steps, err := buildTaskSteps(tc.tc, tc.lang, tc.slot, "./internal/cli")
			if err == nil {
				t.Fatalf("a target was ACCEPTED for %q, building %v — reconciliation must "+
					"only fire for a command that is a shipped default with its placeholder "+
					"written out", tc.tc.Get(tc.slot), steps)
			}
			if !strings.Contains(err.Error(), "{target}") {
				t.Errorf("refusal should name the missing placeholder, got: %v", err)
			}
		})
	}
}

// TestReconcile_NoTargetArgvIsUnchanged is the safety invariant the whole design
// rests on, asserted as a RELATIONSHIP rather than against literal argvs: for
// every shipped default with a placeholder, the expanded spelling and the
// placeholder spelling must build the SAME argv when no target is given.
//
// If that ever stops holding, reconciliation is no longer meaning-preserving and
// is silently changing what a user's stored command runs.
func TestReconcile_NoTargetArgvIsUnchanged(t *testing.T) {
	checked := 0
	for lang, def := range config.Defaults().Tasks {
		for _, slot := range config.ConfiguredSlotNames(def) {
			if !shippedTakesTarget(config.DefaultTaskCommand(lang, slot)) {
				continue
			}
			checked++
			shipped, err := buildTaskSteps(def, lang, slot, "")
			if err != nil {
				t.Fatalf("%s/%s: shipped default refused a bare run: %v", lang, slot, err)
			}
			if slot != "test" {
				t.Fatalf("%s/%s ships a {target} placeholder outside the test slot; extend "+
					"this fixture to set that slot rather than leaving it unchecked", lang, slot)
			}
			expanded := config.TasksConfig{Test: expandShippedDefault(t, lang, slot)}
			stored, err := buildTaskSteps(expanded, lang, slot, "")
			if err != nil {
				t.Fatalf("%s/%s: expanded spelling refused a bare run: %v", lang, slot, err)
			}
			if !slices.Equal(shipped[0], stored[0]) {
				t.Errorf("%s/%s: reconciliation changed an UNSCOPED run: stored %q now builds %v, "+
					"but the shipped default builds %v", lang, slot, expanded.Test, stored[0], shipped[0])
			}
		}
	}
	if checked == 0 {
		t.Fatal("no shipped default carries a {target} placeholder — this invariant checked nothing")
	}
}

// TestTargetPlaceholderRefusal_NamesStoredCommandAndConfigPath pins the remedy
// text of the refusal that survives reconciliation.
//
// The message it replaces was the bare sentence "a target was given but the
// command has no {target} placeholder": it named neither the command that was
// stored nor the file holding it, so a caller could not tell a slot that cannot
// take a target from one merely written without the placeholder, and the same
// call failed the same way forever.
//
// The config path is asserted as an ABSOLUTE path built from a t.TempDir the
// test controls, and the target string is deliberately absent from every
// assertion — so no assertion here can be satisfied by an echo of the input.
func TestTargetPlaceholderRefusal_NamesStoredCommandAndConfigPath(t *testing.T) {
	ws := t.TempDir()
	const stored = "golangci-lint run --timeout=5m"
	if err := config.SetProjectValue(ws, []string{"tasks", "go", "lint"}, stored); err != nil {
		t.Fatal(err)
	}
	tc := config.TasksConfig{Lint: stored}

	msg := targetPlaceholderRefusal(ws, tc, "go", "lint").Error()
	wantPath := config.ProjectConfigPath(ws)
	if wantPath == "" || !filepath.IsAbs(wantPath) {
		t.Fatalf("ProjectConfigPath gave nothing to assert on: %q", wantPath)
	}
	for _, want := range []string{stored, wantPath, "[tasks.go] lint", "without a target"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal must name %q; got: %s", want, msg)
		}
	}
	// The echo guard is NOT here: targetPlaceholderRefusal takes no target, so it
	// is structurally incapable of echoing one and an assertion to that effect
	// could never fail. It lives in
	// TestTaskResolver_TargetRefusalCrossesTheResolverSeam, the layer that does
	// have the caller's target in hand.

	// A slot plumb DOES ship a placeholder for points at that exact spelling
	// instead, because restoring it is the whole remedy. Same build, opposite
	// branch — a message that hardcoded either half would fail one of these.
	shipped := config.DefaultTaskCommand("go", "test")
	expanded := expandShippedDefault(t, "go", "test")
	testMsg := targetPlaceholderRefusal(ws, config.TasksConfig{Test: expanded}, "go", "test").Error()
	if !strings.Contains(testMsg, shipped) {
		t.Errorf("a slot with a shipped placeholder must have that placeholder quoted (%q); got: %s",
			shipped, testMsg)
	}

	// Provenance: a command the project does NOT supply must not be blamed on the
	// project's config file, or the remedy sends the caller to edit the wrong one.
	globalMsg := targetPlaceholderRefusal(ws, config.TasksConfig{Test: "gotestsum ./..."}, "go", "test").Error()
	if strings.Contains(globalMsg, wantPath) {
		t.Errorf("a command absent from the project config was attributed to %s: %s", wantPath, globalMsg)
	}
	if !strings.Contains(globalMsg, config.GlobalConfigPath()) {
		t.Errorf("a non-project, non-default command should point at the global config (%s); got: %s",
			config.GlobalConfigPath(), globalMsg)
	}
}
