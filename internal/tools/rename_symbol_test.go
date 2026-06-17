package tools_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestRenameSymbol_StaleIndexError(t *testing.T) {
	// Write a small file to a temp dir.
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package foo\n\nfunc Foo() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The LSP returns a WorkspaceEdit whose edit positions are past the end of
	// the file, simulating a stale position index after an in-session edit.
	we := &protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file://" + path: {
				{
					Range: protocol.Range{
						Start: protocol.Position{Line: 999, Character: 0},
						End:   protocol.Position{Line: 999, Character: 3},
					},
					NewText: "Bar",
				},
			},
		},
	}

	mock := &mockLSP{renameResult: we}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri":       "file://" + path,
		"line":      2,
		"character": 5,
		"new_name":  "Bar",
		"dry_run":   false,
	})

	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "position index is stale") {
		t.Errorf("expected stale-index hint in error; got: %s", msg)
	}
	if !strings.Contains(msg, "find_replace") {
		t.Errorf("expected find_replace fallback suggestion in error; got: %s", msg)
	}
}

func TestRenameSymbol_EmptyEditSet(t *testing.T) {
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{}}
	tool := tools.NewRenameSymbol(mock, 0)

	args, _ := json.Marshal(map[string]any{
		"uri":       "file:///any.go",
		"line":      0,
		"character": 0,
		"new_name":  "NewName",
	})

	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "empty edit set") {
		t.Errorf("expected empty-edit-set message; got: %s", out)
	}
}

func TestRenameSymbol_LSPError_ActionableHint(t *testing.T) {
	mock := &mockLSP{err: errors.New("server exploded")}
	tool := tools.NewRenameSymbol(mock, 0)
	args, _ := json.Marshal(map[string]any{
		"uri": "file:///nope.go", "line": 0, "character": 0, "new_name": "X",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "could not compute this rename") {
		t.Errorf("missing actionable hint: %s", msg)
	}
	if !strings.Contains(msg, "find_references") {
		t.Errorf("missing find_references fallback: %s", msg)
	}
	// structural_fallback must not be suggested when the fallback is not wired.
	if strings.Contains(msg, "structural_fallback") {
		t.Errorf("should not suggest structural_fallback when unwired: %s", msg)
	}
}

func TestRenameSymbol_LSPError_SuggestsStructuralWhenWired(t *testing.T) {
	mock := &mockLSP{err: errors.New("server exploded")}
	tool := tools.NewRenameSymbol(mock, 0).WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file:///nope.go", "line": 0, "character": 0, "new_name": "X",
	})
	_, err := tool.Execute(context.Background(), args)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "structural_fallback:true") {
		t.Errorf("expected structural_fallback suggestion: %s", err.Error())
	}
}

func TestRenameSymbol_StructuralFallback_DryRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	src := "package p\n\nfunc Foo() {}\nvar x = Foo\nvar FooBar = 1\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{err: errors.New("no index")}
	tool := tools.NewRenameSymbol(mock, 0).
		WithWorkspace(func() string { return dir }).
		WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Renamed",
		"structural_fallback": true, // dry_run defaults true
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(out, "STRUCTURAL FALLBACK") || !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected fallback dry-run banner: %s", out)
	}
	if b, _ := os.ReadFile(path); string(b) != src {
		t.Errorf("dry run must not modify the file; got: %s", b)
	}
}

func TestRenameSymbol_StructuralFallback_Applies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	src := "package p\n\nfunc Foo() {}\nvar x = Foo\nvar FooBar = 1\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{err: errors.New("no index")}
	tool := tools.NewRenameSymbol(mock, 0).
		WithWorkspace(func() string { return dir }).
		WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 2, "character": 5, "new_name": "Renamed",
		"structural_fallback": true, "dry_run": false,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	got, _ := os.ReadFile(path)
	gs := string(got)
	if !strings.Contains(gs, "func Renamed()") || !strings.Contains(gs, "var x = Renamed") {
		t.Errorf("expected Foo->Renamed: %s", gs)
	}
	// Word boundary: FooBar shares a prefix but must NOT be renamed.
	if !strings.Contains(gs, "var FooBar = 1") {
		t.Errorf("word-boundary violated, FooBar changed: %s", gs)
	}
}

func TestRenameSymbol_StructuralFallback_OnEmptyEditSet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.go")
	if err := os.WriteFile(path, []byte("package p\nvar Foo = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{}} // empty edit set
	tool := tools.NewRenameSymbol(mock, 0).
		WithWorkspace(func() string { return dir }).
		WithStructuralFallback(tools.WriteDeps{})
	args, _ := json.Marshal(map[string]any{
		"uri": "file://" + path, "line": 1, "character": 4, "new_name": "Bar",
		"structural_fallback": true,
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(out, "STRUCTURAL FALLBACK") {
		t.Errorf("expected fallback on empty edit set: %s", out)
	}
}
