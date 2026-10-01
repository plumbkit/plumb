package tools_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/config"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
	"github.com/plumbkit/plumb/internal/topology/extractors/treesitter"
)

// symbol_edit_nested_parent_test.go is PR #559's round-3 blocker, end to end. A
// class Inner nested in Outer has a run, and Outer has its own run below it. A
// HEALTHY language server resolves Outer/run to Outer's run — and then
// docCommentStartPreferTopology re-resolved the same name_path through the
// tree-sitter index, which answered with Inner's run, so include_doc_comment
// started the edit at Inner's doc comment and replace_symbol_body DELETED Inner's
// run. A write that names one symbol must never edit another.

const nestedPySrc = "class Outer:\n" + // 0
	"    class Inner:\n" + // 1
	"        # inner doc\n" + // 2
	"        def run(self):\n" + // 3
	"            return 1\n" + // 4
	"    # outer doc\n" + // 5
	"    def run(self):\n" + // 6
	"        return 2\n" // 7

// docSym is a document symbol spanning startLine..endLine (0-based), ending
// endChar characters into its last line, with its identifier on the first line.
func docSym(name string, kind protocol.SymbolKind, startLine, endLine, endChar uint32, kids ...protocol.DocumentSymbol) protocol.DocumentSymbol {
	return protocol.DocumentSymbol{
		Name: name, Kind: kind,
		Range:          protocol.Range{Start: protocol.Position{Line: startLine}, End: protocol.Position{Line: endLine, Character: endChar}},
		SelectionRange: protocol.Range{Start: protocol.Position{Line: startLine, Character: 4}, End: protocol.Position{Line: startLine, Character: 7}},
		Children:       kids,
	}
}

// nestedPySymbols is what a healthy Python server returns for nestedPySrc.
func nestedPySymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		docSym("Outer", protocol.SKClass, 0, 7, 16,
			docSym("Inner", protocol.SKClass, 1, 4, 20, docSym("run", protocol.SKMethod, 3, 4, 20)),
			docSym("run", protocol.SKMethod, 6, 7, 16)),
	}
}

// onlyInnerPySrc and onlyInnerPySymbols: Outer has NO run of its own.
const onlyInnerPySrc = "class Outer:\n" + // 0
	"    class Inner:\n" + // 1
	"        # inner doc\n" + // 2
	"        def run(self):\n" + // 3
	"            return 1\n" // 4

func onlyInnerPySymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		docSym("Outer", protocol.SKClass, 0, 4, 20,
			docSym("Inner", protocol.SKClass, 1, 4, 20, docSym("run", protocol.SKMethod, 3, 4, 20))),
	}
}

