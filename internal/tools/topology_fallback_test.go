package tools_test

import (
	"context"
	"encoding/json"
	"errors"
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

// newIndexedStore builds a topology store over a temp workspace containing one
// Go file and waits for the background indexer to index it. Uses only pure-Go
// dependencies (modernc sqlite + go/ast), so it needs no external binary.
func newIndexedStore(t *testing.T) (store *topology.Store, uri string) {
	t.Helper()
	ws := t.TempDir()
	src := "package demo\n\nfunc HandleRequest() {}\n\ntype RequestHandler struct{}\n"
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

func brokenLSP() *mockLSP { return &mockLSP{err: errors.New("lsp unavailable")} }

func TestWorkspaceSymbols_TopologyFallback(t *testing.T) {
	store, _ := newIndexedStore(t)
	tool := tools.NewWorkspaceSymbols(brokenLSP(), nil, 0, 0, nil).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"query": "HandleRequest"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected topology fallback to succeed, got error: %v", err)
	}
	if !strings.Contains(out, "topology fallback") || !strings.Contains(out, "HandleRequest") {
		t.Errorf("expected annotated topology result naming HandleRequest, got:\n%s", out)
	}
}

func TestListSymbols_TopologyFallback(t *testing.T) {
	store, uri := newIndexedStore(t)
	tool := tools.NewListSymbols(brokenLSP(), nil, 0, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"uri": uri})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected topology fallback to succeed, got error: %v", err)
	}
	if !strings.Contains(out, "topology fallback") || !strings.Contains(out, "HandleRequest") {
		t.Errorf("expected annotated outline naming HandleRequest, got:\n%s", out)
	}
}

func TestFindSymbol_TopologyFallback(t *testing.T) {
	store, uri := newIndexedStore(t)
	tool := tools.NewFindSymbol(brokenLSP(), nil, 0, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"query": "Handle", "uri": uri})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected topology fallback to succeed, got error: %v", err)
	}
	if !strings.Contains(out, "topology fallback") || !strings.Contains(out, "HandleRequest") {
		t.Errorf("expected annotated matches naming HandleRequest, got:\n%s", out)
	}
}

// Without a topology fallback wired, the LSP error must surface unchanged.
func TestWorkspaceSymbols_NoFallbackWhenTopologyNil(t *testing.T) {
	tool := tools.NewWorkspaceSymbols(brokenLSP(), nil, 0, 0, nil)
	args, _ := json.Marshal(map[string]any{"query": "X"})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected the LSP error to surface when no topology fallback is wired")
	}
}
