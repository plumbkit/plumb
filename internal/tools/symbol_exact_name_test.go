package tools_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
	"github.com/plumbkit/plumb/internal/topology"
)

// funcAndMethodSrc has a function and a method sharing the name Run. gopls lists
// the method as the top-level "(*S).Run", so the function is the only symbol
// literally named Run, and it must win a plain-name lookup (PR #559 review B1).
const funcAndMethodSrc = "package demo\n\ntype S struct{}\n\nfunc Run() int { return 1 }\n\nfunc (s *S) Run() int { return 2 }\n"

func funcAndMethodSymbols() []protocol.DocumentSymbol {
	sym := func(name string, kind protocol.SymbolKind, line, char, endChar uint32) protocol.DocumentSymbol {
		return protocol.DocumentSymbol{
			Name:           name,
			Kind:           kind,
			Range:          protocol.Range{Start: protocol.Position{Line: line}, End: protocol.Position{Line: line, Character: endChar}},
			SelectionRange: protocol.Range{Start: protocol.Position{Line: line, Character: char}},
		}
	}
	return []protocol.DocumentSymbol{
		sym("S", protocol.SKStruct, 2, 5, 15),
		sym("Run", protocol.SKFunction, 4, 5, 27),
		sym("(*S).Run", protocol.SKMethod, 6, 12, 34),
	}
}

var runFuncPos = protocol.Position{Line: 4, Character: 5}

// TestPlainName_ExactNameBeatsReceiverStrippedMethod: every name-taking tool
// resolves "Run" to the function literally named Run, not to an ambiguity with
// the method that matches only once its receiver is stripped.
func TestPlainName_ExactNameBeatsReceiverStrippedMethod(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", funcAndMethodSrc)

	refs := &mockLSP{docSymbols: funcAndMethodSymbols(), locations: []protocol.Location{{URI: srcURI, Range: protocol.Range{Start: runFuncPos}}}}
	out, err := tools.NewFindReferences(refs, nil, time.Minute, 0).
		Execute(context.Background(), rawArgs(t, map[string]any{"uri": srcURI, "symbol_name": "Run"}))
	if err != nil {
		t.Fatalf("find_references: %v", err)
	}
	if refs.lastRefPos != runFuncPos || strings.Contains(out, "symbol matches") {
		t.Errorf("find_references: want the function only (queried %+v):\n%s", refs.lastRefPos, out)
	}

	out, err = tools.NewReadSymbol(&mockLSP{docSymbols: funcAndMethodSymbols()}, nil, time.Minute, 0, tools.NewReadTracker()).
		Execute(context.Background(), rawArgs(t, map[string]any{"path": srcPath, "name": "Run"}))
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "return 1") || strings.Contains(out, "return 2") {
		t.Errorf("read_symbol: want the function's body only:\n%s", out)
	}

	ren := &mockLSP{docSymbols: funcAndMethodSymbols(), renameResult: &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		srcURI: {{Range: protocol.Range{Start: runFuncPos, End: protocol.Position{Line: 4, Character: 8}}, NewText: "Go"}},
	}}}
	if _, err := tools.NewRenameSymbol(ren, 0).Execute(context.Background(), rawArgs(t, map[string]any{
		"uri": srcURI, "symbol_name": "Run", "new_name": "Go", "dry_run": false,
	})); err != nil {
		t.Fatalf("rename_symbol refused a name only one symbol literally has: %v", err)
	}
	if ren.lastRenamePos != runFuncPos {
		t.Errorf("rename_symbol renamed at %+v, want the function %+v", ren.lastRenamePos, runFuncPos)
	}
}

// TestMoveSymbol_FunctionSharingAMethodName is the B1 regression: main moves
// the function; the receiver-stripping rule must not turn that into a refusal.
func TestMoveSymbol_FunctionSharingAMethodName(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", funcAndMethodSrc)
	dstPath, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")

	if _, err := tools.NewMoveSymbol(&mockLSP{docSymbols: funcAndMethodSymbols()}, 0).Execute(context.Background(),
		moveArgs(t, map[string]any{"source_uri": srcURI, "name_path": "Run", "destination_uri": dstURI, "dry_run": false})); err != nil {
		t.Fatalf("move_symbol refused the only symbol literally named Run: %v", err)
	}
	src, _ := os.ReadFile(srcPath)
	dst, _ := os.ReadFile(dstPath)
	if strings.Contains(string(src), "func Run() int") || !strings.Contains(string(src), "func (s *S) Run() int") {
		t.Errorf("source: want the function moved and the method kept:\n%s", src)
	}
	if !strings.Contains(string(dst), "func Run() int { return 1 }") {
		t.Errorf("destination lacks the function:\n%s", dst)
	}
}

const twoMethodsSrc = "package demo\n\ntype A struct{}\n\ntype B struct{}\n\nfunc (a A) Run() int { return 1 }\n\nfunc (b *B) Run() int { return 2 }\n"

