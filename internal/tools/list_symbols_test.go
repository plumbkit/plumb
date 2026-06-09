package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
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
	tool := tools.NewListSymbols(mock, nil, 0, 0)
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
	tool := tools.NewListSymbols(mock, nil, 0, 0)
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
	tool := tools.NewListSymbols(&mockLSP{}, nil, 0, 0)
	raw, _ := json.Marshal(map[string]any{})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing uri")
	}
}

func TestListSymbols_IncludeSignatures(t *testing.T) {
	f, err := os.CreateTemp("", "list_symbols_test_*.go")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	content := "package p\n\nfunc Greet(name string) string {\n\treturn name\n}\n"
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Greet",
				Kind: protocol.SKFunction,
				Range: protocol.Range{
					Start: protocol.Position{Line: 2},
					End:   protocol.Position{Line: 4},
				},
			},
		},
	}
	tool := tools.NewListSymbols(mock, nil, 0, 0)
	raw, _ := json.Marshal(map[string]any{
		"uri":                "file://" + f.Name(),
		"include_signatures": true,
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	if !strings.Contains(out, "→ func Greet(name string) string {") {
		t.Errorf("expected signature in output, got:\n%s", out)
	}
}

func TestListSymbols_IncludeSignatures_NonCallableKinds(t *testing.T) {
	f, err := os.CreateTemp("", "list_symbols_test_*.go")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	// Line 0: package; Line 1: blank; Line 2: type Foo struct; Line 3: field; Line 4: closing brace
	content := "package p\n\ntype Foo struct {\n\tBar string\n}\n"
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Foo",
				Kind: protocol.SKStruct,
				Range: protocol.Range{
					Start: protocol.Position{Line: 2},
					End:   protocol.Position{Line: 4},
				},
				Children: []protocol.DocumentSymbol{
					{
						Name: "Bar",
						Kind: protocol.SKField,
						Range: protocol.Range{
							Start: protocol.Position{Line: 3},
							End:   protocol.Position{Line: 3},
						},
					},
				},
			},
		},
	}
	tool := tools.NewListSymbols(mock, nil, 0, 0)
	raw, _ := json.Marshal(map[string]any{
		"uri":                "file://" + f.Name(),
		"include_signatures": true,
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	if strings.Contains(out, "→") {
		t.Errorf("struct/field symbols must not get a → signature annotation; got:\n%s", out)
	}
}

func TestListSymbols_IncludeSignatures_SkipsCommentLines(t *testing.T) {
	f, err := os.CreateTemp("", "list_symbols_test_*.go")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	// Simulate a case where the LSP reports start_line pointing at a comment.
	// Line 0: package; Line 1: blank; Line 2: // comment; Line 3: func Greet...
	content := "package p\n\n// Greet says hello.\nfunc Greet(name string) string {\n\treturn name\n}\n"
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Greet",
				Kind: protocol.SKFunction,
				// start_line=2 (the comment line) — edge case: LSP places range at comment
				Range: protocol.Range{
					Start: protocol.Position{Line: 2},
					End:   protocol.Position{Line: 5},
				},
			},
		},
	}
	tool := tools.NewListSymbols(mock, nil, 0, 0)
	raw, _ := json.Marshal(map[string]any{
		"uri":                "file://" + f.Name(),
		"include_signatures": true,
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("list_symbols: %v", err)
	}
	if strings.Contains(out, "→ //") {
		t.Errorf("comment lines must not be emitted as signatures; got:\n%s", out)
	}
}

func TestListSymbols_Interface(t *testing.T) {
	var _ interface {
		Name() string
		Description() string
		InputSchema() json.RawMessage
		Execute(context.Context, json.RawMessage) (string, error)
	} = tools.NewListSymbols(&mockLSP{}, nil, 0, 0)
}
