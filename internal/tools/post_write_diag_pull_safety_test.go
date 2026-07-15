package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestPullPostWrite_UnknownRelatedUnchangedNeverEmitsCleanPass(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind: protocol.DiagnosticReportFull,
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				"file:///ws/related.go": {
					Kind:     protocol.DiagnosticReportUnchanged,
					ResultID: "unknown-related-result",
				},
			},
		}, nil
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)
	if strings.Contains(out, "✓ fresh diagnostics pass") {
		t.Fatalf("an unvalidated related report must suppress the clean pass:\n%s", out)
	}
	if !strings.Contains(out, "unverified") {
		t.Fatalf("the incomplete pull must be explicit:\n%s", out)
	}
}

func TestPullPostWrite_UnknownWorkspaceUnchangedIsNotExhaustive(t *testing.T) {
	inv := newPullInv(t)
	otherURI := "file:///ws/other.go"
	inv.RecordPullFull(otherURI, "known-result", nil)
	client := &pullModeLSP{mode: "pull", interFile: true, wsPull: true}
	client.wsRespond = func(protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
		return &protocol.WorkspaceDiagnosticReport{
			Items: []protocol.WorkspaceDocumentDiagnosticReport{{
				Kind:     protocol.DiagnosticReportUnchanged,
				URI:      otherURI,
				ResultID: "unknown-result",
			}},
		}, nil
	}
	d := WriteDeps{
		Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond,
		CrossFileDiag: true, WorkspaceFn: func() string { return "/ws" },
	}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)
	if strings.Contains(out, "✓ fresh diagnostics pass") {
		t.Fatalf("an unvalidated workspace report must suppress the clean pass:\n%s", out)
	}
	if !strings.Contains(out, "incomplete") || !strings.Contains(out, "unverified") {
		t.Fatalf("the workspace sweep must be explicitly incomplete:\n%s", out)
	}
}
