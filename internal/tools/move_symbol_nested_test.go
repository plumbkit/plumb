package tools_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// move_symbol_nested_test.go is issue #571: a struct field T.Run and a method
// (*S).Run share a bare name. The resolver's exact-name tier names the field
// (the method only competes when nothing carries the name itself), but the field
// is nested, so the tool's top-level lookup missed it, took the tree-sitter
// fallback behind a healthy gopls, and moved the METHOD under a banner blaming
// the language server.
//
// move_symbol moves top-level declarations. The resolver's single match being
// a member is a refusal that names it, never a hand-off to another tree.

const (
	runMethodLine = "func (s *S) Run() {}"

	fieldMethodSrc = "package demo\n\n" +
		"type T struct {\n\tRun func()\n}\n\n" +
		"type S struct{}\n\n" +
		runMethodLine + "\n"
)

func nestedRange(startLine, startChar, endLine, endChar uint32) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: startLine, Character: startChar},
		End:   protocol.Position{Line: endLine, Character: endChar},
	}
}

// fieldMethodSymbols is the tree gopls returns for fieldMethodSrc: the field
// nested under its struct, the method flat and named by its receiver.
func fieldMethodSymbols() []protocol.DocumentSymbol {
	field := protocol.DocumentSymbol{
		Name: "Run", Kind: protocol.SKField,
		Range: nestedRange(3, 1, 3, 11), SelectionRange: nestedRange(3, 1, 3, 4),
	}
	return []protocol.DocumentSymbol{
		{Name: "T", Kind: protocol.SKStruct, Range: nestedRange(2, 0, 4, 1), SelectionRange: nestedRange(2, 5, 2, 6), Children: []protocol.DocumentSymbol{field}},
		{Name: "S", Kind: protocol.SKStruct, Range: nestedRange(6, 0, 6, 15), SelectionRange: nestedRange(6, 5, 6, 6)},
		{Name: "(*S).Run", Kind: protocol.SKMethod, Range: nestedRange(8, 0, 8, uint32(len(runMethodLine))), SelectionRange: nestedRange(8, 12, 8, 15)},
	}
}

// moveFrom runs move_symbol over src with the given server tree, with or
// without the tree-sitter index wired. It returns the tool's result and both
// files afterwards.
func moveFrom(t *testing.T, src string, syms []protocol.DocumentSymbol, withIndex bool, namePath string) (out, srcAfter, dstAfter string, err error) {
	t.Helper()
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", src)
	dstPath, dstURI := writeInDir(t, dir, "dst.go", moveDstBefore)
	tool := tools.NewMoveSymbol(&mockLSP{docSymbols: syms}, time.Second)
	if withIndex {
		store := openTopologyStore(t, dir)
		tool = tool.WithTopologyFallback(func() *topology.Store { return store })
	}
	out, err = tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri": srcURI, "name_path": namePath, "destination_uri": dstURI, "dry_run": false,
	}))
	return out, readFileText(t, srcPath), readFileText(t, dstPath), err
}

func TestMoveSymbol_FieldNamedRunIsRefusedNotTheMethod(t *testing.T) {
	for _, withIndex := range []bool{true, false} {
		name := "healthy server, no index"
		if withIndex {
			name = "healthy server and index"
		}
		t.Run(name, func(t *testing.T) {
			out, src, dst, err := moveFrom(t, fieldMethodSrc, fieldMethodSymbols(), withIndex, "Run")
			if err == nil {
				t.Fatalf("Run resolves to the field T.Run, which move_symbol cannot move; it moved something:\n%s", out)
			}
			// The refusal names what it found, why it cannot move it, and where
			// the declaration the agent more likely meant is addressed.
			for _, want := range []string{"T/Run", "field", "top-level", `"S/Run"`} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal should say %q: %v", want, err)
				}
			}
			if strings.Contains(err.Error(), "topology fallback") || strings.Contains(err.Error(), "LSP unavailable") {
				t.Errorf("a healthy server answered; the refusal blames it: %v", err)
			}
			if src != fieldMethodSrc || dst != moveDstBefore {
				t.Errorf("a refused move must leave both files untouched:\nsource:\n%s\ndestination:\n%s", src, dst)
			}
		})
	}
}

// TestMoveSymbol_MethodPathFromTheRefusalMovesTheMethod is the other direction:
// the refusal is no blanket one. The path it offers moves the method, from the
// server's own range and with no banner, and leaves the field alone.
func TestMoveSymbol_MethodPathFromTheRefusalMovesTheMethod(t *testing.T) {
	out, src, dst, err := moveFrom(t, fieldMethodSrc, fieldMethodSymbols(), true, "S/Run")
	if err != nil {
		t.Fatalf("S/Run names the method and must move it: %v", err)
	}
	if strings.Contains(src, runMethodLine) || !strings.Contains(dst, runMethodLine) {
		t.Errorf("the method did not move:\nsource:\n%s\ndestination:\n%s", src, dst)
	}
	if !strings.Contains(src, "Run func()") {
		t.Errorf("the field was touched:\n%s", src)
	}
	if strings.Contains(out, "topology fallback") || strings.Contains(out, "LSP unavailable") {
		t.Errorf("a healthy server resolved it; the banner blames the server:\n%s", out)
	}
}

