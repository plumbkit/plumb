package jdtls_test

import (
	"path/filepath"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/jdtls"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// jdtls's validated behaviour is PUSH (see doc.go). This deterministic
// Maven-shaped scenario validates plumb's adapter contract only; the
// real-binary integration test remains the gate on jdtls itself.
func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return jdtls.New(c) }, jdtls.DefaultInitParams, jdtlsConformanceScenario(t))
}

func jdtlsConformanceScenario(t *testing.T) lsptest.Scenario {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "src", "main", "java", "App.java")
	const text = "public class App {\n    public static void main(String[] args) { missing(); }\n}\n"
	const pom = `<project><modelVersion>4.0.0</modelVersion>` +
		`<groupId>dev.plumb</groupId><artifactId>conformance</artifactId><version>1.0</version></project>`
	conformance.WriteFixture(t, map[string]string{
		filepath.Join(root, "pom.xml"): pom,
		source:                         text,
	})
	return lsptest.Scenario{
		Name: "jdtls Maven project push", RootURI: paths.PathToURI(root),
		DocumentURI: paths.PathToURI(source),
		LanguageID:  "java", Source: text, Mode: lsptest.Push,
		Diagnostic:    protocol.Diagnostic{Severity: protocol.SevError, Source: "Java", Message: "The method missing() is undefined for the type App"},
		RegisterWatch: true,
	}
}
