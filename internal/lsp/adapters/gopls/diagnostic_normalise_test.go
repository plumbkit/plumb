package gopls

import (
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestNormaliseDiagnosticReport_GoplsCleanWireReportIsFull(t *testing.T) {
	report := &protocol.DocumentDiagnosticReport{}
	normaliseDiagnosticReport(report)
	if report.Kind != protocol.DiagnosticReportFull {
		t.Fatalf("kind = %q, want full", report.Kind)
	}
}

func TestNormaliseDiagnosticReport_PreservesExplicitKind(t *testing.T) {
	for _, kind := range []string{protocol.DiagnosticReportFull, protocol.DiagnosticReportUnchanged, "future-kind"} {
		report := &protocol.DocumentDiagnosticReport{Kind: kind}
		normaliseDiagnosticReport(report)
		if report.Kind != kind {
			t.Errorf("kind %q changed to %q", kind, report.Kind)
		}
	}
	normaliseDiagnosticReport(nil)
}
