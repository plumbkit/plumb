//go:build integration

package html_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	html "github.com/plumbkit/plumb/internal/lsp/adapters/html"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// requireHTMLServer skips if vscode-html-language-server is not on PATH and
// returns its path. It is not installed on the validation machine, so this test
// skips there; it runs and validates the adapter wherever the binary is present.
func requireHTMLServer(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("vscode-html-language-server")
	if err != nil {
		t.Skip("vscode-html-language-server not found on PATH — install with: npm install -g vscode-langservers-extracted")
	}
	return p
}

// repoRoot walks parent dirs until go.mod is found.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root")
		}
		dir = parent
	}
}

// startHTMLServer spawns vscode-html-language-server and returns a ready adapter.
func startHTMLServer(t *testing.T, ws string) *html.Adapter {
	t.Helper()
	bin := requireHTMLServer(t)

	cmd := exec.Command(bin, "--stdio")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal("stdin pipe:", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal("stdout pipe:", err)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatal("start vscode-html-language-server:", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	conn := jsonrpc.NewConn(stdout, stdin)
	t.Cleanup(func() { _ = conn.Close() })

	ad := html.New(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := ad.Initialize(ctx, html.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	return ad
}

func TestIntegration_DocumentSymbols(t *testing.T) {
	fixture := filepath.Join(repoRoot(t), "testdata", "html-fixture")
	ad := startHTMLServer(t, fixture)
	srcPath := filepath.Join(fixture, "index.html")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	src, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	uri := protocol.FileURI(srcPath)
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: uri, LanguageID: "html", Version: 1, Text: string(src),
		},
	}); err != nil {
		t.Fatal("didOpen:", err)
	}

	var syms []protocol.DocumentSymbol
	deadline := time.After(45 * time.Second)
	for {
		syms, err = ad.DocumentSymbols(ctx, protocol.DocumentSymbolParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
		})
		if err == nil && len(syms) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("no document symbols within deadline (err=%v, n=%d)", err, len(syms))
		case <-time.After(time.Second):
		}
	}

	// vscode-html-language-server returns the tag tree; the <body> element is
	// always present in the fixture.
	if !hasSymbol(syms, "body") {
		t.Fatalf("symbol body not found; got %v", symbolNames(syms))
	}
}

// hasSymbol reports whether name appears anywhere in the symbol tree. HTML
// symbol names embed id/class (e.g. "section#intro"), so the match is a prefix
// check on the tag portion.
func hasSymbol(syms []protocol.DocumentSymbol, name string) bool {
	for _, s := range syms {
		if s.Name == name || tagOf(s.Name) == name || hasSymbol(s.Children, name) {
			return true
		}
	}
	return false
}

// tagOf returns the tag portion of an HTML document-symbol name, stripping any
// "#id" / ".class" suffix the server appends.
func tagOf(name string) string {
	for i, r := range name {
		if r == '#' || r == '.' {
			return name[:i]
		}
	}
	return name
}

func symbolNames(syms []protocol.DocumentSymbol) []string {
	out := make([]string, 0, len(syms))
	for _, s := range syms {
		out = append(out, s.Name)
	}
	return out
}

// TestIntegration_DidChangeWatchedFiles proves capability negotiation + the
// DidChangeWatchedFiles wire format are accepted by a real
// vscode-html-language-server, and that the external-write → notify → open →
// diagnostics pipeline works end to end. The diagnostic is triggered by an
// embedded <style> block with a CSS syntax error — the HTML server validates
// embedded CSS via vscode-css-languageservice, which is its most reliable
// diagnostic source.
func TestIntegration_DidChangeWatchedFiles(t *testing.T) {
	ws := t.TempDir()

	ad := startHTMLServer(t, ws)
	brokenPath := filepath.Join(ws, "broken.html")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	diagCh := make(chan int, 16)
	ad.Subscribe(func(method string, raw json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
		if p.URI != brokenURI {
			return
		}
		select {
		case diagCh <- len(p.Diagnostics):
		default:
		}
	})

	// Embedded CSS with a missing value — vscode-css-languageservice reports it.
	broken := []byte("<!DOCTYPE html>\n<html><head><style>\n.a { color: ; }\n</style></head><body></body></html>\n")
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{
			{URI: brokenURI, Type: protocol.FileCreated},
		},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}
	if err := ad.DidOpen(ctx, protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI: brokenURI, LanguageID: "html", Version: 1, Text: string(broken),
		},
	}); err != nil {
		t.Fatal("DidOpen:", err)
	}

	deadline := time.After(45 * time.Second)
	for {
		select {
		case n := <-diagCh:
			if n > 0 {
				return // success: the server validated embedded CSS and published diagnostics
			}
		case <-deadline:
			t.Fatal("vscode-html-language-server did not publish diagnostics for broken.html within deadline — " +
				"the didChangeWatchedFiles + didOpen pipeline is not reaching the server, " +
				"or embedded-CSS validation is disabled")
		}
	}
}
