package tools_test

// diagnostics_pull_test.go — mode-aware behaviour of the diagnostics tool:
// single-URI pulls (result IDs, unchanged, the unknown-ID retry rule), the
// downgrade fallback, bounded multi-URI pulls, the no-URI workspace pull and
// its honest-note fallback, and — non-negotiably — the SAFETY INVARIANT tests:
// a pull failure must NEVER yield a false "No issues" result.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/tools"
)

// modeOpener is a fileOpener that additionally implements the tool's
// mode-aware pull surface (DiagnosticsMode, Diagnostic, DiagnosticCapabilities,
// WorkspaceDiagnostic). Behaviour is scripted per test via respond/ws fields.
type modeOpener struct {
	mu sync.Mutex

	// modes maps URI → resolved mode; "" key covers the no-URI (primary) query.
	// defaultMode applies to URIs not in the map.
	modes       map[string]string
	defaultMode string

	// respond scripts Diagnostic. calls records every request in order.
	respond func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
	calls   []protocol.DocumentDiagnosticParams

	// inflight tracking for the bounded-concurrency test.
	inflight, peakInflight int

	// workspace pull scripting.
	interFile bool
	wsPull    bool
	wsRespond func(params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error)
	wsCalls   []protocol.WorkspaceDiagnosticParams
}

func (m *modeOpener) DidOpen(context.Context, protocol.DidOpenTextDocumentParams) error   { return nil }
func (m *modeOpener) DidClose(context.Context, protocol.DidCloseTextDocumentParams) error { return nil }
func (m *modeOpener) SupportsPullDiagnostics() bool                                       { return true }

func (m *modeOpener) DiagnosticsMode(uri string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mode, ok := m.modes[uri]; ok {
		return mode
	}
	return m.defaultMode
}

func (m *modeOpener) setMode(uri, mode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.modes == nil {
		m.modes = map[string]string{}
	}
	m.modes[uri] = mode
}

func (m *modeOpener) Diagnostic(_ context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	m.mu.Lock()
	m.calls = append(m.calls, params)
	m.inflight++
	if m.inflight > m.peakInflight {
		m.peakInflight = m.inflight
	}
	respond := m.respond
	m.mu.Unlock()

	// Give concurrent workers a chance to overlap so peakInflight is meaningful.
	time.Sleep(5 * time.Millisecond)

	m.mu.Lock()
	m.inflight--
	m.mu.Unlock()
	if respond == nil {
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}, nil
	}
	return respond(params)
}

func (m *modeOpener) DiagnosticCapabilities(string) (bool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.interFile, m.wsPull
}

func (m *modeOpener) WorkspaceDiagnostic(_ context.Context, _ string, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	m.mu.Lock()
	m.wsCalls = append(m.wsCalls, params)
	respond := m.wsRespond
	m.mu.Unlock()
	if respond == nil {
		return &protocol.WorkspaceDiagnosticReport{}, nil
	}
	return respond(params)
}

func (m *modeOpener) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *modeOpener) callAt(i int) protocol.DocumentDiagnosticParams {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[i]
}

func execDiagnostics(t *testing.T, tool *tools.Diagnostics, args map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(args)
	out, err := tool.Execute(context.Background(), raw)
	if err != nil {
		t.Fatalf("diagnostics: %v", err)
	}
	return out
}

// ─── Single URI: pull mode happy paths ───────────────────────────────────────

func TestDiagnosticsPull_SingleURI_FullReport(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "r1",
			Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "pulled boom"}},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": "file:///p/main.go"})
	if !strings.Contains(out, "pulled boom") || !strings.Contains(out, "source=lsp-pull") {
		t.Errorf("expected pulled diagnostics with source label, got:\n%s", out)
	}
	// The result was recorded: the cache now serves it and holds the result ID.
	if got := inv.Diagnostics("file:///p/main.go"); len(got) != 1 {
		t.Errorf("expected the pull to be recorded in the cache, got %d diags", len(got))
	}
	if id, ok := inv.PullResultID("file:///p/main.go"); !ok || id != "r1" {
		t.Errorf("expected result ID r1 recorded, got %q ok=%v", id, ok)
	}
}

