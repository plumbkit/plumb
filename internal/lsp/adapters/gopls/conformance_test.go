package gopls_test

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/conformance"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return gopls.New(c) }, gopls.DefaultInitParams, lsptest.Scenario{
		Name: "generic push baseline", RootURI: "file:///workspace/go-app",
		DocumentURI: "file:///workspace/go-app/main.go", LanguageID: "go",
		Source: "package main\nfunc main() { missing() }", Mode: lsptest.Push,
		Diagnostic: protocol.Diagnostic{Severity: protocol.SevError, Source: "gopls", Message: "undefined: missing"},
	})
}

// TestConformance_PullDiagnosticsGeneric exercises Plumb's pull-diagnostics
// protocol handling (textDocument/diagnostic, workspace/diagnostic,
// result-ID negotiation, related documents) through the gopls adapter's
// Diagnostic()/WorkspaceDiagnostic() plumbing. This is a GENERIC exercise of
// the protocol surface, not a claim about gopls's own real-world behaviour:
// gopls is validated as push-first (see TestConformance_PushBaseline); real
// per-server pull validation is a real-binary concern (see
// pull_integration_test.go / task 7). The scenario previously lived in the
// zig adapter's conformance test, mislabelled "zig pull baseline" — zig's
// actual validated behaviour is push (see
// internal/lsp/adapters/zig/doc.go), so the pull-mechanics exercise is
// re-homed here against a generic adapter instead of asserting something
// false about zig.
func TestConformance_PullDiagnosticsGeneric(t *testing.T) {
	const otherURI = "file:///workspace/go-app/other.go"
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return gopls.New(c) }, gopls.DefaultInitParams, lsptest.Scenario{
		Name: "generic pull-diagnostics exercise", RootURI: "file:///workspace/go-app",
		DocumentURI: "file:///workspace/go-app/main.go", LanguageID: "go",
		Source: "package main\nfunc main() { missing(); alsoMissing() }", Mode: lsptest.Pull,
		DiagnosticOptions: &protocol.DiagnosticOptions{Identifier: "generic", InterFileDependencies: true, WorkspaceDiagnostics: true},
		Diagnostics: []protocol.Diagnostic{
			{Severity: protocol.SevError, Source: "generic", Message: "undefined: missing"},
			{Severity: protocol.SevError, Source: "generic", Message: "undefined: alsoMissing"},
		},
		RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
			otherURI: {
				Kind:  protocol.DiagnosticReportFull,
				Items: []protocol.Diagnostic{{Severity: protocol.SevWarning, Source: "generic", Message: "unused import"}},
			},
		},
		WorkspaceReports: []protocol.WorkspaceDiagnosticReport{
			{Items: []protocol.WorkspaceDocumentDiagnosticReport{
				{
					Kind: protocol.DiagnosticReportFull, URI: "file:///workspace/go-app/main.go",
					Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "undefined: missing"}},
				},
				// Zero diagnostics: proves the full-report items-key wire fix
				// (a real server's "this document has no problems" answer must
				// not be indistinguishable from an unchanged/no-signal report).
				{Kind: protocol.DiagnosticReportFull, URI: otherURI},
			}},
		},
	})
}

// TestConformance_HybridDiagnosticsGeneric exercises hybrid mode (a server
// that advertises pull AND still emits push notifications) — never observed
// from a currently-validated adapter, but a shape the protocol permits and
// the diagnostics tool must handle (see internal/cli's
// diagnosticsHybridFlip). Generic, like TestConformance_PullDiagnosticsGeneric.
func TestConformance_HybridDiagnosticsGeneric(t *testing.T) {
	conformance.RunConformance(t, func(c jsonrpc.Caller) lsp.Client { return gopls.New(c) }, gopls.DefaultInitParams, lsptest.Scenario{
		Name: "generic hybrid exercise", RootURI: "file:///workspace/go-app",
		DocumentURI: "file:///workspace/go-app/main.go", LanguageID: "go",
		Source: "package main\nfunc main() { missing() }", Mode: lsptest.Hybrid,
		Diagnostic: protocol.Diagnostic{Severity: protocol.SevError, Source: "generic", Message: "undefined: missing"},
	})
}
