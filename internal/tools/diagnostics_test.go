package tools_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
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

// TestDiagnostics_PlainPath verifies a plain absolute path is normalised to the
// file:// URI the language server reports diagnostics under, so passing a path
// (or being aliased from file_path) resolves the same entry.
func TestDiagnostics_PlainPath(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/main.go", []protocol.Diagnostic{
		{
			Range:    protocol.Range{Start: protocol.Position{Line: 0, Character: 0}},
			Severity: protocol.SevError,
			Message:  "boom",
		},
	})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{"uri": "/p/main.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "boom") {
		t.Errorf("plain path should resolve to the file:// diagnostics key:\n%s", out)
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

func TestDiagnostics_URIsField_SingleFile(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/main.go", []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "type mismatch"},
	})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{"uris": []string{"file:///p/main.go"}})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "type mismatch") {
		t.Errorf("expected error in output, got: %q", out)
	}
}

func TestDiagnostics_URIsField_MultipleFiles(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/a.go", []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "syntax error in a"},
	})
	pushDiagnostics(t, inv, "file:///p/b.go", []protocol.Diagnostic{
		{Severity: protocol.SevWarning, Message: "unused import in b"},
	})
	pushDiagnostics(t, inv, "file:///p/c.go", []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "undefined: Foo in c"},
	})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{
		"uris": []string{"file:///p/a.go", "file:///p/b.go", "file:///p/c.go"},
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	for _, want := range []string{"syntax error in a", "unused import in b", "undefined: Foo in c"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "3 issue") {
		t.Errorf("expected 3 issues summary:\n%s", out)
	}
}

func TestDiagnostics_URIsField_MultipleFiles_OneUntracked(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/a.go", []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "broken"},
	})
	// b.go is never pushed, so it is untracked.

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{
		"uris": []string{"file:///p/a.go", "file:///p/b.go"},
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	// The tracked file's error must be present.
	if !strings.Contains(out, "broken") {
		t.Errorf("expected a.go error in output:\n%s", out)
	}
	// b.go has no diagnostics so it should not appear as an error entry.
	if !strings.Contains(out, "1 issue") {
		t.Errorf("expected 1 issue summary:\n%s", out)
	}
}

func TestDiagnostics_URIsField_MultipleFiles_AllClean(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/a.go", []protocol.Diagnostic{})
	pushDiagnostics(t, inv, "file:///p/b.go", []protocol.Diagnostic{})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{
		"uris": []string{"file:///p/a.go", "file:///p/b.go"},
	})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "clean") {
		t.Errorf("expected clean message, got: %q", out)
	}
}

// TestDiagnostics_Staleness verifies that formatDiagnosticsWithTimes emits a
// staleness note when a file's on-disk mtime is newer than the diagnostic
// timestamp stored in the invalidator.
func TestDiagnostics_Staleness(t *testing.T) {
	// Create a real temp file so os.Stat succeeds inside formatDiagnosticsWithTimes.
	f, err := os.CreateTemp(t.TempDir(), "stale*.go")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	uri := "file://" + path

	inv := newTestInvalidator(t)
	// Push diagnostics first — the invalidator records time.Now() as the
	// diagnostic timestamp.
	pushDiagnostics(t, inv, uri, []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "stale error"},
	})

	// Advance the file's mtime to after the diagnostic timestamp.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "modified after last analysis") {
		t.Errorf("expected staleness note in output:\n%s", out)
	}
	if !strings.Contains(out, "stale error") {
		t.Errorf("expected original error in output:\n%s", out)
	}
}

