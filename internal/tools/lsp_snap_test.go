package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// snapMock rejects any cursor position except `good` with a gopls-style "no
// identifier found" error, mimicking a server that refuses a whitespace/comment
// position but answers once the query is snapped to the enclosing symbol's
// identifier. DocumentSymbols (used by the snap) comes from the embedded mock.
type snapMock struct {
	*mockLSP
	good protocol.Position
}

func (m *snapMock) References(_ context.Context, p protocol.ReferenceParams) ([]protocol.Location, error) {
	if p.Position != m.good {
		return nil, errors.New("no identifier found")
	}
	return m.locations, nil
}

func (m *snapMock) Definition(_ context.Context, p protocol.DefinitionParams) ([]protocol.Location, error) {
	if p.Position != m.good {
		return nil, errors.New("no identifier found")
	}
	return m.locations, nil
}

func (m *snapMock) PrepareCallHierarchy(_ context.Context, p protocol.PrepareCallHierarchyParams) ([]protocol.CallHierarchyItem, error) {
	if p.Position != m.good {
		return nil, errors.New("no identifier found")
	}
	return m.chItems, nil
}

// enclosingSymbol models a function spanning lines 5–10 whose identifier
// (SelectionRange) sits at (5,5): a query on any body line snaps here.
func enclosingSymbol(name string) []protocol.DocumentSymbol {
	return symbolWithKeywordRange(name)
}

// TestFindReferences_SnapOnMiss is the RC3 repro: a raw position on a
// non-identifier line fails "no identifier found"; the tool snaps to the
// enclosing symbol and returns references, prefixed with a note.
func TestFindReferences_SnapOnMiss(t *testing.T) {
	m := &snapMock{
		mockLSP: &mockLSP{
			docSymbols: enclosingSymbol("Target"),
			locations:  []protocol.Location{{URI: "file:///p/x.go", Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}}}},
		},
		good: protocol.Position{Line: 5, Character: 5},
	}
	tool := tools.NewFindReferences(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "line": 7, "character": 0})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("snap should recover, got error: %v", err)
	}
	if !strings.Contains(out, "note: no identifier at") {
		t.Errorf("expected a snap note, got:\n%s", out)
	}
	if !strings.Contains(out, "reference(s)") {
		t.Errorf("expected references after snap, got:\n%s", out)
	}
}

// TestGetDefinition_SnapOnMiss is the RC3 repro for get_definition.
func TestGetDefinition_SnapOnMiss(t *testing.T) {
	m := &snapMock{
		mockLSP: &mockLSP{
			docSymbols: enclosingSymbol("Target"),
			locations:  []protocol.Location{{URI: "file:///p/x.go", Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}}}},
		},
		good: protocol.Position{Line: 5, Character: 5},
	}
	tool := tools.NewGetDefinition(m, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "line": 7, "character": 0})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("snap should recover, got error: %v", err)
	}
	if !strings.Contains(out, "note: no identifier at") {
		t.Errorf("expected a snap note, got:\n%s", out)
	}
	if !strings.Contains(out, "Definition at") {
		t.Errorf("expected a definition after snap, got:\n%s", out)
	}
}

// TestCallHierarchy_SnapOnMiss is the RC3 repro for call_hierarchy: no topology
// wired, so a miss falls through to the document-symbol snap.
func TestCallHierarchy_SnapOnMiss(t *testing.T) {
	m := &snapMock{
		mockLSP: &mockLSP{
			docSymbols: enclosingSymbol("Target"),
			chItems: []protocol.CallHierarchyItem{{
				Name: "Target", Kind: protocol.SKFunction, URI: "file:///p/x.go",
				Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}},
			}},
		},
		good: protocol.Position{Line: 5, Character: 5},
	}
	tool := tools.NewCallHierarchy(m, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/x.go", "line": 7, "character": 0})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("snap should recover, got error: %v", err)
	}
	if !strings.Contains(out, "note: no identifier at") {
		t.Errorf("expected a snap note, got:\n%s", out)
	}
	if !strings.Contains(out, "Call hierarchy for Target") {
		t.Errorf("expected the call hierarchy after snap, got:\n%s", out)
	}
}

// TestFindReferences_SnapNoEnclosingActionableError covers the case where a
// missed position has no enclosing symbol: the error names nearby symbols and
// points at symbol_name rather than repeating the raw 0-based hint.
func TestFindReferences_SnapNoEnclosingActionableError(t *testing.T) {
	m := &snapMock{
		mockLSP: &mockLSP{docSymbols: enclosingSymbol("Serve")},
		good:    protocol.Position{Line: 5, Character: 5},
	}
	tool := tools.NewFindReferences(m, nil, time.Minute, 0)
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

// TestCallHierarchy_ByName is the RC3 2a repro: call_hierarchy resolves a
// symbol_name (no line/character), mirroring find_references' name path.
func TestCallHierarchy_ByName(t *testing.T) {
	m := &mockLSP{
		docSymbols: enclosingSymbol("Serve"),
		chItems: []protocol.CallHierarchyItem{{
			Name: "Serve", Kind: protocol.SKFunction, URI: "file:///p/s.go",
			Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 5}},
		}},
	}
	tool := tools.NewCallHierarchy(m, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/s.go", "symbol_name": "Serve"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("by-name call_hierarchy should succeed, got error: %v", err)
	}
	if !strings.Contains(out, "Call hierarchy for Serve") {
		t.Errorf("expected hierarchy for Serve, got:\n%s", out)
	}
}

// TestCallHierarchy_ByName_NoMatch surfaces a clear message for an unknown name.
func TestCallHierarchy_ByName_NoMatch(t *testing.T) {
	m := &mockLSP{docSymbols: enclosingSymbol("Serve")}
	tool := tools.NewCallHierarchy(m, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/s.go", "symbol_name": "Missing"})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, `No symbol named "Missing"`) {
		t.Errorf("expected a no-symbol message, got:\n%s", out)
	}
}

// TestCallHierarchy_NeitherNameNorPosition asserts the relaxed schema still
// requires one of symbol_name or a full position.
func TestCallHierarchy_NeitherNameNorPosition(t *testing.T) {
	tool := tools.NewCallHierarchy(&mockLSP{}, 0)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/s.go"})

	if _, err := tool.Execute(context.Background(), args); err == nil {
		t.Fatal("expected an error when neither symbol_name nor line/character is given")
	}
}
