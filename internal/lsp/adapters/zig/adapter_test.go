package zig_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// initResult is a canned Initialize response.
var initResult = protocol.InitializeResult{
	Capabilities: protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: true,
			Change:    protocol.SyncFull,
		},
		HoverProvider:          &protocol.BoolOrOptions{Enabled: true},
		DefinitionProvider:     &protocol.BoolOrOptions{Enabled: true},
		ReferencesProvider:     &protocol.BoolOrOptions{Enabled: true},
		DocumentSymbolProvider: &protocol.BoolOrOptions{Enabled: true},
	},
	ServerInfo: &protocol.ServerInfo{Name: "zls", Version: "0.13.0"},
}

// newAdapter sets up a MockCaller with sensible defaults and returns the adapter.
func newAdapter(t *testing.T) (*zig.Adapter, *jsonrpc.MockCaller) {
	t.Helper()
	mock := jsonrpc.NewMockCaller()
	mock.HandleOK(protocol.MethodInitialize, initResult)
	mock.Handle(protocol.MethodInitialized, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodShutdown, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodExit, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodDidOpen, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodDidClose, func(_ json.RawMessage) (any, error) { return nil, nil })
	return zig.New(mock), mock
}

// writeTempZig writes content to a temp .zig file and returns its file:// URI,
// so ensureOpen can read the document from disk before a query.
func writeTempZig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "main.zig")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp zig: %v", err)
	}
	return "file://" + path
}

func TestAdapter_Initialize(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	result, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///project"))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ServerInfo == nil || result.ServerInfo.Name != "zls" {
		t.Fatalf("unexpected server info: %v", result.ServerInfo)
	}
	caps := ad.Capabilities()
	if caps == nil {
		t.Fatal("expected capabilities to be stored after Initialize")
	}
	if caps.HoverProvider == nil || !caps.HoverProvider.Enabled {
		t.Fatal("expected hover to be enabled")
	}

	calls := mock.Calls()
	if len(calls) != 1 || calls[0].Method != protocol.MethodInitialize {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestAdapter_Initialized(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///project")); err != nil {
		t.Fatal(err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatalf("Initialized: %v", err)
	}
	var found bool
	for _, c := range mock.Calls() {
		if c.Method == protocol.MethodInitialized {
			found = true
		}
	}
	if !found {
		t.Fatal("initialized notification not sent")
	}
}

func TestAdapter_DidChangeWatchedFiles(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()
	mock.Handle(protocol.MethodDidChangeWatchedFiles, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: "file:///p/src/main.zig", Type: protocol.FileChanged},
			{URI: "file:///p/src/new.zig", Type: protocol.FileCreated},
		},
	})
	if err != nil {
		t.Fatalf("DidChangeWatchedFiles: %v", err)
	}
	var found bool
	for _, c := range mock.Calls() {
		if c.Method == protocol.MethodDidChangeWatchedFiles {
			found = true
			var got protocol.DidChangeWatchedFilesParams
			if err := json.Unmarshal(c.Params, &got); err != nil {
				t.Fatalf("unmarshal params: %v", err)
			}
			if len(got.Changes) != 2 {
				t.Fatalf("expected 2 changes, got %d", len(got.Changes))
			}
			if got.Changes[0].Type != protocol.FileChanged {
				t.Errorf("change[0].type = %d, want FileChanged(2)", got.Changes[0].Type)
			}
		}
	}
	if !found {
		t.Fatal("didChangeWatchedFiles notification not sent")
	}
}

func TestAdapter_DidOpenDidClose(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()
	mock.Handle(protocol.MethodDidOpen, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodDidClose, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: "file:///p/src/main.zig", LanguageID: "zig", Version: 1, Text: "const x = 1;\n",
		},
	}); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	if err := ad.DidClose(ctx, protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/src/main.zig"},
	}); err != nil {
		t.Fatalf("DidClose: %v", err)
	}
}

