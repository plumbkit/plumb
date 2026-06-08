package html_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	html "github.com/golimpio/plumb/internal/lsp/adapters/html"
	"github.com/golimpio/plumb/internal/lsp/jsonrpc"
	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// initResult is a canned Initialize response.
var initResult = protocol.InitializeResult{
	Capabilities: protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: true,
			Change:    protocol.SyncFull,
		},
		HoverProvider:          &protocol.BoolOrOptions{Enabled: true},
		DocumentSymbolProvider: &protocol.BoolOrOptions{Enabled: true},
	},
	ServerInfo: &protocol.ServerInfo{Name: "vscode-html-language-server", Version: "1.0.0"},
}

// newAdapter sets up a MockCaller with sensible defaults and returns the adapter.
func newAdapter(t *testing.T) (*html.Adapter, *jsonrpc.MockCaller) {
	t.Helper()
	mock := jsonrpc.NewMockCaller()
	mock.HandleOK(protocol.MethodInitialize, initResult)
	mock.Handle(protocol.MethodInitialized, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodShutdown, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodExit, func(_ json.RawMessage) (any, error) { return nil, nil })
	return html.New(mock), mock
}

// writeTempHTML writes content to a temp index.html and returns its file:// URI,
// so ensureOpen can read the document from disk before a query.
func writeTempHTML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp html: %v", err)
	}
	return "file://" + path
}

// assertOpenedBefore checks the adapter sent a didOpen notification before its
// first call to method — the HTML server answers document queries only for open
// documents.
func assertOpenedBefore(t *testing.T, mock *jsonrpc.MockCaller, method string) {
	t.Helper()
	opened := -1
	for i, c := range mock.Calls() {
		if c.Method == protocol.MethodDidOpen && opened == -1 {
			opened = i
		}
		if c.Method == method {
			if opened == -1 || opened > i {
				t.Fatalf("expected didOpen before %s; calls=%v", method, mock.Calls())
			}
			return
		}
	}
	t.Fatalf("%s was not called; calls=%v", method, mock.Calls())
}

func TestAdapter_Initialize(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	result, err := ad.Initialize(ctx, html.DefaultInitParams("file:///project"))
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ServerInfo == nil || result.ServerInfo.Name != "vscode-html-language-server" {
		t.Fatalf("unexpected server info: %v", result.ServerInfo)
	}
	caps := ad.Capabilities()
	if caps == nil {
		t.Fatal("expected capabilities to be stored after Initialize")
	}
	if caps.DocumentSymbolProvider == nil || !caps.DocumentSymbolProvider.Enabled {
		t.Fatal("expected document symbols to be enabled")
	}

	calls := mock.Calls()
	if len(calls) != 1 || calls[0].Method != protocol.MethodInitialize {
		t.Fatalf("unexpected calls: %v", calls)
	}
}

func TestAdapter_Initialized(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///project")); err != nil {
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

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: "file:///p/index.html", Type: protocol.FileChanged},
			{URI: "file:///p/about.html", Type: protocol.FileCreated},
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

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: "file:///p/index.html", LanguageID: "html", Version: 1, Text: "<h1>Hi</h1>\n",
		},
	}); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	if err := ad.DidClose(ctx, protocol.DidCloseTextDocumentParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file:///p/index.html"},
	}); err != nil {
		t.Fatalf("DidClose: %v", err)
	}
}

