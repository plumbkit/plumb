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
// contract without pretending to validate kotlin-language-server itself. The
// existing real-binary integration test remains the promotion gate.
func TestConformance_GradleProjectPush(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return kotlin.New(c) }, kotlinScenario(t))
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
		LanguageID:  "kotlin", Source: text, Mode: lsptest.Push,
		Diagnostic:    protocol.Diagnostic{Severity: protocol.SevError, Source: "kotlin", Message: "Unresolved reference: missing"},
		RegisterWatch: true,
	}
}
