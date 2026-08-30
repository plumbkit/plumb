package cli

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// neverAnsweringRequest models a client that accepts the roots/list request
// and never replies — the raw-MCP-harness behaviour that stalled the attach
// ladder for a tool call's whole budget before the bound existed (PR #435).
func neverAnsweringRequest(ctx context.Context, _ string, _ any) (json.RawMessage, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestRootsFromClient_BoundReturnsUnderCallerDeadline proves the bound itself:
// with a caller context far larger than the probe bound, a never-answering
// client must return within the bound, not the caller's deadline. Shortening
// rootsListProbeTimeout is only possible because it is a var; deleting the
// context.WithTimeout in rootsFromClient makes this test hang to the caller's
// deadline and fail — the guard is the test, not the const.
func TestRootsFromClient_BoundReturnsUnderCallerDeadline(t *testing.T) {
	old := rootsListProbeTimeout
	rootsListProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { rootsListProbeTimeout = old })

	callerCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	start := time.Now()
	roots := rootsFromClient(callerCtx, neverAnsweringRequest, testLogger(t))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("rootsFromClient returned after %v — the probe bound was ignored and the caller's deadline was consumed", elapsed)
	}
	if len(roots) != 0 {
		t.Fatalf("roots = %v, want nil from a client that never answered", roots)
	}
}

// TestRootsFromClient_TimeoutLogIsDistinguishableFromUnsupported pins the log
// wording split: a probe that outruns the bound logs the timeout line, and a
// client that answers with an error keeps the historical "not supported"
// line — an operator tailing the daemon log must be able to tell a hung
// client from one without roots capability.
func TestRootsFromClient_TimeoutLogIsDistinguishableFromUnsupported(t *testing.T) {
	old := rootsListProbeTimeout
	rootsListProbeTimeout = 30 * time.Millisecond
	t.Cleanup(func() { rootsListProbeTimeout = old })

	var b strings.Builder
	logger := slog.New(slog.NewTextHandler(&b, nil))
	rootsFromClient(context.Background(), neverAnsweringRequest, logger)
	if !strings.Contains(b.String(), "roots/list probe timed out") {
		t.Fatalf("timeout case logged %q, want the dedicated timeout line", b.String())
	}

	b.Reset()
	failing := func(context.Context, string, any) (json.RawMessage, error) {
		return nil, errors.New("method not found")
	}
	rootsFromClient(context.Background(), failing, logger)
	if !strings.Contains(b.String(), "roots/list not supported by client") {
		t.Fatalf("error case logged %q, want the historical not-supported line", b.String())
	}
	if strings.Contains(b.String(), "probe timed out") {
		t.Fatalf("error case wrongly logged as a timeout: %q", b.String())
	}
}

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(&testWriter{t: t}, nil))
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
