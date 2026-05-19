package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/tools"
)

func TestReadSymbol_SingleMatch(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

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
	tool := tools.NewReadSymbol(mock, nil, time.Minute, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Greet"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	for _, want := range []string{"plumb-read", "Greet", "Function", "return"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestReadSymbol_DottedName(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\ntype Server struct{}\n\nfunc (s *Server) Start() {}\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Server",
				Kind: protocol.SKStruct,
				Range: protocol.Range{
					Start: protocol.Position{Line: 2},
					End:   protocol.Position{Line: 2},
				},
				Children: []protocol.DocumentSymbol{
					{
						Name: "Start",
						Kind: protocol.SKMethod,
						Range: protocol.Range{
							Start: protocol.Position{Line: 4},
							End:   protocol.Position{Line: 4},
						},
					},
				},
			},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Server.Start"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "Start") {
		t.Errorf("expected Start in output:\n%s", out)
	}
	if !strings.Contains(out, "Start()") {
		t.Errorf("expected source line in output:\n%s", out)
	}
}

func TestReadSymbol_MultipleMatches(t *testing.T) {
	dir := t.TempDir()
	src := "package p\n\nfunc Run() {}\n\nfunc Run() error { return nil }\n"
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{Name: "Run", Kind: protocol.SKFunction, Range: protocol.Range{
				Start: protocol.Position{Line: 2}, End: protocol.Position{Line: 2},
			}},
			{Name: "Run", Kind: protocol.SKFunction, Range: protocol.Range{
				Start: protocol.Position{Line: 4}, End: protocol.Position{Line: 4},
			}},
		},
	}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Run"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("read_symbol: %v", err)
	}
	if !strings.Contains(out, "2 matches") {
		t.Errorf("expected '2 matches' for ambiguous name:\n%s", out)
	}
}

func TestReadSymbol_NotFound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.go")
	if err := os.WriteFile(path, []byte("package p\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	mock := &mockLSP{docSymbols: nil}
	tool := tools.NewReadSymbol(mock, nil, time.Minute, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": path, "name": "Missing"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "No symbol") {
		t.Errorf("expected no-symbol message:\n%s", out)
	}
}

func TestReadSymbol_MissingPath(t *testing.T) {
	tool := tools.NewReadSymbol(&mockLSP{}, nil, time.Minute, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"name": "Greet"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing path")
	}
}

func TestReadSymbol_MissingName(t *testing.T) {
	tool := tools.NewReadSymbol(&mockLSP{}, nil, time.Minute, tools.NewReadTracker())
	raw, _ := json.Marshal(map[string]any{"path": "/some/file.go"})
	_, err := tool.Execute(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}
