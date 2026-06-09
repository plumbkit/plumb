package gopls_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

var mockInitResult = protocol.InitializeResult{
	Capabilities: protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: true,
			Change:    protocol.SyncIncremental,
		},
		HoverProvider:           &protocol.BoolOrOptions{Enabled: true},
		DefinitionProvider:      &protocol.BoolOrOptions{Enabled: true},
		ReferencesProvider:      &protocol.BoolOrOptions{Enabled: true},
		DocumentSymbolProvider:  &protocol.BoolOrOptions{Enabled: true},
		WorkspaceSymbolProvider: &protocol.BoolOrOptions{Enabled: true},
		RenameProvider:          json.RawMessage(`true`),
	},
	ServerInfo: &protocol.ServerInfo{Name: "gopls", Version: "0.21.0"},
}

func newMockAdapter(t *testing.T) (*gopls.Adapter, *jsonrpc.MockCaller) {
	t.Helper()
	mock := jsonrpc.NewMockCaller()
	mock.HandleOK(protocol.MethodInitialize, mockInitResult)
	mock.Handle(protocol.MethodInitialized, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodShutdown, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodExit, func(_ json.RawMessage) (any, error) { return nil, nil })
	return gopls.New(mock), mock
}

func TestAdapter_Initialize(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	result, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///project"))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ServerInfo == nil || result.ServerInfo.Name != "gopls" {
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
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///project")); err != nil {
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

func TestAdapter_ShutdownExit(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///project")); err != nil {
		t.Fatal(err)
	}
	if err := ad.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := ad.Exit(ctx); err != nil {
		t.Fatalf("Exit: %v", err)
	}
	var seenShutdown, seenExit bool
	for _, c := range mock.Calls() {
		switch c.Method {
		case protocol.MethodShutdown:
			seenShutdown = true
		case protocol.MethodExit:
			seenExit = true
		}
	}
	if !seenShutdown {
		t.Fatal("shutdown not sent")
	}
	if !seenExit {
		t.Fatal("exit not sent")
	}
}

func TestAdapter_DidOpenDidClose(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()
	mock.Handle(protocol.MethodDidOpen, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodDidClose, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: "file:///p/main.go", LanguageID: "go", Version: 1, Text: "package main\n",
		},
	}); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	if err := ad.DidClose(ctx, protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/main.go"},
	}); err != nil {
		t.Fatalf("DidClose: %v", err)
	}
}

func TestAdapter_DidChangeWatchedFiles(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()
	mock.Handle(protocol.MethodDidChangeWatchedFiles, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: "file:///p/foo.go", Type: protocol.FileChanged},
		},
	})
	if err != nil {
		t.Fatalf("DidChangeWatchedFiles: %v", err)
	}
	var found bool
	for _, c := range mock.Calls() {
		if c.Method == protocol.MethodDidChangeWatchedFiles {
			found = true
		}
	}
	if !found {
		t.Fatal("didChangeWatchedFiles notification not sent")
	}
}

func TestAdapter_DidChange(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()
	mock.Handle(protocol.MethodDidChange, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidChange(ctx, protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			URI:     "file:///p/main.go",
			Version: 2,
		},
		ContentChanges: []protocol.TextDocumentContentChangeEvent{
			{Text: "package main\n\nfunc main() {}\n"},
		},
	}); err != nil {
		t.Fatalf("DidChange: %v", err)
	}
	var found bool
	for _, c := range mock.Calls() {
		if c.Method == protocol.MethodDidChange {
			found = true
		}
	}
	if !found {
		t.Fatal("didChange notification not sent")
	}
}

func TestAdapter_DocumentSymbols(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	expected := []protocol.DocumentSymbol{
		{Name: "main", Kind: protocol.SKFunction, Range: protocol.Range{}},
		{Name: "Greeter", Kind: protocol.SKStruct, Range: protocol.Range{}},
	}
	mock.HandleOK(protocol.MethodDocumentSymbols, expected)

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	syms, err := ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/main.go"},
	})
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) != len(expected) {
		t.Fatalf("got %d symbols, want %d", len(syms), len(expected))
	}
	if syms[0].Name != "main" {
		t.Fatalf("first symbol: got %q, want %q", syms[0].Name, "main")
	}
}

func TestAdapter_WorkspaceSymbols(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	expected := []protocol.SymbolInformation{
		{Name: "Greeter", Kind: protocol.SKStruct, Location: protocol.Location{URI: "file:///p/main.go"}},
	}
	mock.HandleOK(protocol.MethodWorkspaceSymbols, expected)

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
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
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	expected := []protocol.Location{
		{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 5}}},
	}
	mock.HandleOK(protocol.MethodDefinition, expected)

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	locs, err := ad.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/main.go"},
		Position:     protocol.Position{Line: 10, Character: 4},
	})
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("got %d locations, want 1", len(locs))
	}
	if locs[0].Range.Start.Line != 5 {
		t.Fatalf("got line %d, want 5", locs[0].Range.Start.Line)
	}
}

func TestAdapter_References(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	expected := []protocol.Location{
		{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 5}}},
		{URI: "file:///p/main.go", Range: protocol.Range{Start: protocol.Position{Line: 12}}},
	}
	mock.HandleOK(protocol.MethodReferences, expected)

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	refs, err := ad.References(ctx, protocol.ReferenceParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/main.go"},
		Position:     protocol.Position{Line: 5, Character: 6},
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
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	expected := protocol.Hover{
		Contents: protocol.MarkupContent{Kind: "markdown", Value: "```go\nfunc main()\n```"},
	}
	mock.HandleOK(protocol.MethodHover, expected)

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	hover, err := ad.Hover(ctx, protocol.HoverParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/main.go"},
		Position:     protocol.Position{Line: 5, Character: 6},
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
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	mock.HandleOK(protocol.MethodPrepareRename, protocol.PrepareRenameResult{
		Range:       protocol.Range{Start: protocol.Position{Line: 5}},
		Placeholder: "Greeter",
	})
	mock.HandleOK(protocol.MethodRename, protocol.WorkspaceEdit{
		Changes: map[string][]protocol.TextEdit{
			"file:///p/main.go": {
				{Range: protocol.Range{Start: protocol.Position{Line: 5}}, NewText: "Welcomer"},
			},
		},
	})

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	prep, err := ad.PrepareRename(ctx, protocol.PrepareRenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/main.go"},
		Position:     protocol.Position{Line: 5, Character: 6},
	})
	if err != nil {
		t.Fatalf("PrepareRename: %v", err)
	}
	if prep.Placeholder != "Greeter" {
		t.Fatalf("got placeholder %q, want Greeter", prep.Placeholder)
	}

	edit, err := ad.Rename(ctx, protocol.RenameParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/main.go"},
		Position:     protocol.Position{Line: 5, Character: 6},
		NewName:      "Welcomer",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if edit == nil {
		t.Fatal("expected non-nil edit")
	}
	if len(edit.Changes["file:///p/main.go"]) != 1 {
		t.Fatalf("unexpected edit: %v", edit)
	}
}

func TestAdapter_Subscribe(t *testing.T) {
	ad, mock := newMockAdapter(t)
	ctx := context.Background()

	received := make(chan string, 1)
	unsubscribe := ad.Subscribe(func(method string, _ json.RawMessage) {
		received <- method
	})

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	if err := mock.Push(protocol.MethodPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         "file:///p/main.go",
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
	ad := gopls.New(mock)
	if ad.Capabilities() != nil {
		t.Fatal("expected nil capabilities before Initialize")
	}
}
