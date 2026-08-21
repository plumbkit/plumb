package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// TestEmittedTestTargetIsAcceptedByRunTask is PLAN-378's acceptance test, and
// the reason the card exists: topology_affected told callers to run
// `go test ./plumb/internal/config/...`, while this repo's [tasks.go] sets
// working_dir = "plumb". The prescribed handoff — feed the path to run_task —
// therefore ran from plumb/ against a directory that does not exist there.
//
// It asserts the round trip rather than either half: the string the tool emits
// is fed to buildTaskSteps, the same function run_task uses to build its argv.
// Testing the two halves separately is what let them disagree.
//
// The command comes from config.Defaults() rather than a literal, so a change to
// the shipped `go test {target:./...}` is caught here instead of silently
// invalidating the assertion.
func TestEmittedTestTargetIsAcceptedByRunTask(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		p := filepath.Join(ws, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Mirrors this repo: the Go module is a subdirectory of the workspace.
	write("plumb/go.mod", "module example.com/m\n\ngo 1.22\n")
	write("plumb/internal/demo/demo.go", "package demo\n\nfunc Demo() int { return 1 }\n")
	write("plumb/internal/demo/demo_test.go",
		"package demo\n\nimport \"testing\"\n\nfunc TestDemo(t *testing.T) {}\n")

	tc := config.Defaults().Tasks["go"]
	tc.WorkingDir = "plumb"

	// The scope the cli seam would hand the tool for this workspace — built by
	// the real function, not hand-assembled, so the test exercises the decision
	// and not a restatement of it.
	style := testTargetStyle("go", tc)
	if style != tools.TargetGoPackage {
		t.Fatalf("the shipped go test command %q must resolve to a Go package "+
			"target, got style %v", tc.Test, style)
	}
	scope := tools.TestScope{Language: "go", WorkingDir: tc.WorkingDir, Style: style}

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "plumb/internal/demo/demo_test.go"))
		if len(n) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s }).
		WithTestScope(func() tools.TestScope { return scope })
	args, _ := json.Marshal(map[string]any{
		"files": []string{filepath.Join(ws, "plumb/internal/demo/demo.go")},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("topology_affected: %v", err)
	}

	target := changedPackageTarget(t, out)
	if target != "./internal/demo/..." {
		t.Errorf("emitted target = %q, want %q (relative to working_dir %q, not to the workspace root)",
			target, "./internal/demo/...", tc.WorkingDir)
	}

	// The actual acceptance criterion: run_task's own builder takes it.
	steps, err := buildTaskSteps(tc, "test", target)
	if err != nil {
		t.Fatalf("run_task refused the target topology_affected emitted (%q): %v", target, err)
	}
	if len(steps) != 1 {
		t.Fatalf("test slot built %d steps, want 1", len(steps))
	}
	got := strings.Join(steps[0], " ")
	if want := "go test " + target; got != want {
		t.Errorf("run_task argv = %q, want %q", got, want)
	}
}

// changedPackageTarget pulls the run target out of the row for the package the
// change landed in.
func changedPackageTarget(t *testing.T, out string) string {
	t.Helper()
	// Anchored on the column separator, not a bare suffix: "changed package" is
	// also a suffix of "imports the changed package", so HasSuffix alone picks up
	// an importer row whenever one happens to sort first.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") || !strings.HasSuffix(line, "   changed package") {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	t.Fatalf("no changed-package row in topology_affected output:\n%s", out)
	return ""
}

// TestTargetStyleMatchesShippedDefaults is the guard for a claim that was
// otherwise unbacked: that the languages topology_affected emits a target for
// are exactly the languages whose SHIPPED test command takes a positional path.
//
// internal/tools cannot import internal/config (Application may not reach down
// past Domain in that direction), so it cannot derive the rule; this package can
// see both and is where the two are tied together. Without this, adding a
// path-scoped default for a new language to config.defaultTasks would silently
// fail to reach topology_affected, and nothing would say so.
func TestTargetStyleMatchesShippedDefaults(t *testing.T) {
	// Every shipped language whose test command takes a POSITIONAL target must
	// appear here, including those deliberately given no target style. rust is
	// the instructive one: `cargo test {target:}` is positional, but the operand
	// is a test-NAME filter, so handing it a directory would silently select
	// nothing. Listing it as TargetNone records that as a decision rather than an
	// omission — the loop at the end of this test refuses to let a positional
	// language be merely absent.
	want := map[string]tools.TargetStyle{
		"go":     tools.TargetGoPackage,
		"python": tools.TargetPath,
		"rust":   tools.TargetNone,
	}
	defaults := config.Defaults().Tasks
	if len(defaults) == 0 {
		t.Fatal("config.Defaults() shipped no task commands")
	}
	for lang, tc := range defaults {
		got := testTargetStyle(lang, tc)
		if got != want[lang] {
			t.Errorf("language %q: shipped test command %q resolves to style %v, want %v. "+
				"If a default changed, update BOTH this table and testTargetStyle — a "+
				"language whose runner takes a positional path but which topology_affected "+
				"declines to emit a target for is a silently missing feature, and the "+
				"reverse emits a target that runs the wrong tests",
				lang, tc.Test, got, want[lang])
		}
	}
	for lang := range want {
		if _, ok := defaults[lang]; !ok {
			t.Errorf("language %q is classified here but ships no default task config "+
				"at all", lang)
		}
	}

	// The direction the comment above promises, which the table alone does NOT
	// give: a language ABSENT from `want` reads back as TargetNone, so a new
	// shipped default with a positional target would match silently and the
	// feature would simply never fire for it. Force a human to classify it.
	//
	// Found by an adversarial review, which added `npm test {target:}` and a
	// `ruby: rspec {target:}` default and watched this test stay green.
	for lang, tc := range defaults {
		if !testSlotTakesPositionalTarget(tc) {
			continue
		}
		if _, classified := want[lang]; !classified {
			t.Errorf("language %q ships a test command with a positional target (%q) but "+
				"is not classified in this table, so testTargetStyle silently returns "+
				"TargetNone and topology_affected emits no target for it. Decide whether "+
				"its operand is a PATH (add it here and to testTargetStyle) or a NAME "+
				"filter like cargo's (add it here as TargetNone, deliberately)",
				lang, tc.Test)
		}
	}
}

