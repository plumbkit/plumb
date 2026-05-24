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
