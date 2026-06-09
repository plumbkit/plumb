package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestSearchInFiles_IncludeEnclosingSymbol(t *testing.T) {
	dir := t.TempDir()
	src := `package main

func Foo() {
	x := 1
	_ = x
}

func Bar() {
	y := 2
	_ = y
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	// DocumentSymbols response: Foo covers lines 2-5, Bar covers lines 7-10 (0-based).
	mock := &mockLSP{
		docSymbols: []protocol.DocumentSymbol{
			{
				Name: "Foo",
				Kind: protocol.SKFunction,
				Range: protocol.Range{
					Start: protocol.Position{Line: 2, Character: 0},
					End:   protocol.Position{Line: 5, Character: 1},
				},
			},
			{
				Name: "Bar",
				Kind: protocol.SKFunction,
				Range: protocol.Range{
					Start: protocol.Position{Line: 7, Character: 0},
					End:   protocol.Position{Line: 10, Character: 1},
				},
			},
		},
	}

	tool := tools.NewSearchInFiles(func() string { return dir }, mock, nil, 0)

	args, _ := json.Marshal(map[string]any{
		"pattern":                  "x := 1",
		"path":                     dir,
		"include_enclosing_symbol": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The match is on line 4 (1-based), which is inside Foo (lines 3-6 1-based).
	if !strings.Contains(out, "[in: Foo") {
		t.Errorf("expected enclosing symbol annotation '[in: Foo'; got:\n%s", out)
	}
}

func TestSearchInFiles_IncludeEnclosingSymbol_NilClient(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("package p\nfunc F() { x := 1\n_ = x\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No client — feature silently disabled.
	tool := tools.NewSearchInFiles(func() string { return dir }, nil, nil, 0)

	args, _ := json.Marshal(map[string]any{
		"pattern":                  "x := 1",
		"path":                     dir,
		"include_enclosing_symbol": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(out, "[in:") {
		t.Errorf("expected no annotation when client is nil; got:\n%s", out)
	}
}