// TestMoveSymbol_BareNameOfANestedMemberIsRefusedByItsFullPath: the same rule
// for a class member. Outer/Inner/run is the only run in the tree, and a plain
// "run" still does not select a member.
func TestMoveSymbol_BareNameOfANestedMemberIsRefusedByItsFullPath(t *testing.T) {
	out, src, dst, err := moveFrom(t, onlyInnerPySrc, onlyInnerPySymbols(), true, "run")
	if err == nil || !strings.Contains(err.Error(), "Outer/Inner/run") || !strings.Contains(err.Error(), "method") {
		t.Fatalf("a bare run is a member of Inner and should be refused by its full path, got err=%v out:\n%s", err, out)
	}
	if src != onlyInnerPySrc || dst != moveDstBefore {
		t.Errorf("a refused move must leave both files untouched:\nsource:\n%s\ndestination:\n%s", src, dst)
	}
}

// TestMoveSymbol_UniqueBareMethodResolvesFromTheServer: a plain name the
// resolver matches to one top-level method (gopls names it "(*S).Run") is that
// method, taken from the server's range. It used to reach the same method only by
// way of the tree-sitter fallback, announced as an unavailable server.
func TestMoveSymbol_UniqueBareMethodResolvesFromTheServer(t *testing.T) {
	const src = "package demo\n\ntype S struct{}\n\n" + runMethodLine + "\n"
	syms := []protocol.DocumentSymbol{
		{Name: "S", Kind: protocol.SKStruct, Range: nestedRange(2, 0, 2, 15), SelectionRange: nestedRange(2, 5, 2, 6)},
		{Name: "(*S).Run", Kind: protocol.SKMethod, Range: nestedRange(4, 0, 4, uint32(len(runMethodLine))), SelectionRange: nestedRange(4, 12, 4, 15)},
	}
	out, srcAfter, dstAfter, err := moveFrom(t, src, syms, true, "Run")
	if err != nil {
		t.Fatalf("the only Run is the method: %v", err)
	}
	if strings.Contains(srcAfter, runMethodLine) || !strings.Contains(dstAfter, runMethodLine) {
		t.Errorf("the method did not move:\nsource:\n%s\ndestination:\n%s", srcAfter, dstAfter)
	}
	if strings.Contains(out, "topology fallback") || strings.Contains(out, "LSP unavailable") {
		t.Errorf("a healthy server resolved it; the banner blames the server:\n%s", out)
	}
}

// namespacedWidgetSymbols is a server that wraps a declaration in a namespace:
// the declaration is nested in the tree and top-level in the language.
func namespacedWidgetSymbols() []protocol.DocumentSymbol {
	widget := protocol.DocumentSymbol{
		Name: "Foo", Kind: protocol.SKFunction,
		Range: nestedRange(3, 0, 3, 27), SelectionRange: nestedRange(3, 5, 3, 8),
	}
	return []protocol.DocumentSymbol{{
		Name: "demo", Kind: protocol.SKNamespace,
		Range: nestedRange(0, 0, 5, 27), SelectionRange: nestedRange(0, 8, 0, 12),
		Children: []protocol.DocumentSymbol{widget},
	}}
}

// TestMoveSymbol_DeclarationInANamespaceIsNotAMember: what makes a symbol
// "nested" for the refusal is being a member of a type or a body, not the way a
// server groups declarations. A namespace's declaration is still a top-level one.
func TestMoveSymbol_DeclarationInANamespaceIsNotAMember(t *testing.T) {
	out, src, dst, err := moveFrom(t, moveSrc, namespacedWidgetSymbols(), true, "Foo")
	if err != nil {
		t.Fatalf("a declaration inside a namespace moves like any other: %v", err)
	}
	if strings.Contains(src, "func Foo() int { return 1 }") || !strings.Contains(dst, "func Foo() int { return 1 }") {
		t.Errorf("Foo did not move:\nsource:\n%s\ndestination:\n%s", src, dst)
	}
	if strings.Contains(out, "topology fallback") {
		t.Errorf("a healthy server resolved it, yet the fallback banner appeared:\n%s", out)
	}
}

// TestMoveSymbol_StaleServerTreeDoesNotMoveTheIndexsSymbol is the move_symbol
// half of the shared rule: the server answered without Foo, so Foo is not found,
// whatever the index says.
func TestMoveSymbol_StaleServerTreeDoesNotMoveTheIndexsSymbol(t *testing.T) {
	stale := []protocol.DocumentSymbol{symbolAt("Bar", 5, 5, 27)}
	out, src, dst, err := moveFrom(t, moveSrc, stale, true, "Foo")
	if err == nil {
		t.Fatalf("the server answered without Foo; the index's Foo was moved:\n%s", out)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("the refusal should say Foo was not found: %v", err)
	}
	if src != moveSrc || dst != moveDstBefore {
		t.Errorf("a refused move must leave both files untouched:\nsource:\n%s\ndestination:\n%s", src, dst)
	}
}
