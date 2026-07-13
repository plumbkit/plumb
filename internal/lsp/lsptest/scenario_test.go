package lsptest

import (
	"context"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

func TestCallerRejectsUnexpectedRequest(t *testing.T) {
	c := NewCaller(Scenario{})
	if err := c.Call(context.Background(), "unknown/method", nil, nil); err == nil {
		t.Fatal("expected unexpected request to fail")
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
