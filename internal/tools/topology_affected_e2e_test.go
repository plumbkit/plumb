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
