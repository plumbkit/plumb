package tools_test

// diagnostics_pull_ctx_test.go — the pull-record path prefers the ctx-aware
// surface when the diagnostics source provides one (issue #499).
//
// The ctx names the CALLING logical agent, and that is the only thing that can
// tell "my workspace" from "a peer agent's workspace" on a shared connection: a
// server-supplied relatedDocuments key under the peer's root is admitted by the
// peer's own policy, so a ctx-free source can only union the two and record the
// report into the wrong cache.

import (
	"context"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// pullCtxKey marks the pull ctx so the test can prove the CALLER's ctx — not
// some fresh background one — reached the record surface.
type pullCtxKey struct{}

// ctxPullSource is a diagnostics source that implements BOTH pull-record
// surfaces and records which one a pull used.
type ctxPullSource struct {
	used   string // "ctx" or "plain"; "" when neither was reached
	ctxVal any
	items  []protocol.Diagnostic
}

func (s *ctxPullSource) Diagnostics(string) []protocol.Diagnostic { return s.items }
func (s *ctxPullSource) AllDiagnostics() map[string][]protocol.Diagnostic {
	return nil
}
func (s *ctxPullSource) Tracked(string) bool { return false }
func (s *ctxPullSource) WaitDiagnostics(context.Context, string) ([]protocol.Diagnostic, error) {
	return nil, nil
}

func (s *ctxPullSource) PullResultID(string) (string, bool) {
	s.used = "plain"
	return "", false
}

func (s *ctxPullSource) RecordPullResult(string, protocol.DocumentDiagnosticReport) (applied, unresolved []string) {
	s.used = "plain"
	return nil, nil
}
func (s *ctxPullSource) AllPullResultIDs() []protocol.PreviousResultID { return nil }

func (s *ctxPullSource) PullResultIDFor(ctx context.Context, _ string) (string, bool) {
	s.note(ctx)
	return "", false
}

func (s *ctxPullSource) PullGenerationFor(ctx context.Context, _ string) uint64 {
	s.note(ctx)
	return 0
}

func (s *ctxPullSource) RecordPullResultFor(ctx context.Context, uri string, rep protocol.DocumentDiagnosticReport) (applied, unresolved []string) {
	s.note(ctx)
	s.items = rep.Items
	return []string{uri}, nil
}

func (s *ctxPullSource) RecordPullResultAtFor(ctx context.Context, uri string, rep protocol.DocumentDiagnosticReport, _ uint64) (applied, unresolved []string) {
	s.note(ctx)
	s.items = rep.Items
	return []string{uri}, nil
}

func (s *ctxPullSource) note(ctx context.Context) {
	s.used = "ctx"
	s.ctxVal = ctx.Value(pullCtxKey{})
}

// TestDiagnosticsPull_UsesTheCtxAwareRecordSurface pins the preference: the
// record call is made through the ctx-aware surface, with the CALLER's ctx.
func TestDiagnosticsPull_UsesTheCtxAwareRecordSurface(t *testing.T) {
	src := &ctxPullSource{}
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "r1",
			Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "ctx boom"}},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(src, opener)

	ctx := context.WithValue(context.Background(), pullCtxKey{}, "caller-sentinel")
	raw := []byte(`{"uri":"file:///w/main.go"}`)
	out, err := tool.Execute(ctx, raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	if src.used != "ctx" {
		t.Fatalf("pull record surface = %q, want the ctx-aware one", src.used)
	}
	if src.ctxVal != "caller-sentinel" {
		t.Fatalf("record surface saw ctx value %v, want the CALLER's ctx (a fresh background ctx would carry none)", src.ctxVal)
	}
	if !strings.Contains(out, "ctx boom") {
		t.Fatalf("the recorded report was not rendered:\n%s", out)
	}
}

// plainPullSource implements only the ctx-less surface, mirroring
// *cache.Invalidator. The pull path must degrade to it rather than requiring
// the ctx-aware extension.
type plainPullSource struct {
	recorded bool
	items    []protocol.Diagnostic
}

func (s *plainPullSource) Diagnostics(string) []protocol.Diagnostic { return s.items }
func (s *plainPullSource) AllDiagnostics() map[string][]protocol.Diagnostic {
	return nil
}
func (s *plainPullSource) Tracked(string) bool { return false }
func (s *plainPullSource) WaitDiagnostics(context.Context, string) ([]protocol.Diagnostic, error) {
	return nil, nil
}
func (s *plainPullSource) PullResultID(string) (string, bool) { return "", false }
func (s *plainPullSource) RecordPullResult(uri string, rep protocol.DocumentDiagnosticReport) (applied, unresolved []string) {
	s.recorded = true
	s.items = rep.Items
	return []string{uri}, nil
}
func (s *plainPullSource) AllPullResultIDs() []protocol.PreviousResultID { return nil }

func TestDiagnosticsPull_FallsBackWithoutTheCtxSurface(t *testing.T) {
	src := &plainPullSource{}
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "r1",
			Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "plain boom"}},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(src, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": "file:///w/main.go"})
	if !src.recorded {
		t.Fatal("a source without the ctx-aware surface was not recorded through the plain one")
	}
	if !strings.Contains(out, "plain boom") {
		t.Fatalf("the recorded report was not rendered:\n%s", out)
	}
}
