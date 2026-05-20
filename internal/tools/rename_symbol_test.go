package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/tools"
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
	tool := tools.NewRenameSymbol(mock)

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
	tool := tools.NewRenameSymbol(mock)

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