// TestAdapter_ReopensAfterWatchedChange verifies an external edit closes the
// stale open copy so the next query reopens the document with fresh content.
func TestAdapter_ReopensAfterWatchedChange(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()
	mock.HandleOK(protocol.MethodDocumentSymbols, []protocol.DocumentSymbol{})
	mock.Handle(protocol.MethodDidOpen, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodDidClose, func(_ json.RawMessage) (any, error) { return nil, nil })
	mock.Handle(protocol.MethodDidChangeWatchedFiles, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}
	uri := writeTempHTML(t, "<html></html>\n")
	dsp := protocol.DocumentSymbolParams{TextDocument: protocol.TextDocumentIdentifier{URI: uri}}

	if _, err := ad.DocumentSymbols(ctx, dsp); err != nil {
		t.Fatalf("first DocumentSymbols: %v", err)
	}
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: uri, Type: protocol.FileChanged}},
	}); err != nil {
		t.Fatalf("DidChangeWatchedFiles: %v", err)
	}
	if _, err := ad.DocumentSymbols(ctx, dsp); err != nil {
		t.Fatalf("second DocumentSymbols: %v", err)
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
		t.Fatalf("got %d didOpen / %d didClose, want 2 / 1", opens, closes)
	}
}

func TestAdapter_DocumentSymbols(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := []protocol.DocumentSymbol{
		{Name: "html", Kind: protocol.SKField, Range: protocol.Range{}},
		{Name: "body", Kind: protocol.SKField, Range: protocol.Range{}},
	}
	mock.HandleOK(protocol.MethodDocumentSymbols, expected)
	mock.Handle(protocol.MethodDidOpen, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempHTML(t, "<html><body></body></html>\n")
	syms, err := ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) != len(expected) {
		t.Fatalf("got %d symbols, want %d", len(syms), len(expected))
	}
	if syms[0].Name != "html" {
		t.Fatalf("first symbol: got %q, want %q", syms[0].Name, "html")
	}
	assertOpenedBefore(t, mock, protocol.MethodDocumentSymbols)
}

// TestAdapter_DocumentSymbols_FlatShapeRangeMapping proves the union decode:
// when the server returns the legacy flat SymbolInformation[] (range under
// location.range), the adapter maps it onto DocumentSymbol.Range instead of
// leaving every symbol at L1 (the pre-fix bug).
func TestAdapter_DocumentSymbols_FlatShapeRangeMapping(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	flat := []protocol.SymbolInformation{
		{Name: "html", Kind: protocol.SKField, Location: protocol.Location{
			URI:   "file:///p/index.html",
			Range: protocol.Range{Start: protocol.Position{Line: 5}, End: protocol.Position{Line: 9}},
		}},
	}
	mock.HandleOK(protocol.MethodDocumentSymbols, flat)
	mock.Handle(protocol.MethodDidOpen, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempHTML(t, "<html><body></body></html>\n")
	syms, err := ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	if len(syms) != 1 {
		t.Fatalf("got %d symbols, want 1", len(syms))
	}
	if syms[0].Range.Start.Line != 5 || syms[0].Range.End.Line != 9 {
		t.Fatalf("flat location.range must map to .Range; got %+v", syms[0].Range)
	}
}

func TestAdapter_Hover(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	expected := protocol.Hover{
		Contents: protocol.MarkupContent{Kind: "markdown", Value: "The `section` element..."},
	}
	mock.HandleOK(protocol.MethodHover, expected)
	mock.Handle(protocol.MethodDidOpen, func(_ json.RawMessage) (any, error) { return nil, nil })

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	uri := writeTempHTML(t, "<html><body><section>hi</section></body></html>\n")
	hover, err := ad.Hover(ctx, protocol.HoverParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		Position:     protocol.Position{Line: 0, Character: 14},
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
	assertOpenedBefore(t, mock, protocol.MethodHover)
}

func TestAdapter_Subscribe(t *testing.T) {
	ad, mock := newAdapter(t)
	ctx := context.Background()

	received := make(chan string, 1)
	unsubscribe := ad.Subscribe(func(method string, _ json.RawMessage) {
		received <- method
	})

	if _, err := ad.Initialize(ctx, html.DefaultInitParams("file:///p")); err != nil {
		t.Fatal(err)
	}

	if err := mock.Push(protocol.MethodPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         "file:///p/index.html",
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
	ad := html.New(mock)
	if ad.Capabilities() != nil {
		t.Fatal("expected nil capabilities before Initialize")
	}
}
