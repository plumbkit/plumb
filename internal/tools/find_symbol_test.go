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

func TestFindSymbol_DocumentSearch(t *testing.T) {
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Greeter", Kind: protocol.SKClass,
				Range: protocol.Range{Start: protocol.Position{Line: 4}},
				Children: []protocol.DocumentSymbol{
					{
						Name: "greet", Kind: protocol.SKMethod,
						Range: protocol.Range{Start: protocol.Position{Line: 7}},
					},
				},
			},
		},
	}
	tool := tools.NewFindSymbol(mock, nil, time.Minute, 0)
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
	tool := tools.NewFindSymbol(mock, nil, time.Minute, 0)
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
	tool := tools.NewFindSymbol(mock, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"query": "Greeter", "uri": "file:///p/main.go"})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when LSP fails")
	}
}

func TestFindSymbol_EmptyQuery(t *testing.T) {
	tool := tools.NewFindSymbol(&mockLSP{}, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"query": "", "uri": "file:///p/main.go"})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty query")
	}
}

func TestFindSymbol_MissingURI(t *testing.T) {
	tool := tools.NewFindSymbol(&mockLSP{}, nil, time.Minute, 0)
	args, _ := json.Marshal(map[string]any{"query": "Greeter"})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when uri is missing")
	}
	if !strings.Contains(err.Error(), "uri is required") {
		t.Errorf("expected uri-required error, got: %v", err)
	}
}

func TestFindSymbol_Interface(t *testing.T) {
	tool := tools.NewFindSymbol(&mockLSP{}, nil, time.Minute, 0)
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
	// uri must now be in the required list.
	required, _ := schema["required"].([]any)
	hasURI := false
	for _, r := range required {
		if r == "uri" {
			hasURI = true
		}
	}
	if !hasURI {
		t.Errorf("schema must mark uri as required, got: %v", required)
	}
}
