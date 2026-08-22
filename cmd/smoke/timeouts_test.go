//go:build integration

package smoke_test

import (
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

// TestLSPToolTimeoutOutlastsServer is the PLAN-390 re-inversion guard.
//
// The client-side budget for the first LSP-backed tool call must exceed the
// SERVER-side [lsp_query] deadline the daemon applies to that same call.
// Inverted — as it was, at toolTimeout=20s against a 30s server deadline — the
// client abandons the request first, so nothing the daemon does in its last 10s
// can help: not its tree-sitter fallback, not its actionable "still indexing"
// message. The test then fails as a bare transport timeout, indistinguishable
// from a product bug, which is exactly how this flake cost a log inspection on
// eleven unrelated CI runs.
//
// This asserts the RELATIONSHIP, not either number: raise the [lsp_query]
// default or lower lspToolTimeout past it and this goes red rather than the
// integration job going flaky again weeks later.
func TestLSPToolTimeoutOutlastsServer(t *testing.T) {
	serverDeadline := config.Defaults().LSPQuery.Timeout.Duration
	if serverDeadline <= 0 {
		t.Fatalf("the default [lsp_query] timeout is %v — with no server-side deadline "+
			"there is nothing for the client budget to outlast, and this guard is vacuous",
			serverDeadline)
	}
	if lspToolTimeout <= serverDeadline {
		t.Fatalf("lspToolTimeout (%v) must exceed the default [lsp_query] deadline (%v): "+
			"a client that gives up first can never observe the daemon's own degradation "+
			"(tree-sitter fallback or timeout message), so a slow cold start surfaces as a "+
			"bare transport timeout — see PLAN-390",
			lspToolTimeout, serverDeadline)
	}
	// toolTimeout stays deliberately tight for warm calls; the point of the
	// separate constant is that the cold-start call is the only one exempted.
	if toolTimeout > lspToolTimeout {
		t.Fatalf("toolTimeout (%v) exceeds lspToolTimeout (%v) — the cold-start budget "+
			"must be the more generous of the two", toolTimeout, lspToolTimeout)
	}
}
