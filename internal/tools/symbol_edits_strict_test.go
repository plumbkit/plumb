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

// Under [edits] strict = true a symbol edit carries agent-authored content into a
// file, exactly as edit_file does, so it must obey the same read-before-write
// contract. Before this gate, strict mode could be sidestepped entirely by
// reaching for replace_symbol_body instead of edit_file.

func strictDeps(reads *tools.ReadTracker) tools.WriteDeps {
	return tools.WriteDeps{Reads: reads, Strict: func() bool { return true }}
}

func replaceBodyArgs(uri string) json.RawMessage {
	dryRun := false
	args, _ := json.Marshal(map[string]any{
		"uri": uri, "name_path": "Foo", "content": "func Foo() { return }", "dry_run": &dryRun,
	})
	return args
}

// readFixture records a read_file of path on reads, satisfying strict mode.
func readFixture(t *testing.T, reads *tools.ReadTracker, path string) {
	t.Helper()
	args, _ := json.Marshal(map[string]any{"file_path": path})
	if _, err := tools.NewReadFile(reads).Execute(context.Background(), args); err != nil {
		t.Fatalf("read_file: %v", err)
	}
}

func TestReplaceSymbolBody_StrictMode_RequiresRead(t *testing.T) {
	src := "package p\n\nfunc Foo() {}\n"
	path, uri := writeFixture(t, "main.go", src)
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0).WithWriteDeps(strictDeps(tools.NewReadTracker()))

	_, err := tool.Execute(context.Background(), replaceBodyArgs(uri))
	if err == nil {
		t.Fatal("strict mode must refuse a symbol edit to a file this session never read")
	}
	if !strings.Contains(err.Error(), "has not been read in this daemon session") {
		t.Errorf("unexpected error: %v", err)
	}
	if b, _ := os.ReadFile(path); string(b) != src {
		t.Errorf("the refused edit must not have touched the file; got: %q", b)
	}
}

func TestReplaceSymbolBody_StrictMode_AfterRead(t *testing.T) {
	path, uri := writeFixture(t, "main.go", "package p\n\nfunc Foo() {}\n")
	reads := tools.NewReadTracker()
	readFixture(t, reads, path)
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0).WithWriteDeps(strictDeps(reads))

	if _, err := tool.Execute(context.Background(), replaceBodyArgs(uri)); err != nil {
		t.Fatalf("a read file must be editable under strict mode: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "func Foo() { return }") {
		t.Errorf("edit did not apply: %s", b)
	}
}

// A read in one session must not satisfy strict mode for a semantic edit issued
// on another — the read tracker is per MCP connection.
func TestReplaceSymbolBody_StrictMode_TrackerIsolation(t *testing.T) {
	path, uri := writeFixture(t, "main.go", "package p\n\nfunc Foo() {}\n")
	sessionA, sessionB := tools.NewReadTracker(), tools.NewReadTracker()
	readFixture(t, sessionA, path)

	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0).WithWriteDeps(strictDeps(sessionB))

	if _, err := tool.Execute(context.Background(), replaceBodyArgs(uri)); err == nil {
		t.Fatal("another session's read must not satisfy strict mode")
	}
}

// A dry run authors nothing, so it stays readable under strict mode.
func TestReplaceSymbolBody_StrictMode_DryRunAllowed(t *testing.T) {
	_, uri := writeFixture(t, "main.go", "package p\n\nfunc Foo() {}\n")
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{symbolAt("Foo", 2, 2, 13)}}
	tool := tools.NewReplaceSymbolBody(mock, 0).WithWriteDeps(strictDeps(tools.NewReadTracker()))

	args, _ := json.Marshal(map[string]any{
		"uri": uri, "name_path": "Foo", "content": "func Foo() { return }",
	})
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("a dry run must not be gated by strict mode: %v", err)
	}
	if !strings.Contains(out, "DRY RUN") {
		t.Errorf("expected a dry-run preview, got:\n%s", out)
	}
}

// rename_symbol is the deliberate strict-mode exemption: it authors no content
// and the language server, not the agent, chooses the files. Requiring a prior
// read of every renamed file would make the tool unusable under strict mode.
func TestRenameSymbol_StrictMode_Exempt(t *testing.T) {
	path, uri := writeFixture(t, "main.go", "package p\n\nvar Foo = 1\n")
	mock := &mockLSP{renameResult: &protocol.WorkspaceEdit{Changes: map[string][]protocol.TextEdit{
		uri: {{
			Range:   protocol.Range{Start: protocol.Position{Line: 2, Character: 4}, End: protocol.Position{Line: 2, Character: 7}},
			NewText: "Bar",
		}},
	}}}
	tool := tools.NewRenameSymbol(mock, 0).WithWriteDeps(strictDeps(tools.NewReadTracker()))

	args, _ := json.Marshal(map[string]any{
		"uri": uri, "line": 2, "character": 4, "new_name": "Bar", "dry_run": false,
	})
	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("rename_symbol must stay usable under strict mode: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "var Bar = 1") {
		t.Errorf("rename did not apply: %s", b)
	}
}
