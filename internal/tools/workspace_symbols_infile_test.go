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

// The uri-scoped half of workspace_symbols — the single-document search
// find_symbol used to own before the merge. Its distinguishing behaviours are
// the documentSymbol engine (a case-insensitive substring filter walking the
// whole tree, nested children included), the "in <uri>" output shape, and uri
// being OPTIONAL so one tool covers both scopes.

func inFileArgs(t *testing.T, query, uri string) json.RawMessage {
	t.Helper()
	args, err := json.Marshal(map[string]any{"query": query, "uri": uri})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return args
}

// TestWorkspaceSymbols_InFileSearch pins the file-scoped engine: matching is a
// case-insensitive substring against the symbol name and the walk descends into
// children, so a query hits both the enclosing type and its nested method.
func TestWorkspaceSymbols_InFileSearch(t *testing.T) {
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
	tool := tools.NewWorkspaceSymbols(mock, nil, time.Minute, 0, nil)

	result, err := tool.Execute(context.Background(), inFileArgs(t, "greet", "file:///p/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Greeter", "greet", "file:///p/main.go", "line 8"} {
		if !strings.Contains(result, want) {
			t.Errorf("expected %q in the in-file result:\n%s", want, result)
		}
	}
}

func TestWorkspaceSymbols_InFileSearch_NoMatch(t *testing.T) {
	mock := &mockLSP{docSymbols: []protocol.DocumentSymbol{{Name: "Greeter", Kind: protocol.SKClass}}}
	tool := tools.NewWorkspaceSymbols(mock, nil, time.Minute, 0, nil)

	result, err := tool.Execute(context.Background(), inFileArgs(t, "Xyz", "file:///p/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "No symbols matching") {
		t.Errorf("expected the in-file no-match message, got: %s", result)
	}
}

// TestWorkspaceSymbols_InFileLSPError: with no topology store wired there is
// nothing to fall back to, so the language-server error surfaces.
func TestWorkspaceSymbols_InFileLSPError(t *testing.T) {
	tool := tools.NewWorkspaceSymbols(&mockLSP{err: errors.New("lsp unavailable")}, nil, time.Minute, 0, nil)

	if _, err := tool.Execute(context.Background(), inFileArgs(t, "Greeter", "file:///p/main.go")); err == nil {
		t.Fatal("expected an error when the LSP fails and no topology store is wired")
	}
}

// TestWorkspaceSymbols_URIOptional is the merge's contract at the schema level:
// uri must NOT be required, because omitting it selects the workspace-wide
// search — the mode find_symbol could only redirect callers to.
func TestWorkspaceSymbols_URIOptional(t *testing.T) {
	tool := tools.NewWorkspaceSymbols(&mockLSP{}, nil, time.Minute, 0, nil)
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(tool.InputSchema(), &schema); err != nil {
		t.Fatalf("inputSchema is not valid JSON: %v", err)
	}
	if _, ok := schema.Properties["uri"]; !ok {
		t.Error("workspace_symbols must declare a uri property for the file-scoped mode")
	}
	hasQuery := false
	for _, r := range schema.Required {
		if r == "uri" {
			t.Errorf("uri must stay optional — omitting it selects the workspace-wide search; required = %v", schema.Required)
		}
		if r == "query" {
			hasQuery = true
		}
	}
	if !hasQuery {
		t.Errorf("query must stay schema-required, got: %v", schema.Required)
	}
}

// TestWorkspaceSymbols_ModeSelectedByURI proves one tool serves both engines off
// the same arguments: the uri-less call runs the workspace/symbol query, the
// uri-bearing call runs documentSymbol on that file.
func TestWorkspaceSymbols_ModeSelectedByURI(t *testing.T) {
	mock := &mockLSP{
		wsSymbols: []protocol.SymbolInformation{{
			Name: "Greeter", Kind: protocol.SKClass,
			Location: protocol.Location{URI: "file:///p/other.go"},
		}},
		docSymbols: []protocol.DocumentSymbol{{Name: "Greeter", Kind: protocol.SKClass}},
	}
	tool := tools.NewWorkspaceSymbols(mock, nil, time.Minute, 0, nil)

	wide, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"Greeter"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wide, "Found 1 symbol(s)") || !strings.Contains(wide, "file:///p/other.go") {
		t.Errorf("a uri-less call must run the workspace-wide search, got:\n%s", wide)
	}

	scoped, err := tool.Execute(context.Background(), inFileArgs(t, "Greeter", "file:///p/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(scoped, "Symbols matching \"Greeter\" in file:///p/main.go") {
		t.Errorf("a uri-bearing call must run the file-scoped search, got:\n%s", scoped)
	}
}
