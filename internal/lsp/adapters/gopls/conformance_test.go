package gopls_test

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return gopls.New(c) }, lsptest.Scenario{
		Name: "generic push baseline", RootURI: "file:///workspace/go-app",
		DocumentURI: "file:///workspace/go-app/main.go", LanguageID: "go",
		Source: "package main\nfunc main() { missing() }", Mode: lsptest.Push,
		Diagnostic: protocol.Diagnostic{Severity: protocol.SevError, Source: "gopls", Message: "undefined: missing"},
	})
}
