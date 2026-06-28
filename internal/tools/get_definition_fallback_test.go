package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// waitIndexed blocks until the topology index has resolved name (the initial
// resync runs in the background after Open), so the by-name fallback query is
// deterministic.
func waitIndexed(t *testing.T, store *topology.Store, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nodes, err := store.ResolveNodes(context.Background(), name, topology.NodeHint{})
		if err == nil && len(nodes) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("topology index never resolved %q", name)
}

// TestGetDefinition_TopologyFallbackByName proves that a by-name lookup degrades
// to the tree-sitter index when the language server errors (the still-warming
// case), returning the declaration site rather than surfacing the LSP error.
func TestGetDefinition_TopologyFallbackByName(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	waitIndexed(t, store, "Beta")
	tool := tools.NewGetDefinition(brokenLSP(), nil, 0, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"uri": uri, "symbol_name": "Beta"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected topology fallback to succeed, got: %v", err)
	}
	for _, want := range []string{"topology fallback", `Declaration of "Beta"`, "Beta", "demo.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("get_definition fallback missing %q:\n%s", want, out)
		}
	}
}

// TestGetDefinition_PositionPathNoFallback proves the raw-position path has no
// name to resolve against the index, so it surfaces the LSP error rather than
// falling back.
func TestGetDefinition_PositionPathNoFallback(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	tool := tools.NewGetDefinition(brokenLSP(), nil, 0, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"uri": uri, "line": 2, "character": 5})

	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("position path should surface the LSP error, not fall back to the index")
	}
}

// TestGetDefinition_NoFallbackOnAuthoritativeMiss proves a working server's
// "no symbol" answer (empty, no error) is never masked by a stale index hit.
func TestGetDefinition_NoFallbackOnAuthoritativeMiss(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	tool := tools.NewGetDefinition(&mockLSP{}, nil, 0, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"uri": uri, "symbol_name": "Beta"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "topology fallback") {
		t.Errorf("a working LSP's empty answer must not fall back to the index:\n%s", out)
	}
}
