package tools_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

var (
	neighbourCount = regexp.MustCompile(`neighbours \((\d+)\)`)
	inwardCount    = regexp.MustCompile(`depended on by \(inward\) \((\d+) nodes\)`)
)

// TestTopologyExplore_ClampsAnOverCapMaxNodesArgument proves the ceiling the
// topology_explore schema advertises is still enforced after it moved out of the
// traversal and into the tool.
//
// The traversal's own ceiling is now far higher, so nothing but this clamp keeps
// an agent's `max_nodes: 5000` from being honoured. The assertion is on the
// neighbour count the tool RETURNED, and the fixture graph is deliberately wider
// than the cap but well inside max_bytes, so a missing clamp shows up as more
// neighbours rather than as an unchanged answer.
func TestTopologyExplore_ClampsAnOverCapMaxNodesArgument(t *testing.T) {
	const callers = 300
	ceiling := topology.ClampToolNodes(math.MaxInt32)
	if callers <= ceiling {
		t.Fatalf("test is vacuous: the fixture graph (%d callers) must exceed the cap (%d)", callers, ceiling)
	}
	s := wideCallGraphStore(t, callers)

	tool := tools.NewTopologyExplore(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{
		"name":       "Centre",
		"depth":      1,
		"max_nodes":  math.MaxInt32,
		"max_bytes":  100000,
		"edge_kinds": []string{"calls"},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := parseNeighbours(t, out)
	if got != ceiling {
		t.Errorf("topology_explore returned %d neighbours for max_nodes=MaxInt32 over a "+
			"%d-node graph; the advertised ceiling of %d is not being applied", got, callers, ceiling)
	}
	if !strings.Contains(out, "truncated") {
		t.Error("a response cut at the advertised ceiling must say so; no truncation notice found")
	}
}

// TestTopologyExplore_HonoursAnUnderCapMaxNodesArgument is the other direction:
// the clamp is a ceiling, not a rewrite. A caller asking for fewer than the cap
// must get exactly that.
func TestTopologyExplore_HonoursAnUnderCapMaxNodesArgument(t *testing.T) {
	const callers = 300
	const asked = 12
	s := wideCallGraphStore(t, callers)

	tool := tools.NewTopologyExplore(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{
		"name":       "Centre",
		"depth":      1,
		"max_nodes":  asked,
		"max_bytes":  100000,
		"edge_kinds": []string{"calls"},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := parseNeighbours(t, out); got != asked {
		t.Errorf("topology_explore returned %d neighbours for max_nodes=%d", got, asked)
	}
}

// TestTopologyImpact_ClampsAnOverCapMaxNodesArgument is the same guard for the
// other tool that advertises the ceiling. Both apply it themselves now, so both
// need proving: a clamp restored on one call site and not the other is exactly
// the half-fix this change exists to undo.
func TestTopologyImpact_ClampsAnOverCapMaxNodesArgument(t *testing.T) {
	const callers = 300
	ceiling := topology.ClampToolNodes(math.MaxInt32)
	if callers <= ceiling {
		t.Fatalf("test is vacuous: the fixture graph (%d callers) must exceed the cap (%d)", callers, ceiling)
	}
	s := wideCallGraphStore(t, callers)

	tool := tools.NewTopologyImpact(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{
		"name":       "Centre",
		"depth":      1,
		"max_nodes":  math.MaxInt32,
		"max_bytes":  100000,
		"edge_kinds": []string{"calls"},
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got := parseInwardNodes(t, out)
	if got != ceiling {
		t.Errorf("topology_impact returned %d inward nodes for max_nodes=MaxInt32 over a "+
			"%d-node graph; the advertised ceiling of %d is not being applied", got, callers, ceiling)
	}
}

func parseInwardNodes(t *testing.T, out string) int {
	t.Helper()
	m := inwardCount.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no inward node count in response:\n%s", out)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("inward node count %q: %v", m[1], err)
	}
	return n
}

func parseNeighbours(t *testing.T, out string) int {
	t.Helper()
	m := neighbourCount.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no neighbour count in response:\n%s", out)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("neighbour count %q: %v", m[1], err)
	}
	return n
}

// wideCallGraphStore indexes a package where n functions all call Centre, so a
// one-hop traversal from Centre has n neighbours.
func wideCallGraphStore(t *testing.T, n int) *topology.Store {
	t.Helper()
	ws := t.TempDir()
	var src strings.Builder
	src.WriteString("package wide\n\nfunc Centre() {}\n")
	for i := range n {
		fmt.Fprintf(&src, "\nfunc Caller%d() { Centre() }\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "wide.go"), []byte(src.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 1 << 20}, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		nodes, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "wide.go"))
		if len(nodes) > n {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("index did not settle: fewer than %d nodes in wide.go", n)
	return nil
}

// TestTopologyExplore_ClampsAnOverCapMaxBytesArgument is the byte ceiling's
// counterpart to the node one, and it needs its own test for the same reason
// the node clamp did: the traversal's byte ceiling is now far above the number
// the schema advertises, so nothing but this clamp keeps an agent's
// `max_bytes: 1000000` from being honoured.
//
// The fixture uses fat nodes (long doc comments, include_source "full") because
// max_nodes is clamped to 200 first — under that ceiling only a large node makes
// the byte budget the binding one, which is exactly the case the clamp exists
// for.
func TestTopologyExplore_ClampsAnOverCapMaxBytesArgument(t *testing.T) {
	s := fatCallGraphStore(t, 300)
	ask := func(maxBytes int) int {
		t.Helper()
		tool := tools.NewTopologyExplore(func() *topology.Store { return s })
		args, _ := json.Marshal(map[string]any{
			"name": "Centre", "depth": 1, "max_nodes": math.MaxInt32,
			"max_bytes": maxBytes, "include_source": "full", "edge_kinds": []string{"calls"},
		})
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return parseNeighbours(t, out)
	}

	atCeiling := ask(topology.ClampToolBytes(math.MaxInt32))
	nodeCeiling := topology.ClampToolNodes(math.MaxInt32)
	if atCeiling >= nodeCeiling {
		t.Fatalf("test is vacuous: at the advertised byte ceiling the answer holds %d of the "+
			"%d nodes max_nodes allows, so max_bytes is not the binding bound", atCeiling, nodeCeiling)
	}
	if got := ask(math.MaxInt32); got != atCeiling {
		t.Errorf("max_bytes=MaxInt32 returned %d neighbours, the advertised ceiling returns %d; "+
			"the byte ceiling the schema advertises is not being applied", got, atCeiling)
	}
	// The other direction: the clamp is a ceiling, not a rewrite.
	if got := ask(topology.ClampToolBytes(math.MaxInt32) / 4); got >= atCeiling {
		t.Errorf("a quarter of the byte budget returned %d neighbours, not fewer than the %d "+
			"the full budget returns: an under-cap max_bytes is being ignored", got, atCeiling)
	}
}

// TestTopologyImpact_ClampsAnOverCapMaxBytesArgument mirrors it on the other
// tool that advertises the ceiling: a clamp applied at one call site and not the
// other is the half-fix this change exists to undo.
func TestTopologyImpact_ClampsAnOverCapMaxBytesArgument(t *testing.T) {
	s := fatCallGraphStore(t, 300)
	ask := func(maxBytes int) int {
		t.Helper()
		tool := tools.NewTopologyImpact(func() *topology.Store { return s })
		args, _ := json.Marshal(map[string]any{
			"name": "Centre", "depth": 1, "max_nodes": math.MaxInt32,
			"max_bytes": maxBytes, "edge_kinds": []string{"calls"},
		})
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return parseInwardNodes(t, out)
	}

	atCeiling := ask(topology.ClampToolBytes(math.MaxInt32))
	if atCeiling >= topology.ClampToolNodes(math.MaxInt32) {
		t.Fatalf("test is vacuous: max_bytes is not the binding bound at %d nodes", atCeiling)
	}
	if got := ask(math.MaxInt32); got != atCeiling {
		t.Errorf("max_bytes=MaxInt32 returned %d inward nodes, the advertised ceiling returns %d",
			got, atCeiling)
	}
}

// fatCallGraphStore is wideCallGraphStore with a long doc comment on every
// caller, so a node costs enough for the byte budget to bind before max_nodes.
func fatCallGraphStore(t *testing.T, n int) *topology.Store {
	t.Helper()
	ws := t.TempDir()
	doc := strings.Repeat("// padding padding padding padding padding padding padding\n", 12)
	// A long signature as well as a long docstring: topology_impact has no
	// include_source argument, so the docstring alone never reaches its estimate.
	params := make([]string, 0, 24)
	for p := range 24 {
		params = append(params, fmt.Sprintf("parameterNamedAtLength%02d string", p))
	}
	sig := strings.Join(params, ", ")
	var src strings.Builder
	src.WriteString("package fat\n\nfunc Centre() {}\n")
	for i := range n {
		fmt.Fprintf(&src, "\n%sfunc Caller%d(%s) (string, error) { Centre(); return \"\", nil }\n", doc, i, sig)
	}
	if err := os.WriteFile(filepath.Join(ws, "fat.go"), []byte(src.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 8 << 20}, []topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		nodes, _ := s.SymbolsInFile(context.Background(), filepath.Join(ws, "fat.go"))
		if len(nodes) > n {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("index did not settle: fewer than %d nodes in fat.go", n)
	return nil
}