func TestDiagnosticsPull_SingleURI_SendsPreviousResultID(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull, ResultID: "r1"}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	execDiagnostics(t, tool, map[string]any{"uri": "file:///p/main.go"})
	execDiagnostics(t, tool, map[string]any{"uri": "file:///p/main.go"})
	if opener.callCount() != 2 {
		t.Fatalf("expected 2 pulls, got %d", opener.callCount())
	}
	if got := opener.callAt(0).PreviousResultID; got != "" {
		t.Errorf("first pull must have no previousResultId, got %q", got)
	}
	if got := opener.callAt(1).PreviousResultID; got != "r1" {
		t.Errorf("second pull should carry previousResultId r1, got %q", got)
	}
}

func TestDiagnosticsPull_SingleURI_UnchangedServesCache(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	// Prime the cache with a recorded full pull (result ID r1).
	inv.RecordPullFull(uri, "r1", []protocol.Diagnostic{{Severity: protocol.SevError, Message: "cached finding"}})
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r1"}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if !strings.Contains(out, "cached finding") {
		t.Errorf("expected the validated cached snapshot to be served, got:\n%s", out)
	}
	if opener.callCount() != 1 {
		t.Errorf("a matching unchanged answer must not trigger a retry, got %d calls", opener.callCount())
	}
}

func TestDiagnosticsPull_SingleURI_UnchangedClean(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	inv.RecordPullFull(uri, "r1", nil) // clean full previously recorded
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r1"}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if !strings.Contains(out, "No issues found") {
		t.Errorf("a validated unchanged over a clean snapshot is a genuine clean, got:\n%s", out)
	}
}

func TestDiagnosticsPull_SingleURI_RelatedDocuments(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind: protocol.DiagnosticReportFull,
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				"file:///p/other.go": {
					Kind:  protocol.DiagnosticReportFull,
					Items: []protocol.Diagnostic{{Severity: protocol.SevWarning, Message: "related issue"}},
				},
			},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": "file:///p/main.go"})
	if !strings.Contains(out, "other.go") || !strings.Contains(out, "related issue") {
		t.Errorf("expected related-document diagnostics folded into the output, got:\n%s", out)
	}
	if got := inv.Diagnostics("file:///p/other.go"); len(got) != 1 {
		t.Errorf("expected the related document recorded in the cache, got %d", len(got))
	}
}

// The unknown-result-ID rule: an unchanged answer that does not match the
// stored result ID mutates nothing and triggers exactly one retry WITHOUT a
// previousResultId; a full report from the retry is trusted.
func TestDiagnosticsPull_SingleURI_UnknownID_RetryWithoutPrevID(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	inv.RecordPullFull(uri, "r1", []protocol.Diagnostic{{Severity: protocol.SevError, Message: "old finding"}})
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		if params.PreviousResultID != "" {
			// First call (with r1): answer unchanged with an ID we never issued.
			return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r999"}, nil
		}
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "r2",
			Items:    []protocol.Diagnostic{{Severity: protocol.SevError, Message: "fresh finding"}},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if opener.callCount() != 2 {
		t.Fatalf("expected exactly one retry (2 calls), got %d", opener.callCount())
	}
	if got := opener.callAt(1).PreviousResultID; got != "" {
		t.Errorf("the retry must NOT carry a previousResultId, got %q", got)
	}
	if !strings.Contains(out, "fresh finding") {
		t.Errorf("expected the retried full report to be served, got:\n%s", out)
	}
	if id, _ := inv.PullResultID(uri); id != "r2" {
		t.Errorf("expected the retry's result ID recorded, got %q", id)
	}
}

// ─── SAFETY INVARIANT: a pull failure must never read as a false clean ───────

func TestDiagnosticsPull_Safety_ErrorWithCleanCache(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	// The cache says clean (an empty push snapshot: tracked, no findings).
	pushDiagnostics(t, inv, uri, []protocol.Diagnostic{})
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return nil, errors.New("server exploded")
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if strings.Contains(out, "No issues") {
		t.Errorf("SAFETY: a failed pull must never present a clean cache as \"No issues\":\n%s", out)
	}
	if !strings.Contains(out, "server exploded") {
		t.Errorf("SAFETY: the pull error must be visible in the output:\n%s", out)
	}
}

