package cache_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// TestInvalidator_WaitDiagnostics_EarlyReturnMerged proves the T4-review fix:
// WaitDiagnostics' already-tracked early return must serve the deduplicated
// push+pull union (mergedLocked), not the push-only snapshot. A URI carrying
// both a pushed diagnostic and a distinct pulled one must return BOTH.
func TestInvalidator_WaitDiagnostics_EarlyReturnMerged(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	uri := "file:///p/main.go"
	pushDiag := protocol.Diagnostic{
		Range:    protocol.Range{Start: protocol.Position{Line: 1}},
		Severity: protocol.SevError,
		Message:  "pushed error",
	}
	pushDiagnosticsTo(t, inv, uri, []protocol.Diagnostic{pushDiag})
	inv.RecordPullFull(uri, "r1", []protocol.Diagnostic{{
		Range:    protocol.Range{Start: protocol.Position{Line: 2}},
		Severity: protocol.SevWarning,
		Message:  "pulled warning",
	}})

	diags, err := inv.WaitDiagnostics(context.Background(), uri)
	if err != nil {
		t.Fatalf("WaitDiagnostics: %v", err)
	}
	if len(diags) != 2 {
		t.Fatalf("expected merged push+pull (2 diagnostics), got %d: %#v", len(diags), diags)
	}
}

// TestInvalidator_WaitDiagnostics_PullOnlyDoesNotBlock proves that a URI with
// ONLY pull data is treated as tracked: WaitDiagnostics returns its snapshot
// immediately rather than blocking for a push that a pull-only server never
// sends.
func TestInvalidator_WaitDiagnostics_PullOnlyDoesNotBlock(t *testing.T) {
	c := cache.New(time.Hour)
	defer c.Close()
	inv := cache.NewInvalidator(c)

	uri := "file:///p/only.zig"
	inv.RecordPullFull(uri, "r1", []protocol.Diagnostic{{
		Severity: protocol.SevError,
		Message:  "pull-only error",
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	diags, err := inv.WaitDiagnostics(ctx, uri)
	if err != nil {
		t.Fatalf("WaitDiagnostics on a pull-tracked URI must not block/err: %v", err)
	}
	if len(diags) != 1 || diags[0].Message != "pull-only error" {
		t.Fatalf("expected the pulled diagnostic, got %#v", diags)
	}
}

func pushDiagnosticsTo(t *testing.T, inv *cache.Invalidator, uri string, diags []protocol.Diagnostic) {
	t.Helper()
	p := protocol.PublishDiagnosticsParams{URI: uri, Diagnostics: diags}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	inv.Handle(protocol.MethodPublishDiagnostics, b)
}