func twoMethodsSymbols() []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{
		symbolAt("A", 2, 2, 15),
		symbolAt("B", 4, 4, 15),
		symbolAt("(A).Run", 6, 6, 33),
		symbolAt("(*B).Run", 8, 8, 34),
	}
}

// TestMoveSymbol_TwoGoMethodsRefusedWithAWorkableRemedy: two methods equally
// named Run are ambiguous, and the refusal names the exact name_path for each
// — "A/Run", "B/Run" — rather than a generic hint, and the named path really
// moves the method (the next test).
func TestMoveSymbol_TwoGoMethodsRefusedWithAWorkableRemedy(t *testing.T) {
	dir := t.TempDir()
	_, srcURI := writeInDir(t, dir, "src.go", twoMethodsSrc)
	_, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")
	_, err := tools.NewMoveSymbol(&mockLSP{docSymbols: twoMethodsSymbols()}, 0).Execute(context.Background(),
		moveArgs(t, map[string]any{"source_uri": srcURI, "name_path": "Run", "destination_uri": dstURI, "dry_run": false}))
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("two methods named Run must be refused as ambiguous, got %v", err)
	}
	for _, want := range []string{`"A/Run"`, `"B/Run"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should offer the name_path %s: %v", want, err)
		}
	}
}

// TestMoveSymbol_ReceiverSlashMethodMovesThatMethod proves the remedy the
// refusal offers: with a healthy server listing gopls's flat methods, "B/Run"
// moves B's method and leaves A's.
func TestMoveSymbol_ReceiverSlashMethodMovesThatMethod(t *testing.T) {
	dir := t.TempDir()
	srcPath, srcURI := writeInDir(t, dir, "src.go", twoMethodsSrc)
	dstPath, dstURI := writeInDir(t, dir, "dst.go", "package demo\n\nfunc Keep() {}\n")
	store := openTopologyStore(t, dir)
	tool := tools.NewMoveSymbol(&mockLSP{docSymbols: twoMethodsSymbols()}, 0).
		WithTopologyFallback(func() *topology.Store { return store })
	if _, err := tool.Execute(context.Background(), moveArgs(t, map[string]any{
		"source_uri": srcURI, "name_path": "B/Run", "destination_uri": dstURI, "dry_run": false,
	})); err != nil {
		t.Fatalf("the offered name_path B/Run must move B's method: %v", err)
	}
	src, _ := os.ReadFile(srcPath)
	dst, _ := os.ReadFile(dstPath)
	if !strings.Contains(string(src), "func (a A) Run()") || strings.Contains(string(src), "func (b *B) Run()") {
		t.Errorf("source: want A's method kept and B's moved:\n%s", src)
	}
	if !strings.Contains(string(dst), "func (b *B) Run() int { return 2 }") {
		t.Errorf("destination lacks B's method:\n%s", dst)
	}
}

// TestLSPCrossFileCallers_PicksTheCentreByLine is review N1: topology passes a
// method's bare name, which receivers in one file share, plus the node's span.
// The span picks the right symbol instead of giving up.
func TestLSPCrossFileCallers_PicksTheCentreByLine(t *testing.T) {
	syms := append(goplsMethodSymbols(), protocol.DocumentSymbol{
		Name:           "Close",
		Kind:           protocol.SKFunction,
		Range:          protocol.Range{Start: protocol.Position{Line: 12}, End: protocol.Position{Line: 12, Character: 15}},
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 12, Character: 5}},
	})
	mock := &mockLSP{docSymbols: syms, locations: []protocol.Location{loc("file:///ws/other.go", 3)}}
	fn := tools.NewLSPCrossFileCallers(mock, nil, time.Minute, 0, func() string { return "/ws" })

	for _, tc := range []struct {
		name       string
		start, end int // 1-based topology span
		want       protocol.Position
	}{
		{"pointer-receiver method", 9, 9, protocol.Position{Line: 8, Character: 23}},
		{"value-receiver method", 11, 11, protocol.Position{Line: 10, Character: 21}},
		{"function of the same name", 13, 13, protocol.Position{Line: 12, Character: 5}},
	} {
		mock.lastRefPos = protocol.Position{Line: 999}
		if sites := fn(context.Background(), "p.go", "Close", tc.start, tc.end); len(sites) != 1 {
			t.Errorf("%s: want its cross-file caller, got %+v", tc.name, sites)
		}
		if mock.lastRefPos != tc.want {
			t.Errorf("%s: queried %+v, want %+v", tc.name, mock.lastRefPos, tc.want)
		}
	}
	if sites := fn(context.Background(), "p.go", "Close", 0, 0); sites != nil {
		t.Errorf("a shared name with no span to pick by must answer nothing, got %+v", sites)
	}
}
