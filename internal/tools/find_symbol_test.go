package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/tools"
)

func TestFindSymbol_WorkspaceSearch(t *testing.T) {
	mock := &mockLSP{
		wsSymbols: []protocol.SymbolInformation{
			{Name: "Greeter", Kind: protocol.SKClass,
				Location: protocol.Location{URI: "file:///p/main.go",
					Range: protocol.Range{Start: protocol.Position{Line: 9}}}},
			{Name: "greet", Kind: protocol.SKMethod,
				Location: protocol.Location{URI: "file:///p/main.go",
					Range: protocol.Range{Start: protocol.Position{Line: 14}}}},
		},
	}
	tool := tools.NewFindSymbol(mock, nil, time.Minute, nil)
	args, _ := json.Marshal(map[string]any{"query": "Greet"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Greeter") {
		t.Errorf("expected Greeter in result:\n%s", result)
	}
	if !strings.Contains(result, "Class") {
		t.Errorf("expected Class in result:\n%s", result)
	}
	// Line 9 (0-based) → line 10 (1-based)
	if !strings.Contains(result, ":10") {
		t.Errorf("expected :10 in result:\n%s", result)
	}
}

func TestFindSymbol_WorkspaceSearch_Empty(t *testing.T) {
	mock := &mockLSP{wsSymbols: nil}
	tool := tools.NewFindSymbol(mock, nil, time.Minute, nil)
	args, _ := json.Marshal(map[string]any{"query": "Xyz"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No symbols") {
		t.Errorf("expected no-symbols message, got: %s", result)
	}
}

func TestFindSymbol_DocumentSearch(t *testing.T) {
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "Greeter", Kind: protocol.SKClass,
				Range: protocol.Range{Start: protocol.Position{Line: 4}},
				Children: []protocol.DocumentSymbol{
					{Name: "greet", Kind: protocol.SKMethod,
						Range: protocol.Range{Start: protocol.Position{Line: 7}}},
				}},
		},
	}
	tool := tools.NewFindSymbol(mock, nil, time.Minute, nil)
	args, _ := json.Marshal(map[string]any{"query": "greet", "uri": "file:///p/main.go"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	// Should find both Greeter (contains "greet") and greet method.
	if !strings.Contains(result, "Greeter") {
		t.Errorf("expected Greeter in result:\n%s", result)
	}
	if !strings.Contains(result, "greet") {
		t.Errorf("expected greet in result:\n%s", result)
	}
}

func TestFindSymbol_DocumentSearch_NoMatch(t *testing.T) {
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "Greeter", Kind: protocol.SKClass},
		},
	}
	tool := tools.NewFindSymbol(mock, nil, time.Minute, nil)
	args, _ := json.Marshal(map[string]any{"query": "Xyz", "uri": "file:///p/main.go"})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No symbols") {
		t.Errorf("expected no-match message, got: %s", result)
	}
}

func TestFindSymbol_LSPError(t *testing.T) {
	mock := &mockLSP{err: errors.New("lsp unavailable")}
	tool := tools.NewFindSymbol(mock, nil, time.Minute, nil)
	args, _ := json.Marshal(map[string]any{"query": "Greeter"})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when LSP fails")
	}
}

func TestFindSymbol_EmptyQuery(t *testing.T) {
	tool := tools.NewFindSymbol(&mockLSP{}, nil, time.Minute, nil)
	args, _ := json.Marshal(map[string]any{"query": ""})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestFindSymbol_Interface(t *testing.T) {
	tool := tools.NewFindSymbol(&mockLSP{}, nil, time.Minute, nil)
	if tool.Name() != "find_symbol" {
		t.Errorf("unexpected name: %s", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("description must not be empty")
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Errorf("inputSchema is not valid JSON: %v", err)
	}
}
