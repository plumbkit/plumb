package pyright_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/pyright"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// pyright-langserver's validated behaviour is PUSH (see doc.go: the
// DidChangeWatchedFiles → publishDiagnostics round-trip is the integration
// test's promotion gate). This deterministic scenario validates plumb's
// adapter contract only; the real-binary integration test remains the gate on
// pyright itself.
func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return pyright.New(c) }, pyright.DefaultInitParams, pyrightConformanceScenario(t))
}

func pyrightConformanceScenario(t *testing.T) lsptest.Scenario {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "app.py")
	const text = "def main():\n    missing()\n"
	writeConformanceFixture(t, map[string]string{
		filepath.Join(root, "pyrightconfig.json"): `{"include": ["."]}`,
		source: text,
	})
	return lsptest.Scenario{
		Name: "pyright push baseline", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source),
		LanguageID:  "python", Source: text, Mode: lsptest.Push,
		Diagnostic:    protocol.Diagnostic{Severity: protocol.SevError, Source: "Pyright", Message: `"missing" is not defined`},
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
