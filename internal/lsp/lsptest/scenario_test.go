package lsptest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestCallerRejectsUnexpectedRequest(t *testing.T) {
	c := NewCaller(Scenario{})
	err := c.Call(context.Background(), "unknown/method", nil, nil)
	var mnf *jsonrpc.MethodNotFoundError
	if !errors.As(err, &mnf) {
		t.Fatalf("expected a *jsonrpc.MethodNotFoundError, got %v (%T)", err, err)
	}
	if mnf.Method != "unknown/method" {
		t.Fatalf("MethodNotFoundError carries method %q, want %q", mnf.Method, "unknown/method")
	}
}

func TestCallerPullReport(t *testing.T) {
	d := protocol.Diagnostic{Message: "broken", Severity: protocol.SevError}
	c := NewCaller(Scenario{Mode: Pull, Diagnostic: d})
	var report protocol.DocumentDiagnosticReport
	if err := c.Call(context.Background(), protocol.MethodDiagnostic, nil, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Items) != 1 || report.Items[0].Message != d.Message {
		t.Fatalf("report = %#v", report)
	}
}

// TestCallerPullReportHonoursPreviousResultID proves the default (unscripted)
// synthesis only answers "unchanged" when the caller echoes back the exact
// resultId it was handed — never for an empty, stale, or unknown one.
func TestCallerPullReportHonoursPreviousResultID(t *testing.T) {
	c := NewCaller(Scenario{Mode: Pull, Diagnostic: protocol.Diagnostic{Message: "broken"}})
	ctx := context.Background()

	var first protocol.DocumentDiagnosticReport
	if err := c.Call(ctx, protocol.MethodDiagnostic, protocol.DocumentDiagnosticParams{}, &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != protocol.DiagnosticReportFull || first.ResultID == "" {
		t.Fatalf("first pull = %#v, want a full report with a resultId", first)
	}

	var unchanged protocol.DocumentDiagnosticReport
	if err := c.Call(ctx, protocol.MethodDiagnostic, protocol.DocumentDiagnosticParams{PreviousResultID: first.ResultID}, &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Kind != protocol.DiagnosticReportUnchanged || unchanged.ResultID != first.ResultID || len(unchanged.Items) != 0 {
		t.Fatalf("matching previousResultId = %#v, want unchanged with no items", unchanged)
	}

	var freshAgain protocol.DocumentDiagnosticReport
	if err := c.Call(ctx, protocol.MethodDiagnostic, protocol.DocumentDiagnosticParams{PreviousResultID: "stale-id"}, &freshAgain); err != nil {
		t.Fatal(err)
	}
	if freshAgain.Kind != protocol.DiagnosticReportFull {
		t.Fatalf("mismatched previousResultId = %#v, want a fresh full report, not a fabricated unchanged", freshAgain)
	}
}

// TestCallerPullReportsScript proves a declarative PullReports sequence is
// served in order to a client that threads previousResultId correctly, and
// clamps to its final entry once exhausted.
func TestCallerPullReportsScript(t *testing.T) {
	c := NewCaller(Scenario{
		Mode: Pull,
		PullReports: []protocol.DocumentDiagnosticReport{
			{Kind: protocol.DiagnosticReportFull, ResultID: "r1", Items: []protocol.Diagnostic{{Message: "one"}}},
			{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r1"},
			{Kind: protocol.DiagnosticReportFull, ResultID: "r2", Items: []protocol.Diagnostic{{Message: "two"}}},
		},
	})
	ctx := context.Background()
	steps := []struct {
		previousResultID string
		wantKind         string
		wantResultID     string
	}{
		{"", protocol.DiagnosticReportFull, "r1"},        // first pull: no baseline yet
		{"r1", protocol.DiagnosticReportUnchanged, "r1"}, // echoes r1 ⇒ unchanged honoured
		{"r1", protocol.DiagnosticReportFull, "r2"},      // full entries served as scripted
		{"r2", protocol.DiagnosticReportFull, "r2"},      // clamped to the last (full) entry
	}
	for i, step := range steps {
		var report protocol.DocumentDiagnosticReport
		params := protocol.DocumentDiagnosticParams{PreviousResultID: step.previousResultID}
		if err := c.Call(ctx, protocol.MethodDiagnostic, params, &report); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if report.Kind != step.wantKind || report.ResultID != step.wantResultID {
			t.Fatalf("call %d: report = (%q, %q), want (%q, %q)", i, report.Kind, report.ResultID, step.wantKind, step.wantResultID)
		}
	}
}

// TestCallerPullReportsScriptRejectsStaleOrMissingID locks in the scripted
// branch's safety rule: an "unchanged" script entry is served ONLY to a
// caller echoing the preceding entry's ResultID; a wrong OR omitted incoming
// previousResultId gets a fresh full report (the preceding full entry's
// content) — never a fabricated "nothing changed".
func TestCallerPullReportsScriptRejectsStaleOrMissingID(t *testing.T) {
	script := []protocol.DocumentDiagnosticReport{
		{Kind: protocol.DiagnosticReportFull, ResultID: "r1", Items: []protocol.Diagnostic{{Message: "one"}}},
		{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r1"},
	}
	for name, previousResultID := range map[string]string{"wrong ID": "bogus", "omitted ID": ""} {
		t.Run(name, func(t *testing.T) {
			c := NewCaller(Scenario{Mode: Pull, PullReports: script})
			ctx := context.Background()

			var first protocol.DocumentDiagnosticReport
			if err := c.Call(ctx, protocol.MethodDiagnostic, protocol.DocumentDiagnosticParams{}, &first); err != nil {
				t.Fatal(err)
			}
			if first.Kind != protocol.DiagnosticReportFull || first.ResultID != "r1" {
				t.Fatalf("first pull = (%q, %q), want (full, r1)", first.Kind, first.ResultID)
			}

			var second protocol.DocumentDiagnosticReport
			params := protocol.DocumentDiagnosticParams{PreviousResultID: previousResultID}
			if err := c.Call(ctx, protocol.MethodDiagnostic, params, &second); err != nil {
				t.Fatal(err)
			}
			if second.Kind != protocol.DiagnosticReportUnchanged {
				// The substituted full report carries the preceding full entry's state.
				if second.Kind != protocol.DiagnosticReportFull || second.ResultID != "r1" || len(second.Items) != 1 || second.Items[0].Message != "one" {
					t.Fatalf("substituted report = %#v, want the preceding full entry re-served", second)
				}
				return
			}
			t.Fatalf("second pull with %s = %#v — the script must not serve 'unchanged' to a caller whose baseline it cannot verify", name, second)
		})
	}
}

// TestCallerPullReportEmptyFullItemsWireShape proves a scripted full report
// with zero diagnostics survives the fake's Call() round trip with its items
// key intact (a non-nil, empty slice) rather than silently dropped by
// omitempty — see diagnosticReportJSON.
func TestCallerPullReportEmptyFullItemsWireShape(t *testing.T) {
	c := NewCaller(Scenario{Mode: Pull, PullReports: []protocol.DocumentDiagnosticReport{
		{Kind: protocol.DiagnosticReportFull, ResultID: "r1"},
	}})
	var report protocol.DocumentDiagnosticReport
	if err := c.Call(context.Background(), protocol.MethodDiagnostic, nil, &report); err != nil {
		t.Fatal(err)
	}
	if report.Kind != protocol.DiagnosticReportFull || report.Items == nil {
		t.Fatalf("report = %#v, want a full report whose items key survived the wire as a non-nil empty slice", report)
	}
}

func TestDiagnosticReportJSON_FullZeroItemsIncludesItemsKey(t *testing.T) {
	raw, err := diagnosticReportJSON(protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull, ResultID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"items":[]`) {
		t.Fatalf("wire bytes = %s, want an explicit empty items array for a full report", raw)
	}
}

func TestDiagnosticReportJSON_UnchangedOmitsItemsKey(t *testing.T) {
	raw, err := diagnosticReportJSON(protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"items"`) {
		t.Fatalf("wire bytes = %s, want no items key for an unchanged report", raw)
	}
}

func TestWorkspaceDiagnosticReportJSON_FullZeroItemsIncludesItemsKey(t *testing.T) {
	raw, err := workspaceDiagnosticReportJSON(protocol.WorkspaceDiagnosticReport{
		Items: []protocol.WorkspaceDocumentDiagnosticReport{{Kind: protocol.DiagnosticReportFull, URI: "file:///x.go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"items":[]`) {
		t.Fatalf("wire bytes = %s, want an explicit empty items array for the full entry", raw)
	}
}

func TestCallerWorkspaceDiagnostic(t *testing.T) {
	report := protocol.WorkspaceDiagnosticReport{Items: []protocol.WorkspaceDocumentDiagnosticReport{
		{Kind: protocol.DiagnosticReportFull, URI: "file:///a.go", Items: []protocol.Diagnostic{{Message: "boom"}}},
	}}
	c := NewCaller(Scenario{Mode: Pull, WorkspaceReports: []protocol.WorkspaceDiagnosticReport{report}})
	var got protocol.WorkspaceDiagnosticReport
	if err := c.Call(context.Background(), protocol.MethodWorkspaceDiagnostic, nil, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 1 || got.Items[0].URI != "file:///a.go" || len(got.Items[0].Items) != 1 {
		t.Fatalf("got = %#v", got)
	}
}

func TestCallerWorkspaceDiagnosticUnsupportedByDefault(t *testing.T) {
	c := NewCaller(Scenario{Mode: Pull})
	var got protocol.WorkspaceDiagnosticReport
	err := c.Call(context.Background(), protocol.MethodWorkspaceDiagnostic, nil, &got)
	if !jsonrpc.IsMethodNotFound(err) {
		t.Fatalf("expected method-not-found when no WorkspaceReports are scripted, got %v", err)
	}
}

func TestCallerMethodNotFoundOverride(t *testing.T) {
	c := NewCaller(Scenario{
		Mode: Pull, Diagnostic: protocol.Diagnostic{Message: "x"},
		MethodNotFound: map[string]bool{protocol.MethodDiagnostic: true},
	})
	var got protocol.DocumentDiagnosticReport
	err := c.Call(context.Background(), protocol.MethodDiagnostic, nil, &got)
	if !jsonrpc.IsMethodNotFound(err) {
		t.Fatalf("expected a forced method-not-found, got %v", err)
	}
}

func TestCallerDelay(t *testing.T) {
	c := NewCaller(Scenario{Delay: map[string]time.Duration{protocol.MethodHover: 30 * time.Millisecond}})
	start := time.Now()
	if err := c.Call(context.Background(), protocol.MethodHover, nil, nil); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("elapsed = %s, want at least the scripted delay", elapsed)
	}
}

func TestCallerDelayRespectsContextCancellation(t *testing.T) {
	c := NewCaller(Scenario{Delay: map[string]time.Duration{protocol.MethodHover: 200 * time.Millisecond}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := c.Call(ctx, protocol.MethodHover, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the delay to be cut short by ctx, got %v", err)
	}
}

func TestCallerUnexpectedNotifications(t *testing.T) {
	c := NewCaller(Scenario{})
	if err := c.Notify(context.Background(), protocol.MethodDidOpen, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Notify(context.Background(), "textDocument/somethingWeird", nil); err != nil {
		t.Fatal(err)
	}
	got := c.UnexpectedNotifications()
	if len(got) != 1 || got[0] != "textDocument/somethingWeird" {
		t.Fatalf("UnexpectedNotifications() = %v, want exactly the one unrecognised method", got)
	}
}