// openStoreIn opens a topology store over dir with ext.
func openStoreIn(t *testing.T, dir string, ext topology.Extractor) *topology.Store {
	t.Helper()
	s, err := topology.Open(dir, config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, []topology.Extractor{ext})
	if err != nil {
		t.Fatalf("topology.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestReplaceSymbolBody_IncludeDocComment_HealthyServerEditsOnlyTheNamedRun is the
// repro. The server's answer is Outer's run (line 6); the doc comment the edit
// extends over must be THAT run's ("# outer doc"), and Inner's run, with its own
// doc comment, must come out untouched.
func TestReplaceSymbolBody_IncludeDocComment_HealthyServerEditsOnlyTheNamedRun(t *testing.T) {
	dir := t.TempDir()
	path, uri := writeInDir(t, dir, "a.py", nestedPySrc)
	store := openStoreIn(t, dir, treesitter.NewPython())
	tool := tools.NewReplaceSymbolBody(&mockLSP{docSymbols: nestedPySymbols()}, time.Second).
		WithTopologyFallback(func() *topology.Store { return store })
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"uri": uri, "name_path": "Outer/run", "content": "    def run(self):\n        return 99",
		"include_doc_comment": true, "dry_run": false,
	})); err != nil {
		t.Fatalf("replace Outer/run: %v", err)
	}
	want := "class Outer:\n" +
		"    class Inner:\n" +
		"        # inner doc\n" +
		"        def run(self):\n" +
		"            return 1\n" +
		"    def run(self):\n" +
		"        return 99\n"
	if got := readFileText(t, path); got != want {
		t.Errorf("replace_symbol_body Outer/run with include_doc_comment edited the wrong symbol.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestInsertBeforeSymbol_IncludeDocComment_HealthyServerInsertsAboveTheNamedRun:
// the same re-resolution put the insertion above Inner's doc comment, inside
// Inner, instead of above Outer's own run and its comment.
func TestInsertBeforeSymbol_IncludeDocComment_HealthyServerInsertsAboveTheNamedRun(t *testing.T) {
	dir := t.TempDir()
	path, uri := writeInDir(t, dir, "a.py", nestedPySrc)
	store := openStoreIn(t, dir, treesitter.NewPython())
	tool := tools.NewInsertBeforeSymbol(&mockLSP{docSymbols: nestedPySymbols()}, time.Second).
		WithTopologyFallback(func() *topology.Store { return store })
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"uri": uri, "name_path": "Outer/run", "content": "    def helper(self):\n        pass\n",
		"include_doc_comment": true, "dry_run": false,
	})); err != nil {
		t.Fatalf("insert before Outer/run: %v", err)
	}
	want := "class Outer:\n" +
		"    class Inner:\n" +
		"        # inner doc\n" +
		"        def run(self):\n" +
		"            return 1\n" +
		"    def helper(self):\n" +
		"        pass\n" +
		"    # outer doc\n" +
		"    def run(self):\n" +
		"        return 2\n"
	if got := readFileText(t, path); got != want {
		t.Errorf("insert_before_symbol Outer/run with include_doc_comment inserted in the wrong place.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestMoveSymbol_IncludeDocComment_HealthyServerMovesOnlyTheNamedRun: move_symbol
// defaults include_doc_comment to true, so the same re-resolution made it carry
// Inner's run and doc comment away instead of Outer's.
func TestMoveSymbol_IncludeDocComment_HealthyServerMovesOnlyTheNamedRun(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "a.py", nestedPySrc)
	dstPath, dstURI := writeInDir(t, dir, "b.py", "x = 1\n")
	store := openStoreIn(t, dir, treesitter.NewPython())
	tool := tools.NewMoveSymbol(&mockLSP{docSymbols: nestedPySymbols()}, time.Second).
		WithTopologyFallback(func() *topology.Store { return store })
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri": srcURI, "name_path": "Outer/run", "destination_uri": dstURI, "dry_run": false,
	})); err != nil {
		t.Fatalf("move Outer/run: %v", err)
	}
	src, dst := readFileText(t, srcPath), readFileText(t, dstPath)
	for _, keep := range []string{"# inner doc", "def run(self):\n            return 1"} {
		if !strings.Contains(src, keep) {
			t.Errorf("Inner's run lost %q:\n%s", keep, src)
		}
	}
	if strings.Contains(src, "# outer doc") || strings.Contains(src, "return 2") {
		t.Errorf("Outer's run (and its doc comment) should have left the source:\n%s", src)
	}
	if !strings.Contains(dst, "# outer doc") || !strings.Contains(dst, "return 2") || strings.Contains(dst, "return 1") {
		t.Errorf("destination should hold Outer's run with its doc comment, and nothing of Inner's:\n%s", dst)
	}
}

// TestSymbolEdits_ParentWithNoRunOfItsOwnIsRefused: the language server says
// Outer/run is not found, and so should the tree-sitter fallback — it used to
// "find" Inner's run by transitive containment and edit that. Run for every tool
// that resolves a name_path through the fallback, with the server answering "not
// found" and with the server absent; the file must come out untouched.
func TestSymbolEdits_ParentWithNoRunOfItsOwnIsRefused(t *testing.T) {
	servers := []struct {
		name string
		lsp  func() *mockLSP
	}{
		{"server answers not found", func() *mockLSP { return &mockLSP{docSymbols: onlyInnerPySymbols()} }},
		{"server absent", brokenLSP},
	}
	for _, srv := range servers {
		for _, tool := range []string{"replace_symbol_body", "insert_before_symbol", "insert_after_symbol", "move_symbol"} {
			t.Run(srv.name+"/"+tool, func(t *testing.T) {
				dir := t.TempDir()
				path, uri := writeInDir(t, dir, "a.py", onlyInnerPySrc)
				dstPath, dstURI := writeInDir(t, dir, "b.py", "x = 1\n")
				store := openStoreIn(t, dir, treesitter.NewPython())
				topo := func() *topology.Store { return store }
				args := map[string]any{"uri": uri, "name_path": "Outer/run", "content": "        def run(self):\n            return 99", "dry_run": false}
				var err error
				switch tool {
				case "replace_symbol_body":
					_, err = tools.NewReplaceSymbolBody(srv.lsp(), time.Second).WithTopologyFallback(topo).Execute(context.Background(), moveArgs(t, args))
				case "insert_before_symbol":
					_, err = tools.NewInsertBeforeSymbol(srv.lsp(), time.Second).WithTopologyFallback(topo).Execute(context.Background(), moveArgs(t, args))
				case "insert_after_symbol":
					_, err = tools.NewInsertAfterSymbol(srv.lsp(), time.Second).WithTopologyFallback(topo).Execute(context.Background(), moveArgs(t, args))
				case "move_symbol":
					_, err = tools.NewMoveSymbol(srv.lsp(), time.Second).WithTopologyFallback(topo).Execute(context.Background(), moveArgs(t, map[string]any{
						"source_uri": uri, "name_path": "Outer/run", "destination_uri": dstURI, "dry_run": false,
					}))
				}
				if err == nil {
					t.Errorf("%s resolved Outer/run, which names no declaration (only Inner has a run)", tool)
				}
				if got := readFileText(t, path); got != onlyInnerPySrc {
					t.Errorf("a refused %s must leave the source untouched:\n%s", tool, got)
				}
				if got := readFileText(t, dstPath); got != "x = 1\n" {
					t.Errorf("a refused %s must leave the destination untouched:\n%s", tool, got)
				}
			})
		}
	}
}

