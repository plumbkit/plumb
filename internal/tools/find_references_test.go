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

func TestFindReferences_Found(t *testing.T) {
	mock := &mockLSP{
		locations: []protocol.Location{
			{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 4, Character: 2}}},
			{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 9, Character: 8}}},
		},
	}
	tool := tools.NewFindReferences(mock, nil, time.Minute)
	line, char := uint32(4), uint32(2)
	raw, _ := json.Marshal(map[string]any{
		"uri":       "file:///p/main.go",
		"line":      line,
		"character": char,
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
	tool := tools.NewFindReferences(mock, nil, time.Minute)
	zero := uint32(0)
	raw, _ := json.Marshal(map[string]any{
		"uri":       "file:///p/main.go",
		"line":      zero,
		"character": zero,
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
	tool := tools.NewFindReferences(&mockLSP{}, nil, time.Minute)
	raw, _ := json.Marshal(map[string]any{"line": 0, "character": 0})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing uri")
	}
}

func TestFindReferences_LSPError(t *testing.T) {
	mock := &mockLSP{err: errors.New("lsp error")}
	tool := tools.NewFindReferences(mock, nil, time.Minute)
	zero := uint32(0)
	raw, _ := json.Marshal(map[string]any{
		"uri":       "file:///p/main.go",
		"line":      zero,
		"character": zero,
	})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error from LSP")
	}
}

func TestFindReferences_NeitherNameNorPosition(t *testing.T) {
	tool := tools.NewFindReferences(&mockLSP{}, nil, time.Minute)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error when neither symbol_name nor line+character provided")
	}
	if !strings.Contains(err.Error(), "symbol_name") {
		t.Errorf("error should mention symbol_name: %v", err)
	}
}

func TestFindReferences_ByName_SingleMatch(t *testing.T) {
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Greet",
				Kind: protocol.SKFunction,
				Range: protocol.Range{
					Start: protocol.Position{Line: 5, Character: 0},
					End:   protocol.Position{Line: 8},
				},
			},
		},
		locations: []protocol.Location{
			{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 10, Character: 2}}},
		},
	}
	tool := tools.NewFindReferences(mock, nil, time.Minute)
	raw, _ := json.Marshal(map[string]any{
		"uri":         "file:///p/main.go",
		"symbol_name": "Greet",
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("find_references: %v", err)
	}
	if !strings.Contains(out, "reference") {
		t.Errorf("expected references in output, got: %q", out)
	}
}

func TestFindReferences_ByName_NotFound(t *testing.T) {
	mock := &mockLSP{docSymbols: nil}
	tool := tools.NewFindReferences(mock, nil, time.Minute)
	raw, _ := json.Marshal(map[string]any{
		"uri":         "file:///p/main.go",
		"symbol_name": "Missing",
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No symbol") {
		t.Errorf("expected no-symbol message, got: %q", out)
	}
}

func TestFindReferences_ByName_MultipleMatches(t *testing.T) {
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "Run", Kind: protocol.SKFunction, Range: protocol.Range{
				Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 4},
			}},
			{Name: "Run", Kind: protocol.SKMethod, Range: protocol.Range{
				Start: protocol.Position{Line: 10}, End: protocol.Position{Line: 12},
			}},
		},
		locations: []protocol.Location{
			{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 20}}},
		},
	}
	tool := tools.NewFindReferences(mock, nil, time.Minute)
	raw, _ := json.Marshal(map[string]any{
		"uri":         "file:///p/main.go",
		"symbol_name": "Run",
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("find_references: %v", err)
	}
	if !strings.Contains(out, "2 symbol matches") {
		t.Errorf("expected ambiguous-match header, got: %q", out)
	}
}
