package rust_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/rust"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// rust-analyzer's validated behaviour is PUSH (see doc.go: the
// DidChangeWatchedFiles + DidOpen → publishDiagnostics round-trip is the
// integration test's promotion gate). This deterministic Cargo-shaped scenario
// validates plumb's adapter contract only; the real-binary integration test
// remains the gate on rust-analyzer itself.
func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return rust.New(c) }, rust.DefaultInitParams, rustConformanceScenario(t))
}

func rustConformanceScenario(t *testing.T) lsptest.Scenario {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "src", "main.rs")
	const text = "fn main() { missing(); }\n"
	const manifest = "[package]\nname = \"conformance\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"
	writeConformanceFixture(t, map[string]string{
		filepath.Join(root, "Cargo.toml"): manifest,
		source:                            text,
	})
	return lsptest.Scenario{
		Name: "rust Cargo project push", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source),
		LanguageID:  "rust", Source: text, Mode: lsptest.Push,
		Diagnostic:    protocol.Diagnostic{Severity: protocol.SevError, Source: "rustc", Message: "cannot find function `missing` in this scope"},
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
