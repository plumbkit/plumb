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

// fallbackFixture writes a small Go file and opens a topology store over it.
// The store's ExtractFile re-parses on demand, so no index wait is needed.
func fallbackFixture(t *testing.T) (store *topology.Store, fpath, uri string) {
	t.Helper()
	ws := t.TempDir()
	src := "package demo\n\nfunc Alpha() int {\n\treturn 1\n}\n\nfunc Beta() int {\n\treturn 2\n}\n"
	fpath = filepath.Join(ws, "demo.go")
	if err := os.WriteFile(fpath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, fpath, "file://" + fpath
}

func TestReadSymbol_TopologyFallback(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	tool := tools.NewReadSymbol(brokenLSP(), nil, 0, 0, tools.NewReadTracker()).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"path": uri, "name": "Beta"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected topology fallback to succeed, got: %v", err)
	}
	for _, want := range []string{"topology fallback", "func Beta() int {", "return 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("read_symbol fallback missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "return 1") {
		t.Errorf("read_symbol fallback for Beta should not include Alpha's body:\n%s", out)
	}
}

// TestReadSymbol_ColdLSPBareMethodName proves S1: when the LSP answers but does
// NOT resolve a bare method name (a cold server), read_symbol falls back to the
// structural Map — the Go extractor names methods by their bare name — instead
// of returning "No symbol named".
func TestReadSymbol_ColdLSPBareMethodName(t *testing.T) {
	ws := t.TempDir()
	src := "package demo\n\ntype Server struct{}\n\nfunc (s *Server) handleConn() int { return 7 }\n"
	fpath := filepath.Join(ws, "srv.go")
	if err := os.WriteFile(fpath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Empty LSP: answers with no symbols and no error (the cold-server case).
	tool := tools.NewReadSymbol(&mockLSP{}, nil, 0, 0, tools.NewReadTracker()).
		WithTopologyFallback(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"path": "file://" + fpath, "name": "handleConn"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected the cold-LSP topology fallback to resolve, got: %v", err)
	}
	if !strings.Contains(out, "handleConn") || !strings.Contains(out, "return 7") {
		t.Errorf("cold-LSP bare method name should resolve via the Map:\n%s", out)
	}
}

// TestReadSymbol_URIAlias proves S2: read_symbol accepts `uri` as an alias for
// `path`.
func TestReadSymbol_URIAlias(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	tool := tools.NewReadSymbol(brokenLSP(), nil, 0, 0, tools.NewReadTracker()).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{"uri": uri, "name": "Alpha"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("uri alias should work like path, got: %v", err)
	}
	if !strings.Contains(out, "func Alpha() int {") {
		t.Errorf("expected Alpha via uri alias:\n%s", out)
	}
}

// TestReadSymbol_TopologyFallback_ReceiverSegmentNotSubstring guards that a
// dotted ReceiverType.Method name resolves on a whole-segment match, not a
// substring: "User.Save" must resolve (User).Save and never (SuperUser).Save.
func TestReadSymbol_TopologyFallback_ReceiverSegmentNotSubstring(t *testing.T) {
	ws := t.TempDir()
	src := "package demo\n\n" +
		"type User struct{}\n\n" +
		"func (u User) Save() int { return 1 }\n\n" +
		"type SuperUser struct{}\n\n" +
		"func (s SuperUser) Save() int { return 2 }\n"
	fpath := filepath.Join(ws, "user.go")
	if err := os.WriteFile(fpath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	tool := tools.NewReadSymbol(brokenLSP(), nil, 0, 0, tools.NewReadTracker()).
		WithTopologyFallback(func() *topology.Store { return s })
	args, _ := json.Marshal(map[string]any{"path": "file://" + fpath, "name": "User.Save"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected topology fallback to succeed, got: %v", err)
	}
	if !strings.Contains(out, "return 1") {
		t.Errorf("read_symbol User.Save should resolve (User).Save (return 1):\n%s", out)
	}
	if strings.Contains(out, "return 2") {
		t.Errorf("read_symbol User.Save must not match (SuperUser).Save (return 2):\n%s", out)
	}
}

func TestReplaceSymbolBody_TopologyFallback(t *testing.T) {
	store, fpath, uri := fallbackFixture(t)
	tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{
		"uri":       uri,
		"name_path": "Alpha",
		"content":   "func Alpha() int {\n\treturn 99\n}",
		"dry_run":   false,
	})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected fallback replace to succeed, got: %v", err)
	}
	if !strings.Contains(out, "topology fallback") {
		t.Errorf("replace output should note the fallback:\n%s", out)
	}
	got, err := os.ReadFile(fpath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(got)
	if !strings.Contains(content, "return 99") {
		t.Errorf("Alpha body not replaced:\n%s", content)
	}
	if strings.Contains(content, "return 1\n") {
		t.Errorf("old Alpha body should be gone:\n%s", content)
	}
	if !strings.Contains(content, "func Beta() int {\n\treturn 2\n}") {
		t.Errorf("Beta should be untouched:\n%s", content)
	}
	if strings.Count(content, "func Alpha() int {") != 1 {
		t.Errorf("Alpha should appear exactly once:\n%s", content)
	}
}

func TestReplaceSymbolBody_TopologyFallback_DryRunDefault(t *testing.T) {
	store, fpath, uri := fallbackFixture(t)
	before, _ := os.ReadFile(fpath)
	tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	// dry_run defaults to true → preview only, file untouched.
	args, _ := json.Marshal(map[string]any{
		"uri":       uri,
		"name_path": "Alpha",
		"content":   "func Alpha() int { return 0 }",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "DRY RUN") || !strings.Contains(out, "topology fallback") {
		t.Errorf("expected a dry-run preview noting the fallback:\n%s", out)
	}
	after, _ := os.ReadFile(fpath)
	if string(before) != string(after) {
		t.Error("dry run must not modify the file")
	}
}

func TestInsertAfterSymbol_TopologyFallback(t *testing.T) {
	store, fpath, uri := fallbackFixture(t)
	tool := tools.NewInsertAfterSymbol(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{
		"uri":       uri,
		"name_path": "Alpha",
		"content":   "\n\nfunc Gamma() int { return 3 }",
		"dry_run":   false,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("expected fallback insert to succeed, got: %v", err)
	}
	content, _ := os.ReadFile(fpath)
	s := string(content)
	ai, gi, bi := strings.Index(s, "func Alpha"), strings.Index(s, "func Gamma"), strings.Index(s, "func Beta")
	if gi < 0 {
		t.Fatalf("Gamma not inserted:\n%s", s)
	}
	if ai >= gi || gi >= bi {
		t.Errorf("Gamma should sit between Alpha and Beta (a=%d g=%d b=%d):\n%s", ai, gi, bi, s)
	}
}

// TestReadSymbol_TopologyFallback_WarmingNote proves read_symbol's fallback
// note says "still warming" — not "LSP unavailable" — when the warm-up probe
// reports the server as warming.
func TestReadSymbol_TopologyFallback_WarmingNote(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	tool := tools.NewReadSymbol(brokenLSP(), nil, 0, 0, tools.NewReadTracker()).
		WithTopologyFallback(func() *topology.Store { return store }).
		WithLSPWarmup(func(string) (bool, time.Duration) { return true, 4 * time.Second })
	args, _ := json.Marshal(map[string]any{"path": uri, "name": "Beta"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected topology fallback to succeed, got: %v", err)
	}
	if !strings.Contains(out, "still warming") || !strings.Contains(out, "~4s") {
		t.Errorf("expected a warming note with elapsed time:\n%s", out)
	}
	if strings.Contains(out, "LSP unavailable") {
		t.Errorf("a warming server must not be reported unavailable:\n%s", out)
	}
}

// TestSymbolEditFallbackNoteWarming proves the symbol-edit fallback banner is
// warming-aware: with a warming probe the banner says "still warming"; without
// one it stays the legacy text byte-for-byte.
func TestSymbolEditFallbackNoteWarming(t *testing.T) {
	const legacyBanner = "[topology fallback — LSP unavailable; symbol located by tree-sitter, range is line-granular]"

	t.Run("warming probe swaps in the warming banner", func(t *testing.T) {
		store, _, uri := fallbackFixture(t)
		tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
			WithTopologyFallback(func() *topology.Store { return store }).
			WithLSPWarmup(func(string) (bool, time.Duration) { return true, 4 * time.Second })
		args, _ := json.Marshal(map[string]any{"uri": uri, "name_path": "Alpha", "content": "x"})
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("expected fallback preview to succeed, got: %v", err)
		}
		if !strings.Contains(out, "still warming") || !strings.Contains(out, "~4s") {
			t.Errorf("expected a warming banner with elapsed time:\n%s", out)
		}
		if strings.Contains(out, "LSP unavailable") {
			t.Errorf("a warming server must not be reported unavailable:\n%s", out)
		}
	})

	t.Run("unwired probe keeps the legacy banner byte-for-byte", func(t *testing.T) {
		store, _, uri := fallbackFixture(t)
		tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
			WithTopologyFallback(func() *topology.Store { return store })
		args, _ := json.Marshal(map[string]any{"uri": uri, "name_path": "Alpha", "content": "x"})
		out, err := tool.Execute(context.Background(), args)
		if err != nil {
			t.Fatalf("expected fallback preview to succeed, got: %v", err)
		}
		if !strings.Contains(out, legacyBanner) {
			t.Errorf("expected the byte-exact legacy banner:\n%s", out)
		}
	})
}

// Without a fallback wired, a broken LSP must surface its error unchanged.
func TestReplaceSymbolBody_NoFallbackSurfacesError(t *testing.T) {
	tool := tools.NewReplaceSymbolBody(brokenLSP(), 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///x.go", "name_path": "X", "content": "y"})
	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected the LSP error to surface when no topology fallback is wired")
	}
}

// TestReplaceSymbolBody_Fallback_CharPreciseRange proves the topology fallback
// now reports a char-precise range (real start/end columns from the node's byte
// span) rather than the old line-granular 0→EOL approximation. Alpha's body ends
// at the closing brace on its own line (column 1, after the '}').
func TestReplaceSymbolBody_Fallback_CharPreciseRange(t *testing.T) {
	store, _, uri := fallbackFixture(t)
	tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	// Alpha spans lines 3–5 (1-based); the dry-run prints 0-based LSP lines 2–4.
	args, _ := json.Marshal(map[string]any{
		"uri": uri, "name_path": "Alpha", "content": "x",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	// The end position is the closing brace: line 4 (0-based), char 1 — the
	// char-precise column. A line-granular range would report char 0 with the
	// end on the same line only by EOL length; the precise column is the proof.
	if !strings.Contains(out, "line 2 char 0 → line 4 char 1") {
		t.Errorf("expected char-precise range 'line 2 char 0 → line 4 char 1', got:\n%s", out)
	}
}

// TestReplaceSymbolBody_Fallback_IncludeDocComment_PreciseSpan proves that with
// include_doc_comment the replacement start is taken from the node's precise doc
// span (the topology extractor records it exactly), covering the leading comment.
func TestReplaceSymbolBody_Fallback_IncludeDocComment_PreciseSpan(t *testing.T) {
	ws := t.TempDir()
	src := "package demo\n\n// Alpha does a thing.\nfunc Alpha() int {\n\treturn 1\n}\n"
	fpath := filepath.Join(ws, "demo.go")
	if err := os.WriteFile(fpath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + fpath, "name_path": "Alpha",
		"content": "func Alpha() int { return 2 }", "dry_run": false,
		"include_doc_comment": true,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("replace with include_doc_comment failed: %v", err)
	}
	got, _ := os.ReadFile(fpath)
	content := string(got)
	// The leading doc comment must be gone (covered by the precise doc span).
	if strings.Contains(content, "// Alpha does a thing.") {
		t.Errorf("doc comment should have been replaced via the precise doc span:\n%s", content)
	}
	if !strings.Contains(content, "func Alpha() int { return 2 }") {
		t.Errorf("new declaration missing:\n%s", content)
	}
}
