package tools

// diagnostics_ctx_aggregate_test.go — the whole-workspace (URI-less) diagnostics
// query goes through the ctx-aware aggregate when the source provides one, so a
// shared connection answers from the CALLING agent's project.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// stubCtxDiag is a diagnosticsSource that also implements ctxDiagnosticsSource,
// mirroring the daemon's routing inv proxy.
type stubCtxDiag struct {
	conn  map[string][]protocol.Diagnostic
	agent map[string][]protocol.Diagnostic
}

func (s stubCtxDiag) Diagnostics(string) []protocol.Diagnostic         { return nil }
func (s stubCtxDiag) AllDiagnostics() map[string][]protocol.Diagnostic { return s.conn }
func (s stubCtxDiag) Tracked(string) bool                              { return false }
func (s stubCtxDiag) AllDiagnosticsFor(context.Context) map[string][]protocol.Diagnostic {
	return s.agent
}
func (s stubCtxDiag) AllDiagnosticTimesFor(context.Context) map[string]time.Time { return nil }
func (s stubCtxDiag) WaitDiagnostics(context.Context, string) ([]protocol.Diagnostic, error) {
	return nil, nil
}

// TestDiagnostics_AllFilesUsesTheCtxAwareAggregate pins the whole-workspace
// path's preference for AllDiagnosticsFor, which is what scopes a shared
// connection's URI-less query to the calling agent's project.
func TestDiagnostics_AllFilesUsesTheCtxAwareAggregate(t *testing.T) {
	connURI := "file:///w/conn.go"
	agentURI := "file:///w/agent.go"
	tool := NewDiagnostics(stubCtxDiag{
		conn:  map[string][]protocol.Diagnostic{connURI: {{Severity: protocol.SevError, Message: "conn err"}}},
		agent: map[string][]protocol.Diagnostic{agentURI: {{Severity: protocol.SevError, Message: "agent err"}}},
	})

	out := tool.allFilesCached(context.Background())
	if !strings.Contains(out, "agent.go") {
		t.Errorf("whole-workspace diagnostics did not use the ctx-aware aggregate:\n%s", out)
	}
	if strings.Contains(out, "conn.go") {
		t.Errorf("whole-workspace diagnostics used the connection aggregate:\n%s", out)
	}
}

// TestDiagnostics_AllFilesFallsBackWithoutCtxSource keeps the plain
// *cache.Invalidator path byte-for-byte: a source with no ctx-aware aggregate
// still answers AllDiagnostics().
func TestDiagnostics_AllFilesFallsBackWithoutCtxSource(t *testing.T) {
	uri := "file:///w/plain.go"
	tool := NewDiagnostics(stubPlainDiag{
		all: map[string][]protocol.Diagnostic{uri: {{Severity: protocol.SevError, Message: "plain err"}}},
	})
	out := tool.allFilesCached(context.Background())
	if !strings.Contains(out, "plain.go") {
		t.Errorf("fallback aggregate lost the diagnostics:\n%s", out)
	}
}

type stubPlainDiag struct {
	all map[string][]protocol.Diagnostic
}

func (s stubPlainDiag) Diagnostics(string) []protocol.Diagnostic         { return nil }
func (s stubPlainDiag) AllDiagnostics() map[string][]protocol.Diagnostic { return s.all }
func (s stubPlainDiag) Tracked(string) bool                              { return false }
func (s stubPlainDiag) WaitDiagnostics(context.Context, string) ([]protocol.Diagnostic, error) {
	return nil, nil
}
