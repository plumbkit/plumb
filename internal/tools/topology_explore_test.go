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

const exploreFixtureGo = `package demo

// Greeter greets people.
type Greeter struct {}

// Greet prints a greeting.
func (g *Greeter) Greet() string {
	return "hello"
}

// Farewell prints goodbye.
func (g *Greeter) Farewell() string {
	return "bye"
}
`

func openExploreFixture(t *testing.T) (*tools.TopologyExplore, *topology.Store) {
	t.Helper()
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "greeter.go"), []byte(exploreFixtureGo), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		nodes, _ := store.ResolveNodes(context.Background(), "Greeter", topology.NodeHint{})
		if len(nodes) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	tool := tools.NewTopologyExplore(func() *topology.Store { return store })
	return tool, store
}

// TestTopologyExplore_ReceiverSelectorNormalisation verifies that method queries
// using dotted, pointer-decorated, or slashed forms resolve the same declaration.
func TestTopologyExplore_ReceiverSelectorNormalisation(t *testing.T) {
	tool, _ := openExploreFixture(t)

	queries := []string{
		"Greeter.Greet",
		"(*Greeter).Greet",
		"(Greeter).Greet",
		"*Greeter.Greet",
		"Greeter/Greet",
	}

	for _, q := range queries {
		raw, _ := json.Marshal(map[string]any{"name": q})
		out, err := tool.Execute(context.Background(), raw)
		if err != nil {
			t.Fatalf("Execute(%q) error: %v", q, err)
		}
		if !strings.Contains(out, `topology explore: method "Greet"`) {
			t.Errorf("Execute(%q) did not resolve Greet method:\n%s", q, out)
		}
	}
}

// TestTopologyExplore_TypeQueryShowsMembers verifies that exploring a type
// node outputs an explicit members section with actionable method selectors.
func TestTopologyExplore_TypeQueryShowsMembers(t *testing.T) {
	tool, _ := openExploreFixture(t)

	raw, _ := json.Marshal(map[string]any{"name": "Greeter"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute(Greeter): %v", err)
	}

	if !strings.Contains(out, "members (2):") {
		t.Errorf("expected members (2) section in type output:\n%s", out)
	}
	if !strings.Contains(out, "method (*Greeter).Greet") || !strings.Contains(out, "method (*Greeter).Farewell") {
		t.Errorf("expected Greeter methods in members section:\n%s", out)
	}
}

// TestTopologyExplore_SourceModes verifies that docstrings mode (and snippets/full aliases)
// include doc comments while signatures mode only includes signature.
func TestTopologyExplore_SourceModes(t *testing.T) {
	tool, _ := openExploreFixture(t)

	// docstrings mode
	raw, _ := json.Marshal(map[string]any{"name": "Greeter.Greet", "include_source": "docstrings"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("Execute docstrings: %v", err)
	}
	if !strings.Contains(out, "doc:  Greet prints a greeting.") {
		t.Errorf("expected docstring line in docstrings mode:\n%s", out)
	}

	// full mode alias
	rawFull, _ := json.Marshal(map[string]any{"name": "Greeter.Greet", "include_source": "full"})
	outFull, err := tool.Execute(context.Background(), rawFull)
	if err != nil {
		t.Fatalf("Execute full: %v", err)
	}
	if !strings.Contains(outFull, "doc:  Greet prints a greeting.") {
		t.Errorf("expected docstring line in full alias mode:\n%s", outFull)
	}

	// signatures mode (default)
	rawSig, _ := json.Marshal(map[string]any{"name": "Greeter.Greet", "include_source": "signatures"})
	outSig, err := tool.Execute(context.Background(), rawSig)
	if err != nil {
		t.Fatalf("Execute signatures: %v", err)
	}
	if strings.Contains(outSig, "doc:") {
		t.Errorf("signatures mode should not include doc line:\n%s", outSig)
	}
}

// TestTopologyExplore_UnmatchedHintFailsHonestly verifies that a path/kind hint
// that excludes all candidates produces an honest error with retry guidance rather
// than silently selecting an unrelated file.
func TestTopologyExplore_UnmatchedHintFailsHonestly(t *testing.T) {
	tool, _ := openExploreFixture(t)

	raw, _ := json.Marshal(map[string]any{"name": "Greet", "path": "nonexistent.go"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error when path hint excludes all candidates, got nil")
	}
	if !strings.Contains(err.Error(), "not found matching hint") || !strings.Contains(err.Error(), "Retry with") {
		t.Errorf("unexpected error format: %v", err)
	}
}
