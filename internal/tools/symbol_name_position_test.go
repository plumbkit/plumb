package tools_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// symbolWithKeywordRange models how gopls reports a top-level declaration: the
// DocumentSymbol Range spans the whole declaration and starts at the keyword
// (column 0), while SelectionRange points at the identifier (a non-zero column).
// A definition/references query must use the SelectionRange — a query at the
// keyword position yields "no identifier found" / no result from a real server.
func symbolWithKeywordRange(name string) []protocol.DocumentSymbol {
	return []protocol.DocumentSymbol{{
		Name: name,
		Kind: protocol.SKFunction,
		Range: protocol.Range{
			Start: protocol.Position{Line: 5, Character: 0}, // the `func` keyword
			End:   protocol.Position{Line: 10},
		},
		SelectionRange: protocol.Range{
			Start: protocol.Position{Line: 5, Character: 5}, // the identifier
			End:   protocol.Position{Line: 5, Character: 5 + uint32(len(name))},
		},
	}}
}

// TestGetDefinition_ByName_UsesSelectionRange is the regression guard for the
// symbol_name bug: executeByName fed sym.Range.Start (the keyword at column 0)
// into the LSP Definition query instead of sym.SelectionRange.Start (the
// identifier), so by-name lookups failed against a real language server while
// the position-ignoring mock hid it. This asserts the QUERIED position is the
// identifier.
func TestGetDefinition_ByName_UsesSelectionRange(t *testing.T) {
	mock := &mockLSP{
		docSymbols: symbolWithKeywordRange("Serve"),
		locations: []protocol.Location{
			{URI: "file:///p/server.go", Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}}},
		},
	}
	tool := tools.NewGetDefinition(mock, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/server.go", "symbol_name": "Serve"})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	want := protocol.Position{Line: 5, Character: 5}
	if mock.lastDefPos != want {
		t.Errorf("Definition queried at %+v, want the identifier (SelectionRange) %+v — not the declaration keyword",
			mock.lastDefPos, want)
	}
}

// TestFindReferences_ByName_UsesSelectionRange is the same regression guard for
// find_references.executeByName.
func TestFindReferences_ByName_UsesSelectionRange(t *testing.T) {
	mock := &mockLSP{
		docSymbols: symbolWithKeywordRange("underTempDir"),
		locations: []protocol.Location{
			{URI: "file:///p/paths.go", Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}}},
		},
	}
	tool := tools.NewFindReferences(mock, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/paths.go", "symbol_name": "underTempDir"})

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	want := protocol.Position{Line: 5, Character: 5}
	if mock.lastRefPos != want {
		t.Errorf("References queried at %+v, want the identifier (SelectionRange) %+v — not the declaration keyword",
			mock.lastRefPos, want)
	}
}