// TestDiagnostics_NoStaleness verifies that no staleness note appears when the
// file has not changed since the last diagnostic push.
func TestDiagnostics_NoStaleness(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "fresh*.go")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()

	// Set the file's mtime to the past before pushing diagnostics.
	past := time.Now().Add(-2 * time.Second)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}

	inv := newTestInvalidator(t)
	uri := "file://" + path
	pushDiagnostics(t, inv, uri, []protocol.Diagnostic{
		{Severity: protocol.SevError, Message: "fresh error"},
	})

	tool := tools.NewDiagnostics(inv)
	raw, _ := json.Marshal(map[string]any{})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if strings.Contains(out, "modified after last analysis") {
		t.Errorf("unexpected staleness note when file is current:\n%s", out)
	}
	if !strings.Contains(out, "fresh error") {
		t.Errorf("expected error in output:\n%s", out)
	}
}

func TestDiagnostics_BackwardCompat_ScalarURI(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/main.go", []protocol.Diagnostic{
		{
			Range:    protocol.Range{Start: protocol.Position{Line: 4, Character: 0}},
			Severity: protocol.SevError,
			Message:  "legacy call",
		},
	})

	tool := tools.NewDiagnostics(inv)
	// Old callers pass "uri" (scalar), not "uris" (array).
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.go"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "legacy call") {
		t.Errorf("expected legacy call in output, got: %q", out)
	}
}

// pullOpener is a fileOpener that also implements the pull-diagnostics
// capability (structurally matching the tool's internal pullDiagnoser), so a
// NewDiagnosticsWithOpener tool exercises the pull fallback for an untracked,
// push-empty file.
type pullOpener struct {
	supports bool
	report   *protocol.DocumentDiagnosticReport
	err      error
	pulled   string
}

func (p *pullOpener) DidOpen(context.Context, protocol.DidOpenTextDocumentParams) error   { return nil }
func (p *pullOpener) DidClose(context.Context, protocol.DidCloseTextDocumentParams) error { return nil }
func (p *pullOpener) SupportsPullDiagnostics() bool                                       { return p.supports }
func (p *pullOpener) Diagnostic(_ context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	p.pulled = params.TextDocument.URI
	return p.report, p.err
}

func TestDiagnostics_PullFallback_Reports(t *testing.T) {
	inv := newTestInvalidator(t) // empty: the file is untracked, push stream silent
	opener := &pullOpener{
		supports: true,
		report: &protocol.DocumentDiagnosticReport{
			Kind:  protocol.DiagnosticReportFull,
			Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "pulled error"}},
		},
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.zig"})
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if !strings.Contains(out, "pulled error") || !strings.Contains(out, "source=lsp-pull") {
		t.Errorf("expected pulled diagnostics with source label, got:\n%s", out)
	}
	if opener.pulled != "file:///p/main.zig" {
		t.Errorf("pull was not issued for the URI, got %q", opener.pulled)
	}
}

func TestDiagnostics_PullFallback_Clean(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &pullOpener{supports: true, report: &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.zig"})
	out, _ := tool.Execute(context.Background(), raw)
	if !strings.Contains(out, "clean") {
		t.Errorf("expected clean message from an empty pull report, got:\n%s", out)
	}
}

func TestDiagnostics_PullFallback_RelatedDocuments(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &pullOpener{
		supports: true,
		report: &protocol.DocumentDiagnosticReport{
			Kind: protocol.DiagnosticReportFull,
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				"file:///p/other.zig": {Kind: protocol.DiagnosticReportFull, Items: []protocol.Diagnostic{{Severity: protocol.SevWarning, Message: "related issue"}}},
			},
		},
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/main.zig"})
	out, _ := tool.Execute(context.Background(), raw)
	if !strings.Contains(out, "other.zig") || !strings.Contains(out, "related issue") {
		t.Errorf("expected related-document diagnostics in output, got:\n%s", out)
	}
}

func TestDiagnostics_NoPullWhenUnsupported(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &pullOpener{supports: false} // server does not advertise pull
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	raw, _ := json.Marshal(map[string]any{"uri": "file:///p/nope.zig"})
	_, _ = tool.Execute(context.Background(), raw)
	if opener.pulled != "" {
		t.Errorf("pull must not be issued when the server does not support it (pulled %q)", opener.pulled)
	}
}
