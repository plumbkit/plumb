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

func TestGetDefinition_SingleLocation(t *testing.T) {
	mock := &mockLSP{
		locations: []protocol.Location{
			{URI: "file:///p/base.go", Range: protocol.Range{Start: protocol.Position{Line: 2, Character: 5}}},
		},
	}
	tool := tools.NewGetDefinition(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 10, "character": 3})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "file:///p/base.go") {
		t.Errorf("expected base.go URI in result:\n%s", result)
	}
	// Line 2 (0-based) → line 3 (1-based)
	if !strings.Contains(result, ":3:") {
		t.Errorf("expected :3: in result:\n%s", result)
	}
}

func TestGetDefinition_MultipleLocations(t *testing.T) {
	mock := &mockLSP{
		locations: []protocol.Location{
			{URI: "file:///p/a.go", Range: protocol.Range{Start: protocol.Position{Line: 0}}},
			{URI: "file:///p/b.go", Range: protocol.Range{Start: protocol.Position{Line: 5}}},
		},
	}
	tool := tools.NewGetDefinition(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 0, "character": 0})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "2 definitions") {
		t.Errorf("expected '2 definitions' in result:\n%s", result)
	}
	if !strings.Contains(result, "a.go") || !strings.Contains(result, "b.go") {
		t.Errorf("expected both files in result:\n%s", result)
	}
}

func TestGetDefinition_NoResult(t *testing.T) {
	mock := &mockLSP{locations: nil}
	tool := tools.NewGetDefinition(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 0, "character": 0})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No definition") {
		t.Errorf("expected 'No definition' in result:\n%s", result)
	}
}

func TestGetDefinition_LSPError(t *testing.T) {
	mock := &mockLSP{err: errors.New("lsp error")}
	tool := tools.NewGetDefinition(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 0, "character": 0})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when LSP fails")
	}
}

func TestGetDefinition_EmptyURI(t *testing.T) {
	tool := tools.NewGetDefinition(&mockLSP{}, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "", "line": 0, "character": 0})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty uri")
	}
}

func TestGetDefinition_NeitherNameNorPosition(t *testing.T) {
	tool := tools.NewGetDefinition(&mockLSP{}, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go"})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when neither symbol_name nor line+character provided")
	}
	if !strings.Contains(err.Error(), "symbol_name") {
		t.Errorf("error should mention symbol_name: %v", err)
	}
}

func TestGetDefinition_ByName_SingleMatch(t *testing.T) {
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Serve",
				Kind: protocol.SKFunction,
				Range: protocol.Range{
					Start: protocol.Position{Line: 5, Character: 0},
					End:   protocol.Position{Line: 10},
				},
			},
		},
		locations: []protocol.Location{
			{URI: "file:///p/server.go", Range: protocol.Range{Start: protocol.Position{Line: 5, Character: 0}}},
		},
	}
	tool := tools.NewGetDefinition(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{
		"uri":         "file:///p/main.go",
		"symbol_name": "Serve",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "server.go") {
		t.Errorf("expected definition file in output:\n%s", out)
	}
}

func TestGetDefinition_ByName_NotFound(t *testing.T) {
	mock := &mockLSP{docSymbols: nil}
	tool := tools.NewGetDefinition(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{
		"uri":         "file:///p/main.go",
		"symbol_name": "Missing",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No symbol") {
		t.Errorf("expected no-symbol message:\n%s", out)
	}
}

func TestGetDefinition_Interface(t *testing.T) {
	tool := tools.NewGetDefinition(&mockLSP{}, nil, time.Minute)
	if tool.Name() != "get_definition" {
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
