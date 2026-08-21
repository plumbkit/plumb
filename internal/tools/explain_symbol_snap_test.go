package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// TestExplainSymbol_ByName resolves a symbol_name against the document-symbol
// tree and queries hover at its identifier position — PLAN-363 item 1
// (symbol_name primary) applied to explain_symbol, which previously took only
// a raw line/character.
func TestExplainSymbol_ByName(t *testing.T) {
	m := &mockLSP{
		docSymbols: enclosingSymbol("Target"),
		hover: &protocol.Hover{Contents: protocol.MarkupContent{
			Kind: "markdown", Value: "func Target() — a thing.",
		}},
	}
	tool := tools.NewExplainSymbol(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "symbol_name": "Target"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("by-name explain_symbol should succeed, got error: %v", err)
	}
	if !strings.Contains(out, "a thing") {
		t.Errorf("expected hover content for Target, got:\n%s", out)
	}
}

// TestExplainSymbol_ByName_NoMatch surfaces a clear message for an unknown name.
func TestExplainSymbol_ByName_NoMatch(t *testing.T) {
	m := &mockLSP{docSymbols: enclosingSymbol("Target")}
	tool := tools.NewExplainSymbol(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "symbol_name": "Missing"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `No symbol named "Missing"`) {
		t.Errorf("expected a no-symbol message, got:\n%s", out)
	}
}

// TestExplainSymbol_NeitherNameNorPosition asserts the relaxed schema still
// requires one of symbol_name or a full position.
func TestExplainSymbol_NeitherNameNorPosition(t *testing.T) {
	tool := tools.NewExplainSymbol(&mockLSP{}, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go"})

	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected an error when neither symbol_name nor line/character is given")
	}
}

// TestExplainSymbol_SnapOnMiss is the item-2 repro for explain_symbol: a raw
// position on a non-identifier line fails "no identifier found"; the tool
// snaps to the enclosing symbol and returns hover content, prefixed with a
// note — "no identifier found" must never be a terminal error when a snap
// target exists.
func TestExplainSymbol_SnapOnMiss(t *testing.T) {
	m := &snapMock{
		mockLSP: &mockLSP{
			docSymbols: enclosingSymbol("Target"),
			hover:      &protocol.Hover{Contents: protocol.MarkupContent{Value: "doc for Target"}},
		},
		good: protocol.Position{Line: 5, Character: 5},
	}
	tool := tools.NewExplainSymbol(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "line": 7, "character": 0})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("snap should recover, got error: %v", err)
	}
	if !strings.Contains(out, "note: no identifier at") {
		t.Errorf("expected a snap note, got:\n%s", out)
	}
	if !strings.Contains(out, "doc for Target") {
		t.Errorf("expected hover content after snap, got:\n%s", out)
	}
}

// TestExplainSymbol_SnapNoEnclosingActionableError covers the case where a
// missed position has no enclosing symbol: the error names nearby symbols and
// points at symbol_name rather than a bare "no identifier found".
func TestExplainSymbol_SnapNoEnclosingActionableError(t *testing.T) {
	m := &snapMock{
		mockLSP: &mockLSP{docSymbols: enclosingSymbol("Serve")},
		good:    protocol.Position{Line: 5, Character: 5},
	}
	tool := tools.NewExplainSymbol(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "line": 100, "character": 0})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected an actionable error when nothing encloses the line")
	}
	msg := err.Error()
	for _, want := range []string{"did you mean", "Serve", "symbol_name"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// TestExplainSymbol_ByName_Ambiguity_RendersEveryMatch is the item-3 repro: a
// symbol_name matching several symbols never returns a bare "ambiguous"
// error — every match is resolved and rendered.
func TestExplainSymbol_ByName_Ambiguity_RendersEveryMatch(t *testing.T) {
	dup := enclosingSymbol("Target")
	dup2 := protocol.DocumentSymbol{
		Name:           "Target",
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 20, Character: 5}},
	}
	m := &mockLSP{
		docSymbols: append(dup, dup2),
		hover:      &protocol.Hover{Contents: protocol.MarkupContent{Value: "shared doc"}},
	}
	tool := tools.NewExplainSymbol(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "symbol_name": "Target"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("ambiguous name must resolve, not error: %v", err)
	}
	if !strings.Contains(out, "2 matches for") {
		t.Errorf("expected both matches rendered, got:\n%s", out)
	}
}
