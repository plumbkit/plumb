package zig_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/zig"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

func TestConformance_PullBaseline(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src", "main.zig")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	const text = "pub fn main() void { missing(); }"
	if err := os.WriteFile(source, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return zig.New(c) }, lsptest.Scenario{
		Name: "zig pull baseline", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source), LanguageID: "zig",
		Source: text, Mode: lsptest.Pull,
		Diagnostic: protocol.Diagnostic{Severity: protocol.SevError, Source: "zls", Message: "undeclared identifier 'missing'"},
	})
}