// TestTargetStyleRejectsNonPositionalPlaceholders is the regression for the
// hole an adversarial review found: the acceptance probe proves only that a
// {target} SLOT exists, never that the slot means a directory.
//
// `go test -run {target}` accepts a target and then treats it as a test-NAME
// regex: the emitted package path matches nothing, no package operand is given
// so only the current directory is considered, and the run exits 0. A silent
// green over zero tests is worse than the hardcoded `go test ./x/...` this
// feature replaced, because the old string was at least a correct command.
func TestTargetStyleRejectsNonPositionalPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		lang string
		cmd  string
		want tools.TargetStyle
	}{
		{"shipped go default", "go", "go test {target:./...}", tools.TargetGoPackage},
		{"shipped python default", "python", "pytest {target:}", tools.TargetPath},
		{"go with extra flags before a trailing operand", "go", "go test -count=1 {target:./...}", tools.TargetGoPackage},

		// Boolean flags do not consume the target. Reading them as if they did
		// silently killed the feature for the most ordinary customisation there
		// is — adding -race or -v to the shipped default.
		{"go -race is boolean", "go", "go test -race {target:./...}", tools.TargetGoPackage},
		{"go -v is boolean", "go", "go test -v {target:./...}", tools.TargetGoPackage},
		{"pytest -q is boolean", "python", "pytest -q {target:}", tools.TargetPath},
		{"double dash marks what follows as positional", "go", "gotestsum -- {target:./...}", tools.TargetGoPackage},
		// An UNKNOWN flag still counts as consuming: withholding a target costs an
		// edit, emitting a wrong one costs a green run that tested nothing.
		{"unknown flag is assumed to take a value", "go", "go test -mystery {target:./...}", tools.TargetNone},

		{"go -run takes a NAME regex", "go", "go test -run {target}", tools.TargetNone},
		{"pytest -k takes a NAME expression", "python", "pytest -k {target}", tools.TargetNone},
		{"placeholder is not the last operand", "go", "go test {target:./...} -v", tools.TargetNone},
		{"no placeholder at all", "go", "go test ./...", tools.TargetNone},
		{"unset test command", "go", "", tools.TargetNone},
		{"whitespace-only test command", "go", "   ", tools.TargetNone},
		{"rust is positional but scopes by NAME", "rust", "cargo test {target:}", tools.TargetNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := testTargetStyle(tc.lang, config.TasksConfig{Test: tc.cmd})
			if got != tc.want {
				t.Errorf("testTargetStyle(%q, %q) = %v, want %v", tc.lang, tc.cmd, got, tc.want)
			}
		})
	}
}

// TestConnSessionTestScope covers the production seam itself.
//
// An adversarial review found the feature could be deleted at the wiring layer
// with the entire internal/cli suite still green: every other test in this file
// hand-builds a tools.TestScope, so making (*connSession).testScope() return the
// zero value — no target ever emitted, for any workspace — broke nothing. The
// round trip has to start where production starts.
func TestConnSessionTestScope(t *testing.T) {
	goTasks := config.Defaults().Tasks["go"]
	subdirGo := goTasks
	subdirGo.WorkingDir = "plumb"

	cases := []struct {
		name  string
		lang  string
		tasks map[string]config.TasksConfig
		want  tools.TestScope
	}{
		{
			name:  "go with the shipped default",
			lang:  "go",
			tasks: map[string]config.TasksConfig{"go": goTasks},
			want:  tools.TestScope{Language: "go", Style: tools.TargetGoPackage},
		},
		{
			// The PLAN-378 rebasing bug, asserted at the seam: dropping WorkingDir
			// here is what made the emitted target name a directory that does not
			// exist from where run_task runs.
			name:  "go with a working_dir must carry it through",
			lang:  "go",
			tasks: map[string]config.TasksConfig{"go": subdirGo},
			want:  tools.TestScope{Language: "go", WorkingDir: "plumb", Style: tools.TargetGoPackage},
		},
		{
			name:  "python resolves to a plain path",
			lang:  "python",
			tasks: map[string]config.TasksConfig{"python": config.Defaults().Tasks["python"]},
			want:  tools.TestScope{Language: "python", Style: tools.TargetPath},
		},
		{
			name:  "rust is positional but scopes by name",
			lang:  "rust",
			tasks: map[string]config.TasksConfig{"rust": config.Defaults().Tasks["rust"]},
			want:  tools.TestScope{Language: "rust", Style: tools.TargetNone},
		},
		{
			name:  "no language attached yields the zero value",
			lang:  "",
			tasks: map[string]config.TasksConfig{"go": goTasks},
			want:  tools.TestScope{},
		},
		{
			name:  "LanguageNone yields the zero value",
			lang:  LanguageNone,
			tasks: map[string]config.TasksConfig{"go": goTasks},
			want:  tools.TestScope{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &connSession{}
			s.mutate(func(v *sessionView) {
				v.acquiredLanguage = tc.lang
				v.tasks = tc.tasks
			})
			if got := s.testScope(); got != tc.want {
				t.Errorf("testScope() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
