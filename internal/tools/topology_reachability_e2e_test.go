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

	waitForReachabilityGraph(t, s)
	return s
}

// waitForReachabilityGraph polls until the directory-level EDGES this test
// suite depends on actually exist, not merely until the source files' own
// NODES are indexed. This distinction matters: linkImports (the pass that
// creates the cross-file/cross-directory "imports" edges loadPackageEdges
// folds) runs once per drain, AFTER the whole file batch — see
// internal/topology/indexer.go's drain loop, which calls idx.linkImports()
// only after every queued file in the batch has been dispatched. A poll that
// only waits for internal/deadcode's NODE to appear can observe every file's
// nodes indexed while the edge-linking pass has not run yet (or is
// mid-run on a loaded/CI runner); reachability then legitimately reports "no
// root packages resolved" because cmd/entrypoint has no outward edge yet —
// not a bug in the tool, but a race in what this fixture waited for. Waiting
// for the actual edges this suite exercises (cmd/entrypoint -> internal/x,
// and the internal/x <-> internal/y cycle) closes that race.
func waitForReachabilityGraph(t *testing.T, s *topology.Store) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		g, err := s.PackageGraph(context.Background())
		if err == nil &&
			g.Edges["cmd/entrypoint"]["internal/x"] &&
			g.Edges["internal/x"]["internal/y"] &&
			g.Edges["internal/y"]["internal/x"] {
			if _, ok := g.Dirs["internal/deadcode"]; ok {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the reachability fixture's directory edges to be indexed (linkImports never settled)")
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
	if !strings.Contains(out, "function-level unavailable") {
		t.Errorf("missing the required confidence line; got:\n%s", out)
	}
	if !strings.Contains(out, "production imports only") || !strings.Contains(out, "_test.go") {
		t.Errorf("missing the disclosure that _test.go importers are excluded from the graph; got:\n%s", out)
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
	unreachableIdx := strings.Index(out, "unreachable:")
	if unreachableIdx == -1 {
		t.Fatalf("expected an \"unreachable:\" section in the response (cmd/entrypoint should land there), got:\n%s", out)
	}
	if !strings.Contains(out, "cmd/entrypoint") {
		t.Errorf("expected cmd/entrypoint reported unreachable from internal/x, got:\n%s", out)
	}
	reachableSection := out[:unreachableIdx]
	if strings.Contains(reachableSection, "cmd/entrypoint") {
		t.Errorf("cmd/entrypoint must not appear in the reachable bucket when rooted at its own dependency internal/x, got:\n%s", out)
	}
}

// buildCandidateRootFixture indexes a workspace with NO `package main` at
// all, so the default root set can only come from topology_routes
// candidates — isolating the candidateDirs labelling path (independent
// review's "ALSO REQUIRED" item and finding 5, which found the label was
// silently dropped from the path_to/layers shapes because they passed nil
// instead of the resolved candidateDirs through to the header).
//
//	internal/handlers  (func HandleFunc(), a topology_routes candidate; no imports)
//	internal/util       --imports--> internal/helper   (an unrelated production edge,
//	                                  needed so TotalEdges() > 0 and the
//	                                  Go-only refusal does not fire)
func buildCandidateRootFixture(t *testing.T) *topology.Store {
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
	write("internal/handlers/handlers.go", `package handlers

func HandleFunc() {}
`)
	write("internal/util/util.go", `package util

import "myapp/internal/helper"

func U() { helper.H() }
`)
	write("internal/helper/helper.go", `package helper

func H() {}
`)

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		g, gerr := s.PackageGraph(context.Background())
		if gerr == nil && g.Edges["internal/util"]["internal/helper"] {
			if _, ok := g.Dirs["internal/handlers"]; ok {
				return s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the candidate-root fixture to index")
	return nil
}

// TestReachabilityDefaultRootsLabelsRouteCandidate pins that a route
// candidate resolved as a default root is actually labelled
// "(candidate-seeded)" in the response — the assignment this label depends
// on (candidateDirs[resolved]=true in resolveReachabilityRoots) is otherwise
// a guard the test suite did not previously prove could fail.
func TestReachabilityDefaultRootsLabelsRouteCandidate(t *testing.T) {
	s := buildCandidateRootFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"mode": "reachability"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "internal/handlers (candidate-seeded)") {
		t.Errorf("expected internal/handlers labelled candidate-seeded in the default summary, got:\n%s", out)
	}
}

// TestReachabilityPathToLabelsRouteCandidate and
// TestReachabilityLayersLabelsRouteCandidate pin finding 5: the
// candidate-seeded label must survive into the path_to and layers shapes'
// headers too, not just the default summary.
func TestReachabilityPathToLabelsRouteCandidate(t *testing.T) {
	s := buildCandidateRootFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"mode": "reachability", "path_to": "internal/util"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "internal/handlers (candidate-seeded)") {
		t.Errorf("expected the path_to shape's header to label internal/handlers candidate-seeded, got:\n%s", out)
	}
}

func TestReachabilityLayersLabelsRouteCandidate(t *testing.T) {
	s := buildCandidateRootFixture(t)
	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"mode": "reachability", "layers": true})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "internal/handlers (candidate-seeded)") {
		t.Errorf("expected the layers shape's header to label internal/handlers candidate-seeded, got:\n%s", out)
	}
}

// fakeNoImportEdgeExtractor emits a KindPackage node per file and nothing
// else — no KindImport nodes, no edges. It stands in for a real language
// extractor (C#, PHP, Scala, Elixir, …) that indexes package declarations but
// does not emit the KindPackage -(imports)-> KindImport edge shape
// loadPackageEdges depends on; only extractors/golang does that today.
type fakeNoImportEdgeExtractor struct{}

func (fakeNoImportEdgeExtractor) Language() string     { return "fakelang" }
func (fakeNoImportEdgeExtractor) Extensions() []string { return []string{".fakelang"} }
func (fakeNoImportEdgeExtractor) Extract(_ context.Context, relPath string, _ []byte) ([]topology.Node, []topology.Edge, error) {
	return []topology.Node{{Kind: topology.KindPackage, Name: "pkg", Language: "fakelang", Path: relPath}}, nil, nil
}

// TestReachabilityGoOnlyRefusal pins independent review finding (BLOCKING 4):
// a language whose extractor indexes package declarations but never emits
// the import-edge shape this feature depends on must get an explicit
// refusal, not a confident "every package is unreachable" answer.
func TestReachabilityGoOnlyRefusal(t *testing.T) {
	ws := t.TempDir()
	write := func(rel string) {
		t.Helper()
		full := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("stub"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("fakea/a.fakelang")
	write("fakeb/b.fakelang")

	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{fakeNoImportEdgeExtractor{}})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(10 * time.Second)
	var g *topology.PackageGraph
	for time.Now().Before(deadline) {
		gg, gerr := s.PackageGraph(context.Background())
		if gerr == nil && len(gg.Dirs) >= 2 {
			g = gg
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if g == nil {
		t.Fatal("timed out waiting for both fake-language directories to index")
	}
	if g.TotalEdges() != 0 {
		t.Fatalf("test setup bug: fakeNoImportEdgeExtractor produced edges: %v", g.Edges)
	}

	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"mode": "reachability", "roots": []string{"fakea"}})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "Go-only") {
		t.Errorf("expected a clear Go-only refusal, got:\n%s", out)
	}
	if strings.Contains(out, "unreachable:") {
		t.Errorf("must refuse, not confidently report every package unreachable, got:\n%s", out)
	}
}
