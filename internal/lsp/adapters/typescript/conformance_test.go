package typescript_test

import (
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/typescript"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// typescript-language-server's validated behaviour is PUSH: it advertises no
// diagnosticProvider and answers textDocument/diagnostic with -32601 (see
// doc.go), so the scenario deliberately does not claim pull. The harness still
// exercises the adapter's dormant pull surface through its
// "method-not-found downgrade" subtest, which builds its own pull-mode variant
// — the same -32601 the real server returns. Per-server pull validation stays
// a real-binary concern (pull_integration_test.go).
func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return typescript.New(c) }, typescript.DefaultInitParams, typescriptConformanceScenario(t))
}

func typescriptConformanceScenario(t *testing.T) lsptest.Scenario {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "src", "index.ts")
	const text = "export function main(): void {\n  missing();\n}\n"
	conformance.WriteFixture(t, map[string]string{
		filepath.Join(root, "tsconfig.json"): `{"compilerOptions": {"strict": true}, "include": ["src"]}`,
		source:                               text,
	})
	return lsptest.Scenario{
		Name: "typescript project push", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source),
		LanguageID:  "typescript", Source: text, Mode: lsptest.Push,
		Diagnostic:    protocol.Diagnostic{Severity: protocol.SevError, Source: "typescript", Message: "Cannot find name 'missing'."},
		RegisterWatch: true,
	}
}
