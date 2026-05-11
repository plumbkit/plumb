package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/tools"
)

func TestFindReferences_Found(t *testing.T) {
	mock := &mockLSP{
		locations: []protocol.Location{
			{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 4, Character: 2}}},
			{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 9, Character: 8}}},
		},
	}
	tool := tools.NewFindReferences(mock)
	raw, _ := json.Marshal(map[string]any{
		"uri":       "file:///p/main.go",
		"line":      4,
		"character": 2,
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("find_references: %v", err)
	}
	if !strings.Contains(out, "2 reference") {
		t.Errorf("expected 2 references in output, got: %q", out)
	}
	if !strings.Contains(out, "main.go") {
		t.Errorf("expected file path in output, got: %q", out)
	}
}

func TestFindReferences_None(t *testing.T) {
	mock := &mockLSP{locations: nil}
	tool := tools.NewFindReferences(mock)
	raw, _ := json.Marshal(map[string]any{
		"uri":       "file:///p/main.go",
		"line":      0,
		"character": 0,
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("find_references: %v", err)
	}
	if !strings.Contains(out, "No references") {
		t.Errorf("expected no-references message, got: %q", out)
	}
}

func TestFindReferences_MissingURI(t *testing.T) {
	tool := tools.NewFindReferences(&mockLSP{})
	raw, _ := json.Marshal(map[string]any{"line": 0, "character": 0})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing uri")
	}
}

func TestFindReferences_LSPError(t *testing.T) {
	mock := &mockLSP{err: errors.New("lsp error")}
	tool := tools.NewFindReferences(mock)
	raw, _ := json.Marshal(map[string]any{
		"uri":       "file:///p/main.go",
		"line":      0,
		"character": 0,
	})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error from LSP")
	}
}
