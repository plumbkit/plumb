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

// zls's real validated behaviour is PUSH (see internal/lsp/adapters/zig/doc.go:
// the DidChangeWatchedFiles + DidOpen → publishDiagnostics round-trip has been
// green against a real zls since 2026-06-17, once plumb advertised the
// textDocument.publishDiagnostics client capability — the earlier
// "zls is pull-only" hypothesis was wrong). This conformance test previously
// exercised Mode: Pull under the mislabel "zig pull baseline"; the pull
// mechanics it exercised were never actually a zig-specific claim, so they
// now live as a GENERIC scenario on the gopls adapter's conformance test
// (TestConformance_PullDiagnosticsGeneric) instead of asserting something
// false about zls here.
func TestConformance_PushBaseline(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src", "main.zig")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	const text = "pub fn main() void { missing(); }"
	if err := os.WriteFile(source, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return zig.New(c) }, zig.DefaultInitParams, lsptest.Scenario{
		Name: "zig push baseline", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source), LanguageID: "zig",
		Source: text, Mode: lsptest.Push,
		Diagnostic: protocol.Diagnostic{Severity: protocol.SevError, Source: "zls", Message: "undeclared identifier 'missing'"},
	})
}
