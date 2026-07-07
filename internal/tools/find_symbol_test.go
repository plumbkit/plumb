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
	// A missing uri must point the agent at workspace_symbols for a
	// workspace-wide search, not fail with a bare "missing required parameter".
	if !strings.Contains(err.Error(), "workspace_symbols") {
		t.Errorf("expected the redirect to workspace_symbols, got: %v", err)
	}
}

// TestFindSymbol_URINotSchemaRequired guards Fix 4b: uri must NOT be a
// schema-required parameter, so a call omitting it reaches Execute's friendly
// redirect rather than being rejected server-side with "missing required
// parameter \"uri\"". query stays required.
func TestFindSymbol_URINotSchemaRequired(t *testing.T) {
	tool := tools.NewFindSymbol(&mockLSP{}, nil, time.Minute, 0)
	var schema map[string]any
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("inputSchema is not valid JSON: %v", err)
	}
	required, _ := schema["required"].([]any)
	hasQuery := false
	for _, r := range required {
		if r == "uri" {
			t.Errorf("uri must not be schema-required (blocks the workspace_symbols redirect), got: %v", required)
		}
		if r == "query" {
			hasQuery = true
		}
	}
	if !hasQuery {
		t.Errorf("query must stay schema-required, got: %v", required)
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
}
