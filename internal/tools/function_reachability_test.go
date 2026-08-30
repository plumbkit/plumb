package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

func TestTopologyImpact_GranularityValidationAndPackageDefault(t *testing.T) {
	pkg, err := parseTopologyImpactArgs(json.RawMessage(`{"name":"foo"}`))
	if err != nil {
		t.Fatalf("parse package-default args: %v", err)
	}
	if pkg.Granularity != "package" {
		t.Fatalf("omitted granularity = %q, want package", pkg.Granularity)
	}
	if err := pkg.validate(); err != nil {
		t.Fatalf("package-default blast radius should remain valid: %v", err)
	}

	functionMode, err := parseTopologyImpactArgs(json.RawMessage(`{"mode":"reachability"}`))
	if err != nil {
		t.Fatalf("parse function-mode default args: %v", err)
	}
	if functionMode.Granularity != "package" {
		t.Fatalf("reachability granularity default = %q, want package", functionMode.Granularity)
	}
	if err := functionMode.validate(); err != nil {
		t.Fatalf("package reachability default should remain valid: %v", err)
	}
	description := (&TopologyImpact{}).Description()
	for _, want := range []string{`granularity="function"`, "durable derived cross-file edges", "full reachable closure"} {
		if !strings.Contains(description, want) {
			t.Errorf("topology_impact description missing %q: %s", want, description)
		}
	}

	for _, raw := range []string{
		`{"name":"foo","granularity":"function"}`,
		`{"mode":"reachability","granularity":"class"}`,
	} {
		args, err := parseTopologyImpactArgs(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if err := args.validate(); err == nil {
			t.Errorf("%s: expected granularity validation error", raw)
		}
	}
}

func TestTopologyImpact_FunctionReachabilityProductionOnlyBounded(t *testing.T) {
	ws := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		path := filepath.Join(ws, rel)
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("main.go", `package main

func main() { entry() }
func entry() { worker() }
func worker() {}
`)
	write("main_test.go", `package main

func TestOnly() { entry() }
`)

	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tool := NewTopologyImpact(func() *topology.Store { return store })
	args := json.RawMessage(`{"mode":"reachability","granularity":"function","roots":["main"]}`)
	var out string
	var lastErr error
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, lastErr = tool.Execute(context.Background(), args)
		if lastErr == nil && strings.Contains(out, "main.go#entry") && strings.Contains(out, "main.go#worker") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("function reachability: %v", lastErr)
	}
	if !strings.Contains(out, "function-level") || !strings.Contains(out, "reachable: 3 callable(s)") {
		t.Fatalf("expected non-vacuous function summary, got:\n%s", out)
	}
	if strings.Contains(out, "main_test.go") || strings.Contains(out, "TestOnly") {
		t.Fatalf("test-originated caller leaked into production reachability:\n%s", out)
	}
	if len(out) > reachabilityMaxBytes {
		t.Fatalf("function reachability exceeded byte cap: %d > %d", len(out), reachabilityMaxBytes)
	}
}

func TestFunctionReachabilityScopeNoteSurfacesAcrossShapes(t *testing.T) {
	g := &topology.FunctionGraph{
		Nodes: map[int64]topology.Node{
			1: {ID: 1, Kind: topology.KindFunction, Name: "main", Path: "main.go", StartLine: 1},
			2: {ID: 2, Kind: topology.KindFunction, Name: "worker", Path: "main.go", StartLine: 2},
		},
		Edges:    map[int64][]int64{1: {2}},
		MainDirs: map[string]bool{".": true},
	}
	res := &topology.FunctionReachabilityResult{
		Roots:     []int64{1},
		Reachable: map[int64]bool{1: true, 2: true},
	}
	status := topology.CallGraphStatus{CallSites: 1, Resolved: 1}
	scopeNote := "Scope: function-level call edges cover the go subgraph only — 1 package, 1 file. 2 files in python are out of scope for this analysis, not unreachable and not caller-free."
	shapes := []string{
		formatFunctionSummary(g, res, nil, "", status, scopeNote),
		formatFunctionPath(g, res, "main.go#worker", nil, "", status, scopeNote),
		formatFunctionLayers(g, res, nil, "", status, scopeNote),
	}
	for i, shape := range shapes {
		if !strings.Contains(shape, "call graph scope: "+scopeNote) {
			t.Errorf("shape %d omitted admission scope note:\n%s", i, shape)
		}
	}
}