func TestDiagnosticsPull_Safety_ErrorWithDirtyCache(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	pushDiagnostics(t, inv, uri, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "standing error"}})
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return nil, errors.New("pull timed out")
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if !strings.Contains(out, "pull timed out") {
		t.Errorf("SAFETY: the pull error must be visible in the output:\n%s", out)
	}
	if !strings.Contains(out, "standing error") {
		t.Errorf("SAFETY: prior cached diagnostics must still be shown on a failed pull:\n%s", out)
	}
	if !strings.Contains(out, "stale") {
		t.Errorf("expected the cached findings to be labelled possibly stale:\n%s", out)
	}
}

func TestDiagnosticsPull_Safety_UnknownID_RetryFails(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	inv.RecordPullFull(uri, "r1", nil) // clean snapshot with a known result ID
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		// Always answer unchanged with an ID we never issued — including the
		// retry, which makes the whole pull untrustworthy.
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r999"}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if opener.callCount() != 2 {
		t.Fatalf("expected exactly one retry (2 calls), got %d", opener.callCount())
	}
	if strings.Contains(out, "No issues") {
		t.Errorf("SAFETY: an unvalidatable unchanged answer must not read as clean:\n%s", out)
	}
	if !strings.Contains(out, "full report") {
		t.Errorf("expected an explicit degradation note about the failed retry:\n%s", out)
	}
}

func TestDiagnosticsPull_Safety_UnrecognisedKind(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	pushDiagnostics(t, inv, uri, []protocol.Diagnostic{})
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{Kind: "sideways"}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if strings.Contains(out, "No issues") {
		t.Errorf("SAFETY: an unrecognised report kind must not read as clean:\n%s", out)
	}
	if !strings.Contains(out, "sideways") {
		t.Errorf("expected the unrecognised kind surfaced in the degradation note:\n%s", out)
	}
}

// ─── Downgrade fallback ──────────────────────────────────────────────────────

// When the proxy downgrades the connection to push mid-call (-32601), the tool
// falls back to the push machinery without surfacing a degradation note — the
// connection is simply a push connection from now on.
func TestDiagnosticsPull_DowngradeFallsBackToPush(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	pushDiagnostics(t, inv, uri, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "push finding"}})
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		opener.setMode(uri, "push") // the routing proxy flipped the entry
		return nil, errors.New("jsonrpc error -32601: method not found")
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if !strings.Contains(out, "push finding") {
		t.Errorf("expected the push-path result after a downgrade, got:\n%s", out)
	}
	if strings.Contains(out, "failed") || strings.Contains(out, "UNVERIFIED") {
		t.Errorf("a downgrade is a clean fallback, not a degradation:\n%s", out)
	}
}

// ─── Push mode stays byte-identical ──────────────────────────────────────────

func TestDiagnosticsPull_PushModeNeverPulls(t *testing.T) {
	inv := newTestInvalidator(t)
	uri := "file:///p/main.go"
	pushDiagnostics(t, inv, uri, []protocol.Diagnostic{{Severity: protocol.SevError, Message: "pushed"}})
	opener := &modeOpener{defaultMode: "push"}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uri": uri})
	if opener.callCount() != 0 {
		t.Errorf("push mode must never issue a pull, got %d calls", opener.callCount())
	}
	if !strings.Contains(out, "pushed") {
		t.Errorf("expected the push-cache result, got:\n%s", out)
	}
}

// ─── Multi-URI ───────────────────────────────────────────────────────────────

