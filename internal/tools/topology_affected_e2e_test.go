package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/config"
	"github.com/golimpio/plumb/internal/tools"
	"github.com/golimpio/plumb/internal/topology"
	goext "github.com/golimpio/plumb/internal/topology/extractors/golang"
)

// TestTopologyAffected_ColocatedTests proves the recall booster: a sibling test
// that does NOT call the changed symbol (so no dependency edge connects them) is
// still flagged because it lives in the same directory.
func TestTopologyAffected_ColocatedTests(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo.go", "package demo\n\nfunc HandleRequest() {}\n")
	// Deliberately does not call HandleRequest — only co-location can find it.
	write("demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) {}\n")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n1, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo.go"))
		n2, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo_test.go"))
		if len(n1) > 0 && len(n2) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"symbols": []string{"HandleRequest"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TestUnrelated") {
		t.Errorf("co-located test TestUnrelated should be flagged; got:\n%s", out)
	}
	if !strings.Contains(out, "co-located") {
		t.Errorf("output should label the co-located reason; got:\n%s", out)
	}
}

// TestTopologyAffected_FileRootSeedsColocation proves the files: input path:
// a changed file resolved by its exact path (SymbolsInFile, not an FTS5
// path-string search) seeds its directory, so co-located sibling tests surface
// even though no dependency edge connects them.
func TestTopologyAffected_FileRootSeedsColocation(t *testing.T) {
	ws := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("demo.go", "package demo\n\nfunc HandleRequest() {}\n")
	write("demo_test.go", "package demo\n\nimport \"testing\"\n\nfunc TestUnrelated(t *testing.T) {}\n")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n1, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo.go"))
		n2, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "demo_test.go"))
		if len(n1) > 0 && len(n2) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyAffected(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"files": []string{"demo.go"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "TestUnrelated") {
		t.Errorf("files input should surface co-located test TestUnrelated; got:\n%s", out)
	}
	if !strings.Contains(out, "co-located") {
		t.Errorf("output should label the co-located reason; got:\n%s", out)
	}
}

// TestTopologyAffected_TestsInDirs unit-checks the store query that backs the
// co-location booster: only tests whose immediate directory matches are returned.
func TestTopologyAffected_TestsInDirs(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"a_test.go":     "package a\n\nimport \"testing\"\n\nfunc TestTop(t *testing.T) {}\n",
		"sub/b_test.go": "package b\n\nimport \"testing\"\n\nfunc TestSub(t *testing.T) {}\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "sub/b_test.go")); len(n) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Top-level directory is "." — only TestTop should match, not the subdir test.
	got, err := s.TestsInDirs(context.Background(), []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, n := range got {
		names[n.Name] = true
	}
	if !names["TestTop"] {
		t.Errorf("TestsInDirs(\".\") should include TestTop; got %v", names)
	}
	if names["TestSub"] {
		t.Errorf("TestsInDirs(\".\") must not include the subdir TestSub; got %v", names)
	}
}

// TestSessionStart_TopologyLedGuidance verifies that when the topology index is
// active, the Claude Code guidance leads with topology (the Map) and names
// topology_affected as the headline post-change tool.
func TestSessionStart_TopologyLedGuidance(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module demo\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "demo.go"), []byte("package demo\n\nfunc Run() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tool := tools.NewSessionStart(
		func() string { return ws }, nil, nil,
		func() bool { return false },
		func() string { return "claude-code" },
		nil,
	).WithTopology(func() *topology.Store { return s })

	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Topology (the Map)", "topology_affected", "which tests to run"} {
		if !strings.Contains(out, want) {
			t.Errorf("topology-led guidance missing %q:\n%s", want, out)
		}
	}
}

// TestSessionStart_LSPLedGuidanceWhenTopologyOff verifies the fallback when no
// topology store is wired: the LSP-led list plus an enable-topology tip.
func TestSessionStart_LSPLedGuidanceWhenTopologyOff(t *testing.T) {
	ws := t.TempDir()
	tool := tools.NewSessionStart(
		func() string { return ws }, nil, nil,
		func() bool { return false },
		func() string { return "claude-code" },
		nil,
	)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "workspace_symbols") {
		t.Errorf("LSP-led guidance should mention workspace_symbols:\n%s", out)
	}
	if !strings.Contains(out, "[topology] enabled = true") {
		t.Errorf("LSP-led guidance should tip enabling topology:\n%s", out)
	}
}

// editLaneWarningSubstrings are the load-bearing phrases the Claude Code
// edit-lane warning must carry: it must name the anti-pattern, the exact harness
// error an agent will have already seen, and the correct plumb tool.
var editLaneWarningSubstrings = []string{
	"Edit lane",
	"File has not been read yet",
	"File has been modified since read",
	"edit_file",
	"native",
}

// TestSessionStart_EditLaneWarning_ClaudeCode proves the warning is present for
// a Claude Code client in BOTH the topology-led and LSP-led guidance branches
// (it is written before the branch, so it must survive either path). This is
// the structural guard for the harness/plumb read-state mismatch fix.
func TestSessionStart_EditLaneWarning_ClaudeCode(t *testing.T) {
	newTool := func(ws string, topoOn bool) *tools.SessionStart {
		tool := tools.NewSessionStart(
			func() string { return ws }, nil, nil,
			func() bool { return false },
			func() string { return "claude-code" },
			nil,
		)
		if topoOn {
			s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
				[]topology.Extractor{goext.New()})
			if err != nil {
				t.Fatalf("topology.Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			tool = tool.WithTopology(func() *topology.Store { return s })
		}
		return tool
	}

	for _, topoOn := range []bool{false, true} {
		name := "topology-off"
		if topoOn {
			name = "topology-on"
		}
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			if err := os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module demo\n\ngo 1.22\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := newTool(ws, topoOn).Execute(context.Background(), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range editLaneWarningSubstrings {
				if !strings.Contains(out, want) {
					t.Errorf("edit-lane warning missing %q (topoOn=%v):\n%s", want, topoOn, out)
				}
			}
		})
	}
}

// TestSessionStart_EditLaneWarning_AbsentForDesktop proves the warning does NOT
// fire for Claude Desktop: Desktop has no native Edit tool, so the warning would
// be wrong (and the Desktop guidance already says all file ops go through plumb).
func TestSessionStart_EditLaneWarning_AbsentForDesktop(t *testing.T) {
	ws := t.TempDir()
	tool := tools.NewSessionStart(
		func() string { return ws }, nil, nil,
		func() bool { return false },
		func() string { return "claude-ai" },
		nil,
	)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "## Tool guidance (Claude Desktop)") {
		t.Fatalf("expected Desktop guidance:\n%s", out)
	}
	if strings.Contains(out, "File has not been read yet") {
		t.Errorf("Desktop guidance must NOT carry the native-Edit warning:\n%s", out)
	}
}
