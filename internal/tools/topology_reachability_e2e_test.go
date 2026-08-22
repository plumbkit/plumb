package tools_test

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

// buildReachabilityFixture indexes a small synthetic Go workspace through the
// real golang extractor + linkImports pass (the production path, not a
// hand-built DB), and waits for the initial index to settle:
//
//	cmd/entrypoint  (package main) --imports--> internal/x
//	internal/x                     --imports--> internal/y
//	internal/y                     --imports--> internal/x   (a real import cycle —
//	                                             syntactically valid, so the
//	                                             AST-only extractor indexes it
//	                                             even though `go build` would refuse it)
//	internal/deadcode              (no importer at all)
//
// This gives both directions the card asks adversarial fixtures to pin:
// internal/x and internal/y are genuinely reachable through a cycle (a missing
// edge would misreport them unreachable); internal/deadcode has no edge into
// it from anywhere (a false edge would misreport it reachable).
func buildReachabilityFixture(t *testing.T) *topology.Store {
	t.Helper()
	ws := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cmd/entrypoint/main.go", `package main

import "myapp/internal/x"

func main() { x.X() }
`)
	write("internal/x/x.go", `package x

import "myapp/internal/y"

func X() { y.Y() }
`)
	write("internal/y/y.go", `package y

import "myapp/internal/x"

func Y() {}
`)
	write("internal/deadcode/dead.go", `package deadcode

func Dead() {}
`)

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "internal/deadcode/dead.go"))
		if len(n) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return s
}

// TestReachabilityDefaultRootsSummary pins the default
// (reachable/unreachable) response shape end to end through the real
// extractor + linkImports path: main is found automatically, the cycle
// members are both reported reachable (RECALL — a missing edge here would be
// the false negative this feature exists to avoid), and the untouched
// package is reported unreachable (the FALSE-EDGE direction — nothing must
// mark it reachable just because it exists in the index).
func TestReachabilityDefaultRootsSummary(t *testing.T) {
	s := buildReachabilityFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"mode": "reachability"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "package-level (import edges); function-level unavailable") {
		t.Errorf("missing the required confidence line; got:\n%s", out)
	}
	if !strings.Contains(out, "cmd/entrypoint") {
		t.Errorf("expected cmd/entrypoint auto-resolved as a root; got:\n%s", out)
	}
	if !strings.Contains(out, "internal/x") || !strings.Contains(out, "internal/y") {
		t.Errorf("expected both cycle members reported reachable; got:\n%s", out)
	}
	if !strings.Contains(out, "internal/deadcode") {
		t.Errorf("expected internal/deadcode named as unreachable; got:\n%s", out)
	}
	if len(out) > 5*1024 {
		t.Errorf("response exceeds the 5 KB cap: %d bytes", len(out))
	}
}

// TestReachabilityPathToChain pins the path shape: a real chain
// through the cycle is reconstructed as package hops.
func TestReachabilityPathToChain(t *testing.T) {
	s := buildReachabilityFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{
		"mode":    "reachability",
		"roots":   []string{"cmd/entrypoint"},
		"path_to": "internal/y",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "cmd/entrypoint -> internal/x -> internal/y") {
		t.Errorf("expected the root->x->y chain, got:\n%s", out)
	}
}

// TestReachabilityPathToChainUnreachable pins that an unreachable
// target is reported as "no path", not a fabricated chain or an error.
func TestReachabilityPathToChainUnreachable(t *testing.T) {
	s := buildReachabilityFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{
		"mode":    "reachability",
		"roots":   []string{"cmd/entrypoint"},
		"path_to": "internal/deadcode",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "no path") {
		t.Errorf("expected a 'no path' answer for an unreachable target, got:\n%s", out)
	}
}

// TestReachabilityLayersFlagsCycle pins the layers shape: the
// real x<->y import cycle indexed by the production extractor must surface as
// one SCC flagged [cycle], not as two independent packages.
func TestReachabilityLayersFlagsCycle(t *testing.T) {
	s := buildReachabilityFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{
		"mode":   "reachability",
		"roots":  []string{"cmd/entrypoint"},
		"layers": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "[cycle]") {
		t.Errorf("expected the x<->y cycle flagged, got:\n%s", out)
	}
	// Both cycle members must be in the SAME flagged line (one SCC), not two
	// separate non-cyclic entries.
	found := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "internal/x") && strings.Contains(line, "internal/y") && strings.Contains(line, "[cycle]") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected internal/x and internal/y in one [cycle]-flagged component, got:\n%s", out)
	}
}

// TestReachabilityExplicitRoot pins that an explicit,
// exactly-indexed root directory resolves without relying on the "main"
// default.
func TestReachabilityExplicitRoot(t *testing.T) {
	s := buildReachabilityFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{
		"mode":  "reachability",
		"roots": []string{"internal/x"},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "internal/y") {
		t.Errorf("expected internal/y reachable from an explicit internal/x root, got:\n%s", out)
	}
	// cmd/entrypoint imports internal/x, not the reverse, so a traversal rooted
	// at internal/x must report it UNreachable — the false-edge direction:
	// nothing should mark an upstream importer as reachable from its own
	// dependency.
	if !strings.Contains(out, "unreachable") || !strings.Contains(out, "cmd/entrypoint") {
		t.Errorf("expected cmd/entrypoint reported unreachable from internal/x, got:\n%s", out)
	}
	reachableSection := out[:strings.Index(out, "unreachable:")]
	if strings.Contains(reachableSection, "cmd/entrypoint") {
		t.Errorf("cmd/entrypoint must not appear in the reachable bucket when rooted at its own dependency internal/x, got:\n%s", out)
	}
}