func TestDiagnosticsPull_MultiURI_MixedModes(t *testing.T) {
	inv := newTestInvalidator(t)
	pullURI := "file:///p/a.go"
	pushURI := "file:///p/b.go"
	pushDiagnostics(t, inv, pushURI, []protocol.Diagnostic{{Severity: protocol.SevWarning, Message: "push warning"}})
	opener := &modeOpener{defaultMode: "push", modes: map[string]string{pullURI: "pull"}}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:  protocol.DiagnosticReportFull,
			Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "pulled error"}},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uris": []string{pullURI, pushURI}})
	if opener.callCount() != 1 {
		t.Fatalf("expected exactly the pull-mode URI pulled, got %d calls", opener.callCount())
	}
	if got := opener.callAt(0).TextDocument.URI; got != pullURI {
		t.Errorf("pulled the wrong URI: %q", got)
	}
	for _, want := range []string{"pulled error", "push warning", "2 issue"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDiagnosticsPull_MultiURI_BoundedConcurrency(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "pull"}
	uris := []string{
		"file:///p/a.go", "file:///p/b.go", "file:///p/c.go", "file:///p/d.go",
		"file:///p/e.go", "file:///p/f.go", "file:///p/g.go", "file:///p/h.go",
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	execDiagnostics(t, tool, map[string]any{"uris": uris})
	if opener.callCount() != len(uris) {
		t.Fatalf("expected %d pulls, got %d", len(uris), opener.callCount())
	}
	if opener.peakInflight > 4 {
		t.Errorf("concurrency must be capped at 4, observed peak %d", opener.peakInflight)
	}
}

func TestDiagnosticsPull_MultiURI_DeterministicOutput(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:  protocol.DiagnosticReportFull,
			Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "err in " + params.TextDocument.URI}},
		}, nil
	}
	uris := []string{"file:///p/c.go", "file:///p/a.go", "file:///p/b.go"}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	first := execDiagnostics(t, tool, map[string]any{"uris": uris})
	second := execDiagnostics(t, tool, map[string]any{"uris": uris})
	if first != second {
		t.Errorf("multi-URI output must be deterministic:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestDiagnosticsPull_MultiURI_Safety_PartialFailure(t *testing.T) {
	inv := newTestInvalidator(t)
	okURI := "file:///p/ok.go"
	badURI := "file:///p/bad.go"
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		if params.TextDocument.URI == badURI {
			return nil, errors.New("bad file pull broke")
		}
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uris": []string{okURI, badURI}})
	if !strings.Contains(out, "bad file pull broke") {
		t.Errorf("SAFETY: the per-URI failure must be visible:\n%s", out)
	}
	if !strings.Contains(out, "UNVERIFIED") {
		t.Errorf("SAFETY: failed files must be marked unverified:\n%s", out)
	}
	if strings.Contains(out, "all tracked files are clean") {
		t.Errorf("SAFETY: no bare all-clean claim over unverified files:\n%s", out)
	}
}

// A mid-batch -32601 downgrade must not let the downgraded URI vanish into an
// all-clean report: the single-URI path falls back to push open-and-wait, but
// a batch entry is surfaced as UNVERIFIED for the call instead (the safety
// invariant — an unpulled, uncached file is never reported clean).
func TestDiagnosticsPull_MultiURI_DowngradeNeverFalseClean(t *testing.T) {
	inv := newTestInvalidator(t)
	downURI := "file:///p/downgrades.go"
	cleanURI := "file:///p/clean.go"
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		if params.TextDocument.URI == downURI {
			opener.setMode(downURI, "push") // the routing proxy flipped the entry
			return nil, errors.New("jsonrpc error -32601: method not found")
		}
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{"uris": []string{downURI, cleanURI}})
	if strings.Contains(out, "all tracked files are clean") {
		t.Fatalf("SAFETY: the downgraded URI vanished into an all-clean report:\n%s", out)
	}
	if !strings.Contains(out, "UNVERIFIED") || !strings.Contains(out, "downgrades.go") {
		t.Errorf("the downgraded file must be surfaced as unverified:\n%s", out)
	}
}

func TestDiagnosticsPull_MultiURI_CancelledContext(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "pull"}
	opener.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return nil, context.Canceled
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	raw, _ := json.Marshal(map[string]any{"uris": []string{"file:///p/a.go", "file:///p/b.go", "file:///p/c.go"}})
	done := make(chan string, 1)
	go func() {
		out, _ := tool.Execute(ctx, raw)
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "UNVERIFIED") {
			t.Errorf("cancelled pulls must be reported as unverified:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("multi-URI pull hung on a cancelled context")
	}
}

// ─── No-URI: workspace pull and the honest note ──────────────────────────────

