package swift_test

import (
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/swift"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// sourcekit-lsp's validated behaviour is PUSH (see doc.go). This deterministic
// SwiftPM-shaped scenario validates plumb's adapter contract only; the
// real-binary integration test remains the gate on sourcekit-lsp itself.
//
// The document must exist on disk: this adapter opens per-document queries
// lazily, reading the file itself (base.OpenTracker.Ensure), so an in-memory-only
// URI would fail the harness's document subtest on a read error before it ever
// reached the protocol contract under test.
func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return swift.New(c) }, swift.DefaultInitParams, swiftConformanceScenario(t))
}

func swiftConformanceScenario(t *testing.T) lsptest.Scenario {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "Sources", "conformance", "main.swift")
	const text = "missing()\n"
	const manifest = "// swift-tools-version:5.9\nimport PackageDescription\n" +
		"let package = Package(name: \"conformance\", targets: [.executableTarget(name: \"conformance\")])\n"
	conformance.WriteFixture(t, map[string]string{
		filepath.Join(root, "Package.swift"): manifest,
		source:                               text,
	})
	return lsptest.Scenario{
		Name: "swift SwiftPM package push", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source),
		LanguageID:  "swift", Source: text, Mode: lsptest.Push,
		Diagnostic:    protocol.Diagnostic{Severity: protocol.SevError, Source: "sourcekitd", Message: "cannot find 'missing' in scope"},
		RegisterWatch: true,
	}
}
