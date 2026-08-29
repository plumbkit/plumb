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

// TestRenameSymbol_AmbiguousName_CandidateRoundTripsEndToEnd is the MANDATORY
// end-to-end round-trip guard (PLAN-363 review round 2): an ambiguous
// symbol_name is refused with a candidate list, and retrying with the exact
// candidate string the error names must succeed AND target the RIGHT symbol —
// not just produce a string that looks plausible (the bug both blocking
// defects shared). Exercised through the real RenameSymbol.Execute, not the
// internal disambiguatedNames helper directly.
func TestRenameSymbol_AmbiguousName_CandidateRoundTripsEndToEnd(t *testing.T) {
	closeInWidget := protocol.DocumentSymbol{
		Name:           "close",
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 5, Character: 8}},
	}
	closeInGadget := protocol.DocumentSymbol{
		Name:           "close",
		SelectionRange: protocol.Range{Start: protocol.Position{Line: 15, Character: 8}},
	}
	m := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "Widget", Children: []protocol.DocumentSymbol{closeInWidget}},
			{Name: "Gadget", Children: []protocol.DocumentSymbol{closeInGadget}},
		},
		renameResult: &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
			"file:///p/x.go": {{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}, NewText: "renamed"}},
		}},
	}
	tool := tools.NewRenameSymbol(m, time.Minute)

	// Step 1: the ambiguous call is refused with candidate guidance.
	ambigArgs, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "symbol_name": "close", "new_name": "shut", "dry_run": true})
	_, err := tool.Execute(context.Background(), ambigArgs)
	if err == nil {
		t.Fatal("expected an ambiguity error for a symbol_name matching two symbols")
	}
	if !strings.Contains(err.Error(), "Widget.close") || !strings.Contains(err.Error(), "Gadget.close") {
		t.Fatalf("expected both disambiguated candidates named in the error, got: %v", err)
	}

	// Step 2: retry with the FIRST candidate the error named — must succeed and
	// target Widget's close specifically (not Gadget's, not re-error as ambiguous).
	retryArgs, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "symbol_name": "Widget.close", "new_name": "shut", "dry_run": true})
	out, err := tool.Execute(context.Background(), retryArgs)
	if err != nil {
		t.Fatalf("retrying with the exact candidate the error named must succeed, got: %v", err)
	}
	if !strings.Contains(out, "Renamed to") {
		t.Errorf("expected a rename preview, got:\n%s", out)
	}
	if m.lastRenamePos != closeInWidget.SelectionRange.Start {
		t.Errorf("Rename queried at %+v, want Widget.close's position %+v — the candidate must select exactly the RIGHT symbol",
			m.lastRenamePos, closeInWidget.SelectionRange.Start)
	}

	// Step 3: the other candidate must independently select Gadget's close.
	retryArgs2, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "symbol_name": "Gadget.close", "new_name": "shut", "dry_run": true})
	if _, err := tool.Execute(context.Background(), retryArgs2); err != nil {
		t.Fatalf("retrying with the second candidate must also succeed, got: %v", err)
	}
	if m.lastRenamePos != closeInGadget.SelectionRange.Start {
		t.Errorf("Rename queried at %+v, want Gadget.close's position %+v", m.lastRenamePos, closeInGadget.SelectionRange.Start)
	}
}

// TestRenameSymbol_AmbiguousName_UndisambiguableFallsBackToLineCharacter is the
// BLOCKING-1 end-to-end guard: when no symbol_name candidate round-trips (two
// unrelated top-level symbols sharing a name), the error must NOT offer a fake
// symbol_name — and the line/character it names instead must actually work.
func TestRenameSymbol_AmbiguousName_UndisambiguableFallsBackToLineCharacter(t *testing.T) {
	fooA := protocol.DocumentSymbol{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}}}
	fooB := protocol.DocumentSymbol{Name: "Foo", SelectionRange: protocol.Range{Start: protocol.Position{Line: 8, Character: 5}}}
	m := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{fooA, fooB},
		renameResult: &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
			"file:///p/x.go": {{Range: protocol.Range{Start: protocol.Position{Line: 0, Character: 0}, End: protocol.Position{Line: 0, Character: 3}}, NewText: "renamed"}},
		}},
	}
	tool := tools.NewRenameSymbol(m, time.Minute)

	ambigArgs, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "symbol_name": "Foo", "new_name": "Bar", "dry_run": true})
	_, err := tool.Execute(context.Background(), ambigArgs)
	if err == nil {
		t.Fatal("expected an ambiguity error")
	}
	msg := err.Error()
	// The error must not offer "Foo (line 3)" (or similar) as if it were a
	// retryable symbol_name — it must say so isn't one, and name line/character.
	if !strings.Contains(msg, "no unique symbol_name") {
		t.Fatalf("expected the error to say there is no unique symbol_name, got: %v", err)
	}
	if !strings.Contains(msg, "line:2") || !strings.Contains(msg, "character:5") {
		t.Fatalf("expected the error to name usable line/character coordinates, got: %v", err)
	}

	// The named line/character must actually work.
	retryArgs, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "line": 2, "character": 5, "new_name": "Bar", "dry_run": true})
	if _, err := tool.Execute(context.Background(), retryArgs); err != nil {
		t.Fatalf("the named line/character fallback must actually work, got: %v", err)
	}
	if m.lastRenamePos != fooA.SelectionRange.Start {
		t.Errorf("Rename queried at %+v, want %+v", m.lastRenamePos, fooA.SelectionRange.Start)
	}
}
