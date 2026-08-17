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
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
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

// TestReadSymbol_TopologyFallback_PointerReceiverMethod pins the PLAN-16 item 11
// case: when the LSP cannot answer at all (an error, not merely an empty cold
// answer), read_symbol's tree-sitter fallback must resolve a pointer-receiver
// method looked up by BARE name (e.g. resolveSessionWorkspace) AND by dotted
// ReceiverType.Method form — the Go extractor names the node by its bare method
// name and records "(*SomeType).SomeMethod" as its Qualified, so both forms
// resolve through the same fallback matcher.
func TestReadSymbol_TopologyFallback_PointerReceiverMethod(t *testing.T) {
	ws := t.TempDir()
	src := "package demo\n\n" +
		"type SomeType struct{}\n\n" +
		"func (t *SomeType) SomeMethod() int { return 42 }\n"
	fpath := filepath.Join(ws, "some.go")
	if err := os.WriteFile(fpath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{goext.New()})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	for _, name := range []string{"SomeMethod", "SomeType.SomeMethod"} {
		t.Run(name, func(t *testing.T) {
			// brokenLSP errors, so the fallback is the ONLY path that can answer —
			// exactly the cold/absent-LSP case PLAN-16 item 11 was about.
			tool := tools.NewReadSymbol(brokenLSP(), nil, 0, 0, tools.NewReadTracker()).
				WithTopologyFallback(func() *topology.Store { return s })
			args, _ := json.Marshal(map[string]any{"path": "file://" + fpath, "name": name})

			out, err := tool.Execute(context.Background(), args)
			if err != nil {
				t.Fatalf("expected the topology fallback to resolve %q, got: %v", name, err)
			}
			if !strings.Contains(out, "SomeMethod") || !strings.Contains(out, "return 42") {
				t.Errorf("fallback for %q should resolve the method body:\n%s", name, out)
			}
		})
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

// docFixture writes src to a file called name in a fresh workspace and opens a
// topology store over it with ext — the write path the doc-span hazard lives
// on, since include_doc_comment reads the extractor's span in preference to the
// line-scan heuristic.
func docFixture(t *testing.T, name, src string, ext topology.Extractor) (store *topology.Store, fpath, uri string) {
	t.Helper()
	ws := t.TempDir()
	fpath = filepath.Join(ws, name)
	if err := os.WriteFile(fpath, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := topology.Open(ws, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024},
		[]topology.Extractor{ext})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, fpath, "file://" + fpath
}

// tsDocFixture is docFixture over a .ts file, the shape most of these cases
// want: TS/TSX declarations are exported far more often than not.
func tsDocFixture(t *testing.T, src string) (store *topology.Store, fpath, uri string) {
	t.Helper()
	return docFixture(t, "mod.ts", src, treesitter.NewTypeScript())
}

// TestReplaceSymbolBody_IncludeDocComment_BannerSurvives is the end-to-end pin
// for the doc-span flushness rule, on the exact path that made it dangerous.
// docCommentStartPreferTopology prefers the topology span over the line-scan
// heuristic, and the line-scan stops at a blank line while the extractor's
// previous-sibling scan did not — so a file-leading licence banner became the
// first export's "doc comment" and include_doc_comment (default TRUE on
// move_symbol) quietly deleted it. Both halves are asserted together, because
// the fix is only correct if it keeps the real doc comment attached.
func TestReplaceSymbolBody_IncludeDocComment_BannerSurvives(t *testing.T) {
	const banner = "// Copyright 2026 The Plumb Authors.\n// SPDX-License-Identifier: Apache-2.0\n"

	t.Run("a banner separated by a blank line is not the symbol's doc comment", func(t *testing.T) {
		store, fpath, uri := tsDocFixture(t, banner+`
export function detached(a: number): number {
  return a;
}
`)
		tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
			WithTopologyFallback(func() *topology.Store { return store })
		args, _ := json.Marshal(map[string]any{
			"uri": uri, "name_path": "detached",
			"content": "export function detached(a: number): number { return a + 1; }",
			"dry_run": false, "include_doc_comment": true,
		})
		if _, err := tool.Execute(context.Background(), args); err != nil {
			t.Fatalf("replace with include_doc_comment failed: %v", err)
		}
		got, _ := os.ReadFile(fpath)
		content := string(got)
		if !strings.HasPrefix(content, banner) {
			t.Errorf("the licence banner was consumed as a doc comment:\n%s", content)
		}
		if !strings.Contains(content, "return a + 1;") {
			t.Errorf("body not replaced:\n%s", content)
		}
	})

	t.Run("a doc comment flush against the export still travels with it", func(t *testing.T) {
		store, fpath, uri := tsDocFixture(t, banner+`
/** Documents attached. */
export function attached(b: number): number {
  return b;
}
`)
		tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
			WithTopologyFallback(func() *topology.Store { return store })
		args, _ := json.Marshal(map[string]any{
			"uri": uri, "name_path": "attached",
			"content": "export function attached(b: number): number { return b + 1; }",
			"dry_run": false, "include_doc_comment": true,
		})
		if _, err := tool.Execute(context.Background(), args); err != nil {
			t.Fatalf("replace with include_doc_comment failed: %v", err)
		}
		got, _ := os.ReadFile(fpath)
		content := string(got)
		if strings.Contains(content, "/** Documents attached. */") {
			t.Errorf("the flush doc comment should have been replaced with its symbol:\n%s", content)
		}
		if !strings.HasPrefix(content, banner) {
			t.Errorf("the licence banner must still survive the flush-doc case:\n%s", content)
		}
		if !strings.Contains(content, "return b + 1;") {
			t.Errorf("body not replaced:\n%s", content)
		}
	})
}

// TestReplaceSymbolBody_IncludeDocComment_JSCommentRunIsNotSplit is the write
// path for the other half of the run-walk: a multi-line `//` doc block above
// the first declaration in a .js file. The JavaScript grammar reports a nil
// Parent for a comment that precedes every other top-level node, so a
// PrevSibling-chained walk stopped after one hop and the span covered only the
// LAST line of the block. include_doc_comment starts the edit at that span, so
// the replacement cut the block in half and left the first line orphaned above
// the new declaration — an edit no LSP-up path would ever produce.
func TestReplaceSymbolBody_IncludeDocComment_JSCommentRunIsNotSplit(t *testing.T) {
	store, fpath, uri := docFixture(t, "mod.js", `// Adds two numbers.
// Both arguments are coerced to Number.
export function add(a, b) {
  return a + b;
}
`, treesitter.NewJavaScript())

	tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{
		"uri": uri, "name_path": "add",
		"content": "// Adds two numbers, then one more.\nexport function add(a, b) { return a + b + 1; }",
		"dry_run": false, "include_doc_comment": true,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("replace with include_doc_comment failed: %v", err)
	}
	got, _ := os.ReadFile(fpath)
	content := string(got)
	for _, orphan := range []string{"// Adds two numbers.\n", "// Both arguments are coerced to Number."} {
		if strings.Contains(content, orphan) {
			t.Errorf("doc block was split — %q survives the replacement:\n%s", orphan, content)
		}
	}
	if !strings.Contains(content, "return a + b + 1;") {
		t.Errorf("body not replaced:\n%s", content)
	}
}

// TestReplaceSymbolBody_IncludeDocComment_IndentedMember is the PLAN-288
// end-to-end pin: replacing an indented Python method with
// include_doc_comment must start the edit range at column 0 of the comment's
// line, so replacement content carrying its own four-space indentation lands at
// column 4 — not column 8, which is what the pre-fix topology path (doc span
// start at the comment's own column) produced.
func TestReplaceSymbolBody_IncludeDocComment_IndentedMember(t *testing.T) {
	store, fpath, uri := docFixture(t, "widget.py",
		"class Widget:\n    # Does the thing.\n    def run(self):\n        return 1\n",
		treesitter.NewPython())

	tool := tools.NewReplaceSymbolBody(brokenLSP(), 0).
		WithTopologyFallback(func() *topology.Store { return store })
	args, _ := json.Marshal(map[string]any{
		"uri": uri, "name_path": "run",
		"content":             "    # Does the thing, now faster.\n    def run(self):\n        return 2",
		"dry_run":             false,
		"include_doc_comment": true,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("replace with include_doc_comment failed: %v", err)
	}
	got, _ := os.ReadFile(fpath)
	want := "class Widget:\n    # Does the thing, now faster.\n    def run(self):\n        return 2\n"
	if string(got) != want {
		t.Errorf("indented member replace mismatch\n got: %q\nwant: %q", got, want)
	}
}
