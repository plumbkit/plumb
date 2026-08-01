package html_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/html"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// vscode-html-language-server pushes diagnostics; it has no pull surface (see
// doc.go). This deterministic scenario validates plumb's adapter contract only
// — the adapter is still "experimental" because its real-binary integration
// test has never run green, and this suite does not change that.
//
// The document must exist on disk: the HTML server has no filesystem access, so
// the adapter opens the file itself before a per-document query
// (Adapter.ensureOpen), and an in-memory-only URI would fail the harness's
// document subtest on a read error before reaching the contract under test.
func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return html.New(c) }, html.DefaultInitParams, htmlConformanceScenario(t))
}

func htmlConformanceScenario(t *testing.T) lsptest.Scenario {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "index.html")
	const text = "<html>\n  <body>\n    <h1>Sample\n  </body>\n</html>\n"
	writeConformanceFixture(t, map[string]string{source: text})
	return lsptest.Scenario{
		Name: "html document push", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source),
		LanguageID:  "html", Source: text, Mode: lsptest.Push,
		Diagnostic:    protocol.Diagnostic{Severity: protocol.SevWarning, Source: "html", Message: "Tag must be paired, missing end tag: </h1>"},
		RegisterWatch: true,
	}
}

// writeConformanceFixture materialises the scenario's files on disk. The
// conformance harness queries the document through the adapter, and adapters
// that open lazily read it from disk, so the fixture must be real rather than
// in-memory.
func writeConformanceFixture(t *testing.T, files map[string]string) {
	t.Helper()
	for path, body := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
