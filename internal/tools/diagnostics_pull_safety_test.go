package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

func TestDiagnosticsPull_MultiURI_IncludesRelatedDocuments(t *testing.T) {
	inv := newTestInvalidator(t)
	requested := "file:///p/main.go"
	related := "file:///p/related.go"
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		rep := &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}
		if params.TextDocument.URI == requested {
			rep.RelatedDocuments = map[string]protocol.DocumentDiagnosticReport{
				related: {
					Kind:  protocol.DiagnosticReportFull,
					Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "related multi-file failure"}},
				},
			}
		}
		return rep, nil
	}

	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uris": []string{requested, "file:///p/other.go"}})
	if !strings.Contains(out, "related.go") || !strings.Contains(out, "related multi-file failure") {
		t.Fatalf("related diagnostics must be included in multi-file output:\n%s", out)
	}
}

func TestDiagnosticsPull_SingleURI_UnknownRelatedUnchangedIsUnverified(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind: protocol.DiagnosticReportFull,
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				"file:///p/related.go": {
					Kind:     protocol.DiagnosticReportUnchanged,
					ResultID: "unknown-related-result",
				},
			},
		}, nil
	}

	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": "file:///p/main.go"})
	if strings.Contains(out, "No issues") {
		t.Fatalf("an unvalidated related report must never read as clean:\n%s", out)
	}
	if !strings.Contains(out, "UNVERIFIED") || !strings.Contains(out, "related.go") {
		t.Fatalf("the unresolved related URI must be explicit:\n%s", out)
	}
}

func TestDiagnosticsPull_NoURI_UnknownWorkspaceUnchangedIsUnverified(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/known.go"
	inv.RecordPullFull(uri, "known-result", nil)
	opener := &modeOpener{defaultMode: "pull", wsPull: true}
	opener.wsRespond = func(protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
		return &protocol.WorkspaceDiagnosticReport{
			Items: []protocol.WorkspaceDocumentDiagnosticReport{{
				Kind:     protocol.DiagnosticReportUnchanged,
				URI:      uri,
				ResultID: "unknown-result",
			}},
		}, nil
	}

	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{})
	if strings.Contains(out, "No issues") {
		t.Fatalf("an unvalidated workspace report must never read as clean:\n%s", out)
	}
	if !strings.Contains(out, "UNVERIFIED") || !strings.Contains(out, "known.go") {
		t.Fatalf("the unresolved workspace URI must be explicit:\n%s", out)
	}
}

func TestDiagnosticsPush_UntrackedPullCapableOpenerNeverPulls(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "push"}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)

	raw, _ := json.Marshal(map[string]any{"uri": "file:///definitely/missing.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if opener.callCount() != 0 {
		t.Fatalf("push mode must never issue textDocument/diagnostic, got %d call(s); output:\n%s", opener.callCount(), out)
	}
}

func TestDiagnosticsPull_DowngradePreventsLaterPullFallback(t *testing.T) {
	inv := newTestInvalidator(t)
	first := "file:///p/tracked.go"
	second := "file:///definitely/missing-after-downgrade.go"
	pushDiagnostics(t, inv, first, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "push survives downgrade"}})
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		opener.mu.Lock()
		opener.defaultMode = "push"
		opener.mu.Unlock()
		return nil, context.Canceled
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)

	execDiagnostics(t, tool, map[string]any{"uri": first})
	execDiagnostics(t, tool, map[string]any{"uri": second})
	if opener.callCount() != 1 {
		t.Fatalf("after downgrade, later untracked push-mode queries must not pull; got %d calls", opener.callCount())
	}
}

func TestDiagnosticsPull_MultiURI_DeduplicatesSharedRelatedDocument(t *testing.T) {
	inv := newTestInvalidator(t)
	related := "file:///p/shared.go"
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind: protocol.DiagnosticReportFull,
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				related: {
					Kind:  protocol.DiagnosticReportFull,
					Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "shared related failure"}},
				},
			},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	args := map[string]any{"uris": []string{"file:///p/a.go", "file:///p/b.go"}}

	first := execDiagnostics(t, tool, args)
	second := execDiagnostics(t, tool, args)
	if first != second {
		t.Fatalf("shared related-document output must be deterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if got := strings.Count(first, "shared related failure"); got != 1 {
		t.Fatalf("shared related diagnostic rendered %d times, want once:\n%s", got, first)
	}
}