// TestReplaceSymbolBody_IncludeDocComment_DocCommentFollowsTheResolvedSymbol:
// the doc comment is looked up for the symbol the tool ALREADY resolved — by its
// name and where it starts — whichever tier resolved it. Where the server's range
// starts on a line no topology node starts on (a decorated def, whose range pyright
// opens at the decorator), nothing matches exactly and the line-scan finds the
// comment above it; where it starts on the node's own line, the extractor's span
// is used and reaches above the decorator.
func TestReplaceSymbolBody_IncludeDocComment_DocCommentFollowsTheResolvedSymbol(t *testing.T) {
	const src = "class Widget:\n" + // 0
		"    # Documents bump.\n" + // 1
		"    @property\n" + // 2
		"    def bump(self):\n" + // 3
		"        return 1\n" // 4
	for _, tc := range []struct {
		name      string
		startLine uint32 // where the server says the symbol's range starts
		want      string
	}{
		// Range from the decorator: no node starts there, the line-scan runs from it.
		{"range opens at the decorator", 2, "class Widget:\n    @property\n    def bump(self):\n        return 2\n"},
		// Range from the def: the node starting there carries a doc span above the decorator.
		{"range opens at the def", 3, "class Widget:\n    def bump(self):\n        return 2\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, uri := writeInDir(t, dir, "w.py", src)
			store := openStoreIn(t, dir, treesitter.NewPython())
			lsp := &mockLSP{docSymbols: []protocol.DocumentSymbol{
				docSym("Widget", protocol.SKClass, 0, 4, 16,
					docSym("bump", protocol.SKMethod, tc.startLine, 4, 16)),
			}}
			tool := tools.NewReplaceSymbolBody(lsp, time.Second).WithTopologyFallback(func() *topology.Store { return store })
			content := "    def bump(self):\n        return 2"
			if tc.startLine == 2 {
				content = "    @property\n    def bump(self):\n        return 2"
			}
			if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
				"uri": uri, "name_path": "Widget/bump", "content": content,
				"include_doc_comment": true, "dry_run": false,
			})); err != nil {
				t.Fatalf("replace Widget/bump: %v", err)
			}
			if got := readFileText(t, path); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestReplaceSymbolBody_IncludeDocComment_SameNameSameColumnIsToldApartByLine: two
// classes each hold a decorated run at the same column, so nothing but the line
// the server's range opens on says which node's doc span is meant. The edit
// extends over the doc comment above THAT run's decorator, and the other class is
// left alone.
func TestReplaceSymbolBody_IncludeDocComment_SameNameSameColumnIsToldApartByLine(t *testing.T) {
	const src = "class A:\n" + // 0
		"    # A.run doc\n" + // 1
		"    @property\n" + // 2
		"    def run(self):\n" + // 3
		"        return 1\n" + // 4
		"\n" + // 5
		"class B:\n" + // 6
		"    # B.run doc\n" + // 7
		"    @property\n" + // 8
		"    def run(self):\n" + // 9
		"        return 2\n" // 10
	run := func(line uint32) protocol.DocumentSymbol {
		d := docSym("run", protocol.SKMethod, line, line+1, 16)
		d.Range.Start.Character = 4
		return d
	}
	dir := t.TempDir()
	path, uri := writeInDir(t, dir, "ab.py", src)
	store := openStoreIn(t, dir, treesitter.NewPython())
	lsp := &mockLSP{docSymbols: []protocol.DocumentSymbol{
		docSym("A", protocol.SKClass, 0, 4, 16, run(3)),
		docSym("B", protocol.SKClass, 6, 10, 16, run(9)),
	}}
	tool := tools.NewReplaceSymbolBody(lsp, time.Second).WithTopologyFallback(func() *topology.Store { return store })
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"uri": uri, "name_path": "B/run", "content": "    def run(self):\n        return 99",
		"include_doc_comment": true, "dry_run": false,
	})); err != nil {
		t.Fatalf("replace B/run: %v", err)
	}
	want := "class A:\n    # A.run doc\n    @property\n    def run(self):\n        return 1\n\n" +
		"class B:\n    def run(self):\n        return 99\n"
	if got := readFileText(t, path); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

