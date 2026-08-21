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

	// The scope the cli seam would hand the tool for this workspace.
	if !testSlotTakesTarget(tc) {
		t.Fatalf("the shipped go test command %q must accept a target, "+
			"or topology_affected cannot emit one", tc.Test)
	}
	scope := tools.TestScope{Language: "go", WorkingDir: tc.WorkingDir, ScopedTests: true}

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
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") || !strings.HasSuffix(line, "changed package") {
			continue
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			return fields[0]
		}
	}
	t.Fatalf("no changed-package row in topology_affected output:\n%s", out)
	return ""
}
