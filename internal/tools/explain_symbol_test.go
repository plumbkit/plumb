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

func TestExplainSymbol_WithContent(t *testing.T) {
	mock := &mockLSP{
		hover: &protocol.Hover{
			Contents: protocol.MarkupContent{
				Kind:  "markdown",
				Value: "```go\nfunc Greet(name string) string\n```\nGreet returns a greeting.",
			},
		},
	}
	tool := tools.NewExplainSymbol(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 5, "character": 2})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Greet") {
		t.Errorf("expected hover content in result:\n%s", result)
	}
}

func TestExplainSymbol_NilHover(t *testing.T) {
	mock := &mockLSP{hover: nil}
	tool := tools.NewExplainSymbol(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 0, "character": 0})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No documentation") {
		t.Errorf("expected 'No documentation' in result:\n%s", result)
	}
}

func TestExplainSymbol_EmptyContent(t *testing.T) {
	mock := &mockLSP{hover: &protocol.Hover{Contents: protocol.MarkupContent{Value: ""}}}
	tool := tools.NewExplainSymbol(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 0, "character": 0})

	result, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No documentation") {
		t.Errorf("expected 'No documentation' for empty content:\n%s", result)
	}
}

func TestExplainSymbol_LSPError(t *testing.T) {
	mock := &mockLSP{err: errors.New("hover failed")}
	tool := tools.NewExplainSymbol(mock, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go", "line": 0, "character": 0})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error when LSP fails")
	}
}

func TestExplainSymbol_EmptyURI(t *testing.T) {
	tool := tools.NewExplainSymbol(&mockLSP{}, nil, time.Minute)
	args, _ := json.Marshal(map[string]any{"uri": "", "line": 0, "character": 0})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error for empty uri")
	}
}

func TestExplainSymbol_Interface(t *testing.T) {
	tool := tools.NewExplainSymbol(&mockLSP{}, nil, time.Minute)
	if tool.Name() != "explain_symbol" {
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
