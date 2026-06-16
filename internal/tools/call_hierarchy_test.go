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
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	goext "github.com/plumbkit/plumb/internal/topology/extractors/golang"
)

// newCallGraphStore builds a topology store over a temp Go file containing a
// three-level call chain (Top → Mid → Bottom) so the index holds "calls" edges
// in both directions around Mid. Pure-Go deps only (modernc sqlite + go/ast).
func newCallGraphStore(t *testing.T) (store *topology.Store, uri string) {
	t.Helper()
	ws := t.TempDir()
	src := "package demo\n\n" +
		"func Top() { Mid() }\n\n" +
		"func Mid() { Bottom() }\n\n" +
		"func Bottom() {}\n"
	path := filepath.Join(ws, "demo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	uri = "file://" + path
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if nodes, _ := s.SymbolsInFile(context.Background(), uri); len(nodes) > 0 {
			return s, uri
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("topology did not index demo.go within 5s")
	return nil, ""
}

// TestCallHierarchy_TopologyFallback covers Bug C: when the language server
// returns no call-hierarchy item (zls has no prepareCallHierarchy), the tool
// reconstructs the hierarchy. With no LSP references available the callers fall
// back to the topology call graph. Mid is called by Top (incoming) and calls
// Bottom (outgoing).
func TestCallHierarchy_TopologyFallback(t *testing.T) {
	store, uri := newCallGraphStore(t)
	// emptyLSP: PrepareCallHierarchy and References return (nil, nil).
	tool := tools.NewCallHierarchy(&mockLSP{}, 0).
		WithTopologyFallback(func() *topology.Store { return store })

	args, _ := json.Marshal(map[string]any{"uri": uri, "line": 4, "character": 5, "direction": "both"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("fallback should succeed, got error: %v", err)
	}
	if !strings.Contains(out, "reconstructed") {
		t.Errorf("expected reconstructed annotation, got:\n%s", out)
	}
	if !strings.Contains(out, "Top") {
		t.Errorf("expected caller Top (topology) in incoming section, got:\n%s", out)
	}
	if !strings.Contains(out, "Bottom") {
		t.Errorf("expected callee Bottom in outgoing section, got:\n%s", out)
	}
}

// TestCallHierarchy_FallbackCallersFromReferences covers the hybrid fallback:
// when the server has no call hierarchy but DOES answer find_references (zls),
// callers are reconstructed from the references mapped to their enclosing
// symbol — catching callers (e.g. a Zig test block) the topology graph misses.
func TestCallHierarchy_FallbackCallersFromReferences(t *testing.T) {
	store, uri := newCallGraphStore(t)
	// References returns one call site at line 10; the enclosing document symbol
	// is the test block "TestBlock" (lines 5–20). No prepareCallHierarchy.
	m := &mockLSP{
		locations: []protocol.Location{{
			URI:   uri,
			Range: protocol.Range{Start: protocol.Position{Line: 10}},
		}},
		docSymbols: []protocol.DocumentSymbol{{
			Name: "TestBlock", Kind: protocol.SKMethod,
			Range:          protocol.Range{Start: protocol.Position{Line: 5}, End: protocol.Position{Line: 20}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: 5}},
		}},
	}
	tool := tools.NewCallHierarchy(m, 0).
		WithTopologyFallback(func() *topology.Store { return store })

	args, _ := json.Marshal(map[string]any{"uri": uri, "line": 4, "character": 5, "direction": "both"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("fallback should succeed, got error: %v", err)
	}
	if !strings.Contains(out, "TestBlock") {
		t.Errorf("expected caller TestBlock from LSP references, got:\n%s", out)
	}
	if strings.Contains(out, "Top") {
		t.Errorf("callers should come from LSP references, not topology (Top), got:\n%s", out)
	}
	if !strings.Contains(out, "Bottom") {
		t.Errorf("expected callee Bottom from topology, got:\n%s", out)
	}
}

// TestCallHierarchy_TopologyFallbackDisabled confirms that with no topology
// store wired the tool keeps the original "no item" message rather than erroring.
func TestCallHierarchy_TopologyFallbackDisabled(t *testing.T) {
	tool := tools.NewCallHierarchy(&mockLSP{}, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///x.go", "line": 1, "character": 0})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No call hierarchy item found") {
		t.Errorf("expected the original empty-result message, got:\n%s", out)
	}
}

// TestCallHierarchy_DedupsOutgoing covers the Bug A cosmetic nit: sourcekit-lsp
// reports a repeatedly-used property getter once per call site, so the rendered
// callee list duplicated it. The tool now dedups by (name, uri, line).
func TestCallHierarchy_DedupsOutgoing(t *testing.T) {
	caller := protocol.CallHierarchyItem{
		Name: "show", Kind: protocol.SKMethod, URI: "file:///x.swift",
		Range: protocol.Range{Start: protocol.Position{Line: 10}},
	}
	dup := protocol.CallHierarchyOutgoingCall{To: protocol.CallHierarchyItem{
		Name: "getter:panel", Kind: protocol.SKMethod, URI: "file:///x.swift",
		Range: protocol.Range{Start: protocol.Position{Line: 30}},
	}}
	m := &mockLSP{
		chItems:    []protocol.CallHierarchyItem{caller},
		chOutgoing: []protocol.CallHierarchyOutgoingCall{dup, dup, dup},
	}
	tool := tools.NewCallHierarchy(m, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///x.swift", "line": 10, "character": 0, "direction": "outgoing"})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n := strings.Count(out, "getter:panel"); n != 1 {
		t.Errorf("expected getter:panel rendered once after dedup, got %d:\n%s", n, out)
	}
}
