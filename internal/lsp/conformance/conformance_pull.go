package conformance

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/lsptest"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// runUnchangedResultIDSubtest proves a pull-capable adapter threads
// DocumentDiagnosticParams.PreviousResultID faithfully: a first pull returns
// a full report with a resultId, a second pull echoing that exact resultId
// gets "unchanged" with no items, and a third pull with a stale/unknown
// resultId gets a FRESH full report rather than a fabricated "unchanged" —
// the safety-relevant half of the LSP 3.17 result-ID contract (see
// lsptest.Scenario.PullReports's doc comment).
func runUnchangedResultIDSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	if s.Mode != lsptest.Pull && s.Mode != lsptest.Hybrid {
		t.Skip("scenario is not pull-capable")
	}
	adapter, _, ctx := newAdapter(t, s)
	pull, ok := adapter.(pullClient)
	if !ok {
		t.Fatal("adapter does not implement pull diagnostics")
	}
	uri := protocol.TextDocumentIdentifier{URI: s.DocumentURI}

	first, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: uri})
	if err != nil {
		t.Fatal(err)
	}
	if first.Kind != protocol.DiagnosticReportFull || first.ResultID == "" {
		t.Fatalf("first pull = %#v, want a full report with a resultId", first)
	}

	second, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: uri, PreviousResultID: first.ResultID})
	if err != nil {
		t.Fatal(err)
	}
	if second.Kind != protocol.DiagnosticReportUnchanged || second.ResultID != first.ResultID || len(second.Items) != 0 {
		t.Fatalf("second pull (matching previousResultId) = %#v, want unchanged with no items", second)
	}

	third, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: uri, PreviousResultID: "stale-or-unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if third.Kind != protocol.DiagnosticReportFull {
		t.Fatalf("third pull (mismatched previousResultId) = %#v, want a fresh full report — the fake must not fabricate 'unchanged' for an ID it never issued", third)
	}
}

// runRelatedDocumentsSubtest proves a full report's RelatedDocuments map
// round-trips through the adapter's Diagnostic() decode. Skipped when the
// scenario declares none.
func runRelatedDocumentsSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	if s.RelatedDocuments == nil {
		t.Skip("scenario has no related documents")
	}
	adapter, _, ctx := newAdapter(t, s)
	pull, ok := adapter.(pullClient)
	if !ok {
		t.Fatal("adapter does not implement pull diagnostics despite a RelatedDocuments scenario")
	}
	report, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: protocol.TextDocumentIdentifier{URI: s.DocumentURI}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RelatedDocuments) != len(s.RelatedDocuments) {
		t.Fatalf("relatedDocuments = %#v, want %d entr(ies)", report.RelatedDocuments, len(s.RelatedDocuments))
	}
	for uri, want := range s.RelatedDocuments {
		got, ok := report.RelatedDocuments[uri]
		if !ok {
			t.Fatalf("missing related document %s", uri)
		}
		if got.Kind != want.Kind || len(got.Items) != len(want.Items) {
			t.Fatalf("related document %s = %#v, want %#v", uri, got, want)
		}
	}
}

// runWorkspacePullSubtest proves workspace/diagnostic round-trips a scripted
// WorkspaceDiagnosticReport, including a full-but-zero-diagnostics document
// entry's items key surviving the wire (the caution this task fixed: see
// lsptest's diagnosticReportJSON/workspaceDiagnosticReportJSON). Skipped when
// the scenario declares no WorkspaceReports (matching a server that does not
// support workspace pulls at all).
func runWorkspacePullSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	if len(s.WorkspaceReports) == 0 {
		t.Skip("scenario has no workspace/diagnostic reports")
	}
	adapter, _, ctx := newAdapter(t, s)
	wpc, ok := adapter.(workspacePullClient)
	if !ok {
		t.Fatal("adapter does not implement workspace pull despite a WorkspaceReports scenario")
	}
	report, err := wpc.WorkspaceDiagnostic(ctx, protocol.WorkspaceDiagnosticParams{})
	if err != nil {
		t.Fatal(err)
	}
	want := s.WorkspaceReports[0]
	if len(report.Items) != len(want.Items) {
		t.Fatalf("workspace report = %#v, want %d item(s)", report, len(want.Items))
	}
	for i, wantItem := range want.Items {
		got := report.Items[i]
		if got.Kind != wantItem.Kind || got.URI != wantItem.URI {
			t.Fatalf("workspace report item %d = %#v, want %#v", i, got, wantItem)
		}
		if wantItem.Kind == protocol.DiagnosticReportFull && got.Items == nil {
			t.Fatalf("full workspace report for %s lost its items key on the wire", wantItem.URI)
		}
	}
}

// runRefreshSubtest proves an adapter's OWN (unmediated) server-request
// handler declines workspace/diagnostic/refresh with a proper
// method-not-found. Production wires the actual refresh HANDLING one layer
// above this — internal/cli/pool_diagnostics.go's wrapServerRequest, applied
// in poolOnStart, wraps every adapter's handler in front to intercept
// refresh before it ever reaches the adapter. This conformance suite lives
// below internal/cli and cannot reach that wrapper, so it instead proves the
// base case the wrapper falls back on for every OTHER method: an adapter
// that does not special-case a server-initiated request answers it with
// method-not-found, full stop. Runs unconditionally — every adapter, in
// every mode, must decline identically.
func runRefreshSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	_, server, ctx := newAdapter(t, s)
	err := server.Refresh(ctx)
	if !jsonrpc.IsMethodNotFound(err) {
		t.Fatalf("adapter's unmediated refresh handling = %v, want a proper method-not-found", err)
	}
}

// runMethodNotFoundDowngradeSubtest proves an adapter propagates a
// server-side -32601 on textDocument/diagnostic as a jsonrpc.IsMethodNotFound
// -detectable error — the exact signal internal/cli's downgradeDiagMode keys
// off to flip a pull/hybrid connection to push for the session. It builds
// its own scenario variant (forcing Mode Pull and
// MethodNotFound[textDocument/diagnostic]) rather than mutating s, so this
// runs for every adapter regardless of the scenario's own declared mode.
func runMethodNotFoundDowngradeSubtest(t *testing.T, s lsptest.Scenario, newAdapter adapterFactory) {
	t.Helper()
	downgrade := s
	downgrade.Mode = lsptest.Pull
	downgrade.MethodNotFound = map[string]bool{protocol.MethodDiagnostic: true}
	adapter, _, ctx := newAdapter(t, downgrade)
	pull, ok := adapter.(pullClient)
	if !ok {
		t.Skip("adapter does not implement pull diagnostics")
	}
	_, err := pull.Diagnostic(ctx, protocol.DocumentDiagnosticParams{TextDocument: protocol.TextDocumentIdentifier{URI: s.DocumentURI}})
	if !jsonrpc.IsMethodNotFound(err) {
		t.Fatalf("expected a method-not-found error simulating a server that advertised pull but does not implement it, got %v", err)
	}
}
