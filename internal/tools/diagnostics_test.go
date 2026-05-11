package tools_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/tools"
)

func newTestInvalidator(t *testing.T) *cache.Invalidator {
	t.Helper()
	c := cache.New(5 * time.Minute)
	t.Cleanup(c.Close)
	return cache.NewInvalidator(c)
}

func pushDiagnostics(t *testing.T, inv *cache.Invalidator, uri string, diags []protocol.Diagnostic) {
	t.Helper()
	p := protocol.PublishDiagnosticsParams{URI: uri, Diagnostics: diags}
	b, _ := json.Marshal(p)
	inv.Handle(protocol.MethodPublishDiagnostics, b)
}

func TestDiagnostics_SpecificURI(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/main.go", []protocol.Diagnostic{
		{
			Range:    protocol.Range{Start: protocol.Position{Line: 9, Character: 4}},
			Severity: protocol.SevError,
			Message:  "undefined: Greeter",
		},
	})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	for _, want := range []string{"ERROR", "10:5", "undefined: Greeter"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDiagnostics_AllFiles(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/a.go", []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "syntax error"},
	})
	pushDiagnostics(t, inv, "file:///p/b.go", []protocol.Diagnostic{
		{Severity: protocol.SevWarning, Message: "unused import"},
	})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "a.go") || !strings.Contains(out, "b.go") {
		t.Errorf("expected both files in output:\n%s", out)
	}
	if !strings.Contains(out, "2 issue") {
		t.Errorf("expected 2 issues summary:\n%s", out)
	}
}

func TestDiagnostics_Empty(t *testing.T) {
	inv := newTestInvalidator(t)
	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "No diagnostics") {
		t.Errorf("expected empty message, got: %q", out)
	}
}

func TestDiagnostics_CleanFile(t *testing.T) {
	inv := newTestInvalidator(t)
	// gopls sends an empty slice when a file becomes clean.
	pushDiagnostics(t, inv, "file:///p/main.go", []protocol.Diagnostic{})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "clean") {
		t.Errorf("expected clean message, got: %q", out)
	}
}
