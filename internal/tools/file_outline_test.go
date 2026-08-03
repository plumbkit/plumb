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
	tsext "github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// outlineSource is a Go file whose symbols and line numbers the LSP-path test
// asserts against. Line numbers (1-based) are noted in the test.
const outlineSource = "package demo\n" + // 1
	"\n" + // 2
	"// Server is the HTTP server.\n" + // 3
	"type Server struct {\n" + // 4
	"\taddr string\n" + // 5
	"}\n" + // 6
	"\n" + // 7
	"// Handle processes a request and\n" + // 8
	"// returns a response.\n" + // 9
	"func (s *Server) Handle(ctx Context,\n" + // 10
	"\tr Request) (Response, error) {\n" + // 11
	"\treturn Response{}, nil\n" + // 12
	"}\n" // 13

func writeOutlineFile(t *testing.T) (path, uri string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "demo.go")
	if err := os.WriteFile(path, []byte(outlineSource), 0o644); err != nil {
		t.Fatal(err)
	}
	return path, "file://" + path
}

// serverDocSymbols mirrors what gopls would return for outlineSource (0-based
// LSP line numbers).
func serverDocSymbols() []protocol.DocumentSymbol {
	rng := func(s, e uint32) protocol.Range {
		return protocol.Range{Start: protocol.Position{Line: s}, End: protocol.Position{Line: e}}
	}
	return []protocol.DocumentSymbol{
		{
			Name:  "Server",
			Kind:  protocol.SKStruct,
			Range: rng(3, 5),
			Children: []protocol.DocumentSymbol{
				{Name: "addr", Kind: protocol.SKField, Range: rng(4, 4)},
			},
		},
		{Name: "Handle", Kind: protocol.SKMethod, Range: rng(9, 12)},
	}
}

func runOutline(t *testing.T, tool *tools.FileOutline, uri string, includeDocs *bool) string {
	t.Helper()
	args := map[string]any{"uri": uri}
	if includeDocs != nil {
		args["include_docs"] = *includeDocs
	}
	raw, _ := json.Marshal(args)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("file_outline Execute: %v", err)
	}
	return out
}

func TestFileOutline_LSPPath(t *testing.T) {
	_, uri := writeOutlineFile(t)
	tool := tools.NewFileOutline(&mockLSP{docSymbols: serverDocSymbols()}, nil, 0, 0)
	out := runOutline(t, tool, uri, nil)

	wantSubstrings := []string{
		"source=lsp",
		"type Server struct  [Struct L4-6]",
		"· Server is the HTTP server.",
		"func (s *Server) Handle(ctx Context, r Request) (Response, error)  [Method L10-13]",
		"· Handle processes a request and",
	}
	for _, w := range wantSubstrings {
		if !strings.Contains(out, w) {
			t.Errorf("outline missing %q in:\n%s", w, out)
		}
	}
}

func TestFileOutline_NestedFieldIndented(t *testing.T) {
	_, uri := writeOutlineFile(t)
	tool := tools.NewFileOutline(&mockLSP{docSymbols: serverDocSymbols()}, nil, 0, 0)
	out := runOutline(t, tool, uri, nil)

	// addr is a child of Server, so it is indented two spaces.
	if !strings.Contains(out, "\n  addr string  [Field L5]") {
		t.Errorf("expected addr indented under Server; got:\n%s", out)
	}
}

func TestFileOutline_IncludeDocsFalse(t *testing.T) {
	_, uri := writeOutlineFile(t)
	tool := tools.NewFileOutline(&mockLSP{docSymbols: serverDocSymbols()}, nil, 0, 0)
	no := false
	out := runOutline(t, tool, uri, &no)

	if strings.Contains(out, "Server is the HTTP server.") {
		t.Errorf("include_docs=false should omit doc lines; got:\n%s", out)
	}
	// Signatures must still be present.
	if !strings.Contains(out, "type Server struct  [Struct L4-6]") {
		t.Errorf("signatures should remain with include_docs=false; got:\n%s", out)
	}
}

// newIndexedMarkdownStore indexes one Markdown file — a language flagged
// PreferStructuralOutline — and waits for the background indexer.
func newIndexedMarkdownStore(t *testing.T) (store *topology.Store, uri string) {
	t.Helper()
	ws := t.TempDir()
	path := filepath.Join(ws, "notes.md")
	if err := os.WriteFile(path, []byte("# Title\n\n## Section One\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{tsext.NewMarkdown()})
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
	t.Fatal("topology did not index notes.md within 5s")
	return nil, ""
}

// TestFileOutline_MarkupPrefersStructuralMap pins the markup pre-empt: for a
// language flagged PreferStructuralOutline the Map is consulted BEFORE the
// language server, whose documentSymbol for markup is a node per tag/attribute
// and unusable as an outline. A healthy, answering LSP must not win here.
func TestFileOutline_MarkupPrefersStructuralMap(t *testing.T) {
	store, uri := newIndexedMarkdownStore(t)
	tool := tools.NewFileOutline(&mockLSP{docSymbols: serverDocSymbols()}, nil, 0, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	out := runOutline(t, tool, uri, nil)

	if !strings.Contains(out, "source=topology") {
		t.Errorf("markup outline must come from the structural Map; got:\n%s", out)
	}
	if strings.Contains(out, "Server") {
		t.Errorf("LSP symbols leaked into a markup outline; got:\n%s", out)
	}
	if !strings.Contains(out, "Section One") {
		t.Errorf("expected the Markdown headings from the Map; got:\n%s", out)
	}
}

func TestFileOutline_TopologyFallback(t *testing.T) {
	store, uri := newIndexedStore(t)
	tool := tools.NewFileOutline(brokenLSP(), nil, 0, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	out := runOutline(t, tool, uri, nil)

	if !strings.Contains(out, "source=topology") {
		t.Errorf("expected topology source label; got:\n%s", out)
	}
	if !strings.Contains(out, "func HandleRequest()") {
		t.Errorf("expected HandleRequest signature from topology; got:\n%s", out)
	}
}

// With no topology fallback wired, an LSP error surfaces rather than an empty
// outline.
func TestFileOutline_NoFallbackSurfacesError(t *testing.T) {
	_, uri := writeOutlineFile(t)
	tool := tools.NewFileOutline(brokenLSP(), nil, 0, 0)
	raw, _ := json.Marshal(map[string]any{"uri": uri})
	if _, err := tool.Execute(context.Background(), raw); err == nil {
		t.Fatal("expected the LSP error to surface when no topology fallback is wired")
	}
}

func TestFileOutline_MissingURI(t *testing.T) {
	tool := tools.NewFileOutline(&mockLSP{}, nil, 0, 0)
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error when uri is omitted")
	}
}

func TestFileOutline_DirectoryTarget(t *testing.T) {
	tool := tools.NewFileOutline(&mockLSP{docSymbols: serverDocSymbols()}, nil, 0, 0)
	raw, _ := json.Marshal(map[string]any{"uri": "file://" + t.TempDir()})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error outlining a directory")
	}
	if !strings.Contains(err.Error(), "file_outline:") {
		t.Errorf("error should carry the tool prefix, got: %v", err)
	}
	if !strings.Contains(err.Error(), "use list_directory") {
		t.Errorf("error should suggest list_directory, got: %v", err)
	}
}

func TestFileOutline_NonexistentFile(t *testing.T) {
	tool := tools.NewFileOutline(&mockLSP{docSymbols: serverDocSymbols()}, nil, 0, 0)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///no/such/file_xyz.go"})
	if _, err := tool.Execute(context.Background(), raw); err == nil {
		t.Fatal("expected error reading a nonexistent file")
	}
}
