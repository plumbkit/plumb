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
	ts "github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// indexedStore opens a topology store over one source file and waits for the
// background indexer to index it, so a Search reaches a populated FTS index.
func indexedStore(t *testing.T, filename, src string, ext topology.Extractor) (*topology.Store, string) {
	t.Helper()
	ws := t.TempDir()
	path := filepath.Join(ws, filename)
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{ext})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	uri := "file://" + path
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if nodes, _ := s.SymbolsInFile(context.Background(), uri); len(nodes) > 0 {
			return s, uri
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("topology did not index %s within 5s", filename)
	return nil, ""
}

// TestWorkspaceSymbols_TreeSitterFillOnEmptyLSP proves Zig-support Item 1: a
// language server that answers with no symbols and no error (the lazy-server
// case — zls only knows files it has already analysed) is supplemented from the
// topology Map for a tree-sitter-backed language, instead of "No symbols found".
func TestWorkspaceSymbols_TreeSitterFillOnEmptyLSP(t *testing.T) {
	store, _ := indexedStore(t, "demo.py", "def handle_request():\n    pass\n", ts.NewPython())
	// mockLSP{} answers empty with no error.
	tool := tools.NewWorkspaceSymbols(&mockLSP{}, nil, 0, 0, nil).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"query": "handle_request"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected the topology fill to succeed, got: %v", err)
	}
	if !strings.Contains(out, "topology fill") || !strings.Contains(out, "handle_request") {
		t.Errorf("expected an annotated fill naming handle_request, got:\n%s", out)
	}
}

// TestWorkspaceSymbols_NoFillForNativeAST proves the fill is gated to
// tree-sitter languages: an empty-but-no-error answer for a Go workspace (gopls
// indexes eagerly, so empty is authoritative) is NOT supplanted by approximate
// index matches.
func TestWorkspaceSymbols_NoFillForNativeAST(t *testing.T) {
	store, _ := indexedStore(t, "demo.go", "package demo\n\nfunc HandleRequest() {}\n", goext.New())
	tool := tools.NewWorkspaceSymbols(&mockLSP{}, nil, 0, 0, nil).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"query": "HandleRequest"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if strings.Contains(out, "topology fill") {
		t.Errorf("Go (native-AST) empty result must not be filled from the index, got:\n%s", out)
	}
	if !strings.Contains(out, "No symbols found") {
		t.Errorf("expected the authoritative empty answer to surface, got:\n%s", out)
	}
}