func TestDiagnosticsPull_NoURI_WorkspacePull(t *testing.T) {
	inv := newTestInvalidator(t)
	inv.RecordPullFull("file:///p/known.go", "r7", nil)
	opener := &modeOpener{defaultMode: "pull", wsPull: true}
	opener.wsRespond = func(params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
		return &protocol.WorkspaceDiagnosticReport{
			Items: []protocol.WorkspaceDocumentDiagnosticReport{
				{
					Kind:  protocol.DiagnosticReportFull,
					URI:   "file:///p/broken.go",
					Items: []protocol.Diagnostic{{Severity: protocol.SevError, Message: "workspace finding"}},
				},
			},
		}, nil
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{})
	if len(opener.wsCalls) != 1 {
		t.Fatalf("expected one workspace pull, got %d", len(opener.wsCalls))
	}
	prev := opener.wsCalls[0].PreviousResultIDs
	if len(prev) != 1 || prev[0].URI != "file:///p/known.go" || prev[0].Value != "r7" {
		t.Errorf("expected previousResultIds from the cache, got %#v", prev)
	}
	if !strings.Contains(out, "workspace finding") {
		t.Errorf("expected the workspace pull's findings served:\n%s", out)
	}
	if got := inv.Diagnostics("file:///p/broken.go"); len(got) != 1 {
		t.Errorf("expected the workspace report recorded in the cache, got %d", len(got))
	}
}

func TestDiagnosticsPull_NoURI_HonestNoteWithoutWorkspaceSupport(t *testing.T) {
	inv := newTestInvalidator(t)
	inv.RecordPullFull("file:///p/seen.go", "r1", []protocol.Diagnostic{{Severity: protocol.SevError, Message: "seen error"}})
	opener := &modeOpener{defaultMode: "pull", wsPull: false}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{})
	if len(opener.wsCalls) != 0 {
		t.Errorf("workspace pull must not be issued without the capability, got %d calls", len(opener.wsCalls))
	}
	if !strings.Contains(out, "seen error") {
		t.Errorf("cached results must still be served:\n%s", out)
	}
	if !strings.Contains(out, "NOT verified") {
		t.Errorf("expected the honest completeness note:\n%s", out)
	}
}

func TestDiagnosticsPull_NoURI_WorkspacePullError(t *testing.T) {
	inv := newTestInvalidator(t)
	inv.RecordPullFull("file:///p/seen.go", "r1", []protocol.Diagnostic{{Severity: protocol.SevError, Message: "seen error"}})
	opener := &modeOpener{defaultMode: "pull", wsPull: true}
	opener.wsRespond = func(protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
		return nil, errors.New("workspace pull exploded")
	}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{})
	if !strings.Contains(out, "workspace pull exploded") {
		t.Errorf("SAFETY: the workspace pull error must be visible:\n%s", out)
	}
	if !strings.Contains(out, "seen error") {
		t.Errorf("SAFETY: cached diagnostics must still be shown:\n%s", out)
	}
}

func TestDiagnosticsPull_NoURI_PushModeUnchanged(t *testing.T) {
	inv := newTestInvalidator(t)
	pushDiagnostics(t, inv, "file:///p/a.go", []protocol.Diagnostic{{Severity: protocol.SevError, Message: "pushed"}})
	opener := &modeOpener{defaultMode: "push", wsPull: true}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	out := execDiagnostics(t, tool, map[string]any{})
	if len(opener.wsCalls) != 0 {
		t.Errorf("push mode must never issue a workspace pull")
	}
	if strings.Contains(out, "note:") || strings.Contains(out, "workspace pull") {
		t.Errorf("push-mode no-URI output must be unchanged:\n%s", out)
	}
	if !strings.Contains(out, "pushed") {
		t.Errorf("expected cached push output:\n%s", out)
	}
}

// hybridOpener check: hybrid mode pulls too.
func TestDiagnosticsPull_HybridModePulls(t *testing.T) {
	inv := newTestInvalidator(t)
	opener := &modeOpener{defaultMode: "hybrid"}
	tool := tools.NewDiagnosticsWithOpener(inv, opener)
	execDiagnostics(t, tool, map[string]any{"uri": "file:///p/main.go"})
	if opener.callCount() != 1 {
		t.Errorf("hybrid mode must pull on demand, got %d calls", opener.callCount())
	}
}