func TestAdapter_DocumentSymbols(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := []protocol.DocumentSymbol{
		{Name: "Greeter", Kind: protocol.SKStruct, Range: protocol.Range{}},
		{Name: "greet", Kind: protocol.SKFunction, Range: protocol.Range{}},
	}
	mock.HandleOK(protocol.MethodDocumentSymbols, expected)

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempZig(t, "const Greeter = struct {};\n")
	syms, err := ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) != len(expected) {
		t.Fatalf("got %d symbols, want %d", len(syms), len(expected))
	}
	if syms[0].Name != "Greeter" {
		t.Fatalf("first symbol: got %q, want %q", syms[0].Name, "Greeter")
	}
}

func TestAdapter_WorkspaceSymbols(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := []protocol.SymbolInformation{
		{Name: "Greeter", Kind: protocol.SKStruct, Location: protocol.Location{URI: "file:///p/src/main.zig"}},
	}
	mock.HandleOK(protocol.MethodWorkspaceSymbols, expected)

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	syms, err := ad.WorkspaceSymbols(ctx, protocol.WorkspaceSymbolParams{Query: "Greet"})
	if err != nil {
		t.Fatalf("WorkspaceSymbols: %v", err)
	}
	if len(syms) != 1 || syms[0].Name != "Greeter" {
		t.Fatalf("unexpected symbols: %v", syms)
	}
}

func TestAdapter_Definition(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := []protocol.Location{
		{URI: "file:///p/src/main.zig", Range: protocol.Range{Start: protocol.Position{Line: 3}}},
	}
	mock.HandleOK(protocol.MethodDefinition, expected)

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempZig(t, "const Greeter = struct {};\n")
	locs, err := ad.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 12, Character: 4},
	})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d locations, want 1", len(locs))
	}
}

func TestAdapter_References(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := []protocol.Location{
		{URI: "file:///p/src/main.zig", Range: protocol.Range{Start: protocol.Position{Line: 10}}},
		{URI: "file:///p/src/main.zig", Range: protocol.Range{Start: protocol.Position{Line: 14}}},
	}
	mock.HandleOK(protocol.MethodReferences, expected)

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempZig(t, "const Greeter = struct {};\n")
	refs, err := ad.References(ctx, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 3, Character: 6},
		Context:      protocol.ReferenceContext{IncludeDeclaration: true},
	})
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want 2", len(refs))
	}
}

func TestAdapter_Hover(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := protocol.Hover{
		Contents: protocol.MarkupContent{Kind: "markdown", Value: "```zig\nconst Greeter = struct\n```"},
	}
	mock.HandleOK(protocol.MethodHover, expected)

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempZig(t, "const Greeter = struct {};\n")
	hover, err := ad.Hover(ctx, protocol.HoverParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 3, Character: 6},
	})
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if hover == nil {
		t.Fatal("expected non-nil hover")
	}
	if hover.Contents.Kind != "markdown" {
		t.Fatalf("got kind %q, want markdown", hover.Contents.Kind)
	}
}

func TestAdapter_Rename(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file:///p/src/main.zig": {
				{Range: protocol.Range{Start: protocol.Position{Line: 3}}, NewText: "Welcomer"},
			},
		},
	}
	mock.HandleOK(protocol.MethodPrepareRename, protocol.PrepareRenameResult{
		Range:       protocol.Range{Start: protocol.Position{Line: 3}},
		Placeholder: "Greeter",
	})
	mock.HandleOK(protocol.MethodRename, expected)

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempZig(t, "const Greeter = struct {};\n")
	prep, err := ad.PrepareRename(ctx, protocol.PrepareRenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 3, Character: 6},
	})
	if err != nil {
		t.Fatalf("PrepareRename: %v", err)
	}
	if prep.Placeholder != "Greeter" {
		t.Fatalf("got placeholder %q, want Greeter", prep.Placeholder)
	}

	edit, err := ad.Rename(ctx, protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 3, Character: 6},
		NewName:      "Welcomer",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}
	if len(edit.Changes["file:///p/src/main.zig"]) != 1 {
		t.Fatalf("unexpected edit: %v", edit)
	}
}