// rustEditSrc is rustParentsSrc's shape: a run in each of two impls and a trait
// method implemented for Foo.
const rustEditSrc = "struct Foo;\n" + // 0
	"struct Bar;\n" + // 1
	"impl Foo {\n" + // 2
	"    fn run(&self) {}\n" + // 3
	"}\n" + // 4
	"impl Bar {\n" + // 5
	"    fn run(&self) {}\n" + // 6
	"}\n" + // 7
	"trait T { fn go(&self); }\n" + // 8
	"impl T for Foo {\n" + // 9
	"    fn go(&self) {}\n" + // 10
	"}\n" // 11

// TestReplaceSymbolBody_RustTypeSlashMethodResolvesByTheFallback: Type/method in
// a Rust file resolves through the tree-sitter index when no language server
// answers — an inherent method and a trait-impl method alike — and edits only the
// method of the type it names. Round 2 refused every one of these.
func TestReplaceSymbolBody_RustTypeSlashMethodResolvesByTheFallback(t *testing.T) {
	for _, tc := range []struct {
		namePath string
		line     int // 0-based line of the method that must change
	}{
		{"Foo/run", 3},
		{"Bar/run", 6},
		{"Foo/go", 10}, // the impl's method, not the trait's signature on line 8
	} {
		t.Run(tc.namePath, func(t *testing.T) {
			dir := t.TempDir()
			path, uri := writeInDir(t, dir, "a.rs", rustEditSrc)
			store := openStoreIn(t, dir, treesitter.NewRust())
			tool := tools.NewReplaceSymbolBody(brokenLSP(), time.Second).WithTopologyFallback(func() *topology.Store { return store })
			if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
				"uri": uri, "name_path": tc.namePath, "content": "fn changed(&self) {}", "dry_run": false,
			})); err != nil {
				t.Fatalf("replace %s: %v", tc.namePath, err)
			}
			want := strings.Split(rustEditSrc, "\n")
			want[tc.line] = "    fn changed(&self) {}"
			if got := readFileText(t, path); got != strings.Join(want, "\n") {
				t.Errorf("replace %s changed the wrong method.\ngot:\n%s\nwant:\n%s", tc.namePath, got, strings.Join(want, "\n"))
			}
		})
	}
}

// TestReplaceSymbolBody_RustUnevidencedParentIsRefused: an impl for a type this
// file does not declare, and a method the named type does not have, resolve to
// nothing — and leave the file as it was.
func TestReplaceSymbolBody_RustUnevidencedParentIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, src, namePath string }{
		{"impl of an undeclared type", "impl Missing {\n    fn run(&self) {}\n}\n", "Missing/run"},
		{"method of another type", rustEditSrc, "Bar/go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path, uri := writeInDir(t, dir, "a.rs", tc.src)
			store := openStoreIn(t, dir, treesitter.NewRust())
			tool := tools.NewReplaceSymbolBody(brokenLSP(), time.Second).WithTopologyFallback(func() *topology.Store { return store })
			if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
				"uri": uri, "name_path": tc.namePath, "content": "fn changed(&self) {}", "dry_run": false,
			})); err == nil {
				t.Errorf("%s resolved, want it refused", tc.namePath)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != tc.src {
				t.Errorf("a refused replace must leave the file untouched:\n%s", got)
			}
		})
	}
}

// TestReplaceSymbolBody_AmbiguousFallbackPathIsRefusedAndSaysSo: two overloads of
// one name in one class cannot be told apart by a name_path. The fallback refuses
// both, and says it found several — not that it found none, which would send the
// agent looking for a typo.
func TestReplaceSymbolBody_AmbiguousFallbackPathIsRefusedAndSaysSo(t *testing.T) {
	const src = "class Foo {\n    void add(int a) {}\n    void add(String s) {}\n}\n"
	dir := t.TempDir()
	path, uri := writeInDir(t, dir, "A.java", src)
	store := openStoreIn(t, dir, treesitter.NewJava())
	tool := tools.NewReplaceSymbolBody(brokenLSP(), time.Second).WithTopologyFallback(func() *topology.Store { return store })
	_, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"uri": uri, "name_path": "Foo/add", "content": "void add() {}", "dry_run": false,
	}))
	if err == nil {
		t.Fatal("Foo/add names two overloads; the fallback resolved one anyway")
	}
	for _, want := range []string{"Foo/add", "2 declarations", "lines 2, 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "finds no symbol") {
		t.Errorf("the refusal claims the fallback found nothing, when it found two: %v", err)
	}
	if got := readFileText(t, path); got != src {
		t.Errorf("a refused replace must leave the file untouched:\n%s", got)
	}
}
