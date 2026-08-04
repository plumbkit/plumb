package html_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/html"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// TestErrorContract pins the "vscode-html-language-server <label>: <cause>"
// strings this adapter wraps every transport failure in. They are what plumb
// surfaces to agents and nothing else asserts them, so this is the net under
// any refactor that moves the wrapping elsewhere. The harness writes the named
// document to disk itself (see conformance.RunErrorContract).
func TestErrorContract(t *testing.T) {
	conformance.RunErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return html.New(c) },
		html.DefaultInitParams, "vscode-html-language-server", "index.html", "<html><body></body></html>\n")
}

// TestLazyOpenErrorContract pins "vscode-html-language-server open <uri>:
// <cause>", the label this adapter emits when the document it opens lazily is
// not readable. TestErrorContract cannot reach it: it hands the adapter a real
// file on purpose, so the request labels rather than this one are what fail
// there.
func TestLazyOpenErrorContract(t *testing.T) {
	conformance.RunLazyOpenErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return html.New(c) },
		"vscode-html-language-server", "absent.html")
}

// TestDocumentSymbolDecodeErrorLabel pins
// "vscode-html-language-server documentSymbol: decoding symbols: <cause>".
// This adapter is the only one that decodes a reply by hand — it maps the
// DocumentSymbol | SymbolInformation union — so it is the only one with a
// wrapping the transport can never reach: the request must SUCCEED and the
// payload be undecodable. The transport-failure harness therefore cannot pin
// it, and a shared base adapter reworking the reply path would otherwise change
// this string unobserved.
func TestDocumentSymbolDecodeErrorLabel(t *testing.T) {
	doc := filepath.Join(t.TempDir(), "index.html")
	conformance.WriteFixture(t, map[string]string{doc: "<html></html>\n"})

	conn := jsonrpc.NewMockCaller()
	// An object where the adapter expects an array of symbol nodes: the call
	// succeeds and the decode is what fails.
	conn.HandleOK(protocol.MethodDocumentSymbols, map[string]any{})
	adapter := html.New(conn)

	_, err := adapter.DocumentSymbols(context.Background(), protocol.DocumentSymbolParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: paths.PathToURI(doc)},
	})
	if err == nil {
		t.Fatal("DocumentSymbols on an undecodable reply = nil error, want a decoding-symbols wrapping")
	}
	const want = "vscode-html-language-server documentSymbol: decoding symbols: "
	if !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error = %q, want prefix %q", err.Error(), want)
	}
	// encoding/json's message wording is not plumb's to pin; the unwrap is.
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("error %q does not unwrap to the decode cause — the %%w wrapping was lost", err)
	}
}