// TestAdapter_EnsureOpenBeforeQuery verifies the adapter sends textDocument/
// didOpen before its first per-document query and caches it (one open across
// repeated queries) — zls resolves nothing on an unopened file.
func TestAdapter_EnsureOpenBeforeQuery(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()
	mock.HandleOK(protocol.MethodDocumentSymbols, []protocol.DocumentSymbol{{Name: "Greeter"}})
	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	uri := writeTempZig(t, "const Greeter = struct {};\n")
	q := protocol.DocumentSymbolParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}}
	for range 2 {
		if _, err := ad.DocumentSymbols(ctx, q); err != nil {
			t.Fatalf("DocumentSymbols: %v", err)
		}
	}
	var firstOpen, firstSym, opens int
	for i, c := range mock.Calls() {
		switch c.Method {
		case protocol.MethodDidOpen:
			opens++
			if firstOpen == 0 {
				firstOpen = i + 1
			}
		case protocol.MethodDocumentSymbols:
			if firstSym == 0 {
				firstSym = i + 1
			}
		}
	}
	if opens != 1 {
		t.Fatalf("expected exactly 1 didOpen (cached), got %d", opens)
	}
	if firstOpen == 0 || firstSym == 0 || firstOpen > firstSym {
		t.Fatalf("didOpen (idx %d) must precede documentSymbol (idx %d)", firstOpen, firstSym)
	}
}

// TestAdapter_RefreshOpenReopensAfterWatchedChange verifies a watched-file change
// to an open document closes it so the next query reopens it with fresh content.
func TestAdapter_RefreshOpenReopensAfterWatchedChange(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()
	mock.HandleOK(protocol.MethodDocumentSymbols, []protocol.DocumentSymbol{{Name: "Greeter"}})
	mock.Handle(protocol.MethodDidChangeWatchedFiles, func(_ json.RawMessage) (any, error) { return nil, nil })
	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	uri := writeTempZig(t, "const Greeter = struct {};\n")
	q := protocol.DocumentSymbolParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}}
	if _, err := ad.DocumentSymbols(ctx, q); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri, Type: protocol.FileChanged}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ad.DocumentSymbols(ctx, q); err != nil {
		t.Fatal(err)
	}
	var opens, closes int
	for _, c := range mock.Calls() {
		switch c.Method {
		case protocol.MethodDidOpen:
			opens++
		case protocol.MethodDidClose:
			closes++
		}
	}
	if opens != 2 || closes != 1 {
		t.Fatalf("expected 2 didOpen + 1 didClose after watched change, got %d open / %d close", opens, closes)
	}
}

func TestAdapter_Subscribe(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	received := make(chan string, 1)
	unsubscribe := ad.Subscribe(func(method string, _ json.RawMessage) {
		received <- method
	})

	if _, err := ad.Initialize(ctx, zig.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	if err := mock.Push(protocol.MethodPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         "file:///p/src/main.zig",
		Diagnostics: []protocol.Diagnostic{},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case method := <-received:
		if method != protocol.MethodPublishDiagnostics {
			t.Fatalf("got %q, want publishDiagnostics", method)
		}
	default:
		t.Fatal("notification not delivered to subscriber")
	}

	unsubscribe()
	if err := mock.Push(protocol.MethodPublishDiagnostics, protocol.PublishDiagnosticsParams{}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-received:
		t.Fatalf("received notification after unsubscribe: %q", m)
	default:
	}
}

func TestAdapter_Capabilities_NilBeforeInitialize(t *testing.T) {
	mock := jsonrpc.NewMockCaller()
	ad := zig.New(mock)
	if ad.Capabilities() != nil {
		t.Fatal("expected nil capabilities before Initialize")
	}
}
