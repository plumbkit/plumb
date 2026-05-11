package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/tools"
)

func TestListSymbols_Full(t *testing.T) {
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name:   "Greeter",
				Kind:   protocol.SKStruct,
				Detail: "",
				Range:  protocol.Range{Start: protocol.Position{Line: 5}, End: protocol.Position{Line: 9}},
				Children: []protocol.DocumentSymbol{
					{
						Name:  "Prefix",
						Kind:  protocol.SKField,
						Range: protocol.Range{Start: protocol.Position{Line: 6}, End: protocol.Position{Line: 6}},
					},
				},
			},
			{
				Name:   "Greet",
				Detail: "(name string) string",
				Kind:   protocol.SKMethod,
				Range:  protocol.Range{Start: protocol.Position{Line: 11}, End: protocol.Position{Line: 13}},
			},
		},
	}
	tool := tools.NewListSymbols(mock, nil, 0)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	for _, want := range []string{"Greeter", "Struct", "line", "Prefix", "Field", "Greet", "Method"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestListSymbols_Empty(t *testing.T) {
	mock := &mockLSP{}
	tool := tools.NewListSymbols(mock, nil, 0)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/empty.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	if !strings.Contains(out, "No symbols") {
		t.Errorf("expected no-symbols message, got: %q", out)
	}
}

func TestListSymbols_MissingURI(t *testing.T) {
	tool := tools.NewListSymbols(&mockLSP{}, nil, 0)
	raw, _ := json.Marshal(map[string]any{})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing uri")
	}
}

func TestListSymbols_Interface(t *testing.T) {
	var _ interface {
		Name() string
		Description() string
		InputSchema() json.RawMessage
		Execute(context.Context, json.RawMessage) (string, error)
	} = tools.NewListSymbols(&mockLSP{}, nil, 0)
}
