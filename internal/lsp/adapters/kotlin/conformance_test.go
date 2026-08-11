package kotlin_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/kotlin"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// This deterministic Gradle-shaped scenario validates Plumb's Kotlin adapter
// contract without pretending to validate kotlin-lsp itself. The real-binary
// integration test remains the promotion gate.
//
// The scenario is PULL, not push: kotlin-lsp advertises diagnosticProvider and
// never sends publishDiagnostics (measured — see doc.go), so a push scenario
// here would assert a round-trip the real server does not perform.
func TestConformance_GradleProjectPull(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return kotlin.New(c) }, kotlin.DefaultInitParams, kotlinScenario(t))
}

func kotlinScenario(t *testing.T) lsptest.Scenario {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "src", "main", "kotlin", "App.kt")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	const text = "fun main() = missing()"
	for path, body := range map[string]string{
		filepath.Join(root, "settings.gradle.kts"): `rootProject.name = "conformance"`,
		filepath.Join(root, "build.gradle.kts"):    `plugins { kotlin("jvm") version "2.2.0" }`,
		source:                                     text,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return lsptest.Scenario{
		Name: "kotlin Gradle project", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source),
		LanguageID:  "kotlin", Source: text, Mode: lsptest.Pull,
		// The options-object form, matching what the real server advertises:
		// interFileDependencies true, workspaceDiagnostics false.
		DiagnosticOptions: &protocol.DiagnosticOptions{InterFileDependencies: true},
		// Shaped like a real reply: source "Kotlin" (capitalised by the server,
		// unlike every other adapter's lower-case id) with a FIR diagnostic code.
		Diagnostic: protocol.Diagnostic{
			Severity: protocol.SevError, Source: "Kotlin", Code: "UNRESOLVED_REFERENCE",
			Message: "Unresolved reference 'missing'.",
		},
		RegisterWatch: true,
	}
}
