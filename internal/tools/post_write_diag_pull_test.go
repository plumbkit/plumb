package tools

// post_write_diag_pull_test.go — mode-aware post-write diagnostics: the pull
// refresh replaces the push wait on pull/hybrid connections, the differential
// attribution runs unchanged on pulled results, the cross-file sweep uses a
// workspace pull only when advertised (and says so when it cannot), and — the
// SAFETY INVARIANT — a failed pull never reads as a clean pass.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// pullModeLSP is a minimal lsp.Client for post-write pull tests: the embedded
// nil interface panics on any method the pull path must never touch, and the
// pull surface (DiagnosticsMode / Diagnostic / DiagnosticCapabilities /
// WorkspaceDiagnostic) is scripted per test.
type pullModeLSP struct {
	lsp.Client // nil — only the methods below may be called

	mode    string
	respond func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
	calls   []protocol.DocumentDiagnosticParams

	interFile, wsPull bool
	wsRespond         func(params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error)
	wsCalls           int
}

func (p *pullModeLSP) DiagnosticsMode(string) string { return p.mode }

func (p *pullModeLSP) Diagnostic(_ context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	p.calls = append(p.calls, params)
	if p.respond == nil {
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportFull}, nil
	}
	return p.respond(params)
}

func (p *pullModeLSP) DiagnosticCapabilities(string) (bool, bool) { return p.interFile, p.wsPull }

func (p *pullModeLSP) WorkspaceDiagnostic(_ context.Context, _ string, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
	p.wsCalls++
	if p.wsRespond == nil {
		return &protocol.WorkspaceDiagnosticReport{}, nil
	}
	return p.wsRespond(params)
}

func newPullInv(t *testing.T) *cache.Invalidator {
	t.Helper()
	c := cache.New(time.Hour)
	t.Cleanup(c.Close)
	return cache.NewInvalidator(c)
}

const pwURI = "file:///ws/edited.go"

func TestPullPostWrite_PullReplacesWait(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "r1",
			Items:    []protocol.Diagnostic{errAt("syntax error", 1)},
		}, nil
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a\nb", "a\nB", false, baseline)
	if len(client.calls) != 1 {
		t.Fatalf("expected exactly one pull, got %d", len(client.calls))
	}
	if !strings.Contains(out, "syntax error") || !strings.Contains(out, "new since this edit") {
		t.Errorf("expected the pulled finding in the differential, got:\n%s", out)
	}
	// The pull was recorded: the cache serves it and holds the result ID.
	if id, ok := inv.PullResultID(pwURI); !ok || id != "r1" {
		t.Errorf("expected result ID recorded, got %q ok=%v", id, ok)
	}
}

func TestPullPostWrite_CarriedOverDroppedAndStandingNote(t *testing.T) {
	inv := newPullInv(t)
	standing := errAt("pre-existing breakage", 5)
	inv.RecordPullFull(pwURI, "r0", []protocol.Diagnostic{standing})
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind:     protocol.DiagnosticReportFull,
			ResultID: "r1",
			Items:    []protocol.Diagnostic{standing}, // carried over, not new
		}, nil
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a\nb", "a\nB", true, baseline)
	if !strings.Contains(out, "✓ fresh diagnostics pass") {
		t.Errorf("a carried-over-only result is a clean pass for this edit:\n%s", out)
	}
	if !strings.Contains(out, "1 pre-existing issue") {
		t.Errorf("expected the standing pre-existing note:\n%s", out)
	}
	if strings.Contains(out, "new since this edit") {
		t.Errorf("carried-over findings must not be reported as new:\n%s", out)
	}
}

func TestPullPostWrite_UnchangedValidatedServesCache(t *testing.T) {
	inv := newPullInv(t)
	inv.RecordPullFull(pwURI, "r1", nil) // clean snapshot, known result ID
	client := &pullModeLSP{mode: "hybrid"}
	client.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		if params.PreviousResultID != "r1" {
			t.Errorf("expected previousResultId r1, got %q", params.PreviousResultID)
		}
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r1"}, nil
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)
	if !strings.Contains(out, "✓ fresh diagnostics pass") {
		t.Errorf("a validated unchanged over a clean snapshot is a genuine clean pass:\n%s", out)
	}
	if len(client.calls) != 1 {
		t.Errorf("a validated unchanged answer must not retry, got %d calls", len(client.calls))
	}
}

// SAFETY INVARIANT: a failed post-write pull must never produce the ✓ line or
// an empty (implicitly clean) suffix.
func TestPullPostWrite_Safety_ErrorNeverReadsClean(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return nil, errors.New("pull broke")
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)
	if strings.Contains(out, "✓") {
		t.Errorf("SAFETY: a failed pull must never render the clean tick:\n%s", out)
	}
	if !strings.Contains(out, "pull broke") || !strings.Contains(out, "unverified") {
		t.Errorf("SAFETY: the failure must be explicit and marked unverified:\n%s", out)
	}
}

func TestPullPostWrite_Safety_UnvalidatableUnchanged(t *testing.T) {
	inv := newPullInv(t)
	inv.RecordPullFull(pwURI, "r1", nil)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{Kind: protocol.DiagnosticReportUnchanged, ResultID: "r999"}, nil
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)
	if len(client.calls) != 2 {
		t.Fatalf("expected exactly one retry, got %d calls", len(client.calls))
	}
	if strings.Contains(out, "✓") {
		t.Errorf("SAFETY: an unvalidatable unchanged chain must not read clean:\n%s", out)
	}
	if !strings.Contains(out, "unverified") {
		t.Errorf("SAFETY: expected the explicit unverified note:\n%s", out)
	}
}

func TestPullPostWrite_PushModeNeverPulls(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "push"}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 5 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", false, baseline)
	if len(client.calls) != 0 {
		t.Errorf("push mode must never pull, got %d calls", len(client.calls))
	}
	if out != "" {
		// No cached diags and no push arrives: the push body returns "".
		t.Errorf("push-mode behaviour must be unchanged, got:\n%s", out)
	}
}

func TestPullPostWrite_DisabledWindowSkipsPull(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: -1}

	baseline := d.capturePreWriteBaseline(pwURI)
	_ = d.postWriteDiagnostics(pwURI, "a", "b", false, baseline)
	if len(client.calls) != 0 {
		t.Errorf("a disabled post-write window must not pull, got %d calls", len(client.calls))
	}
}

func TestPullPostWrite_DowngradeFallsBackToPushMachinery(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull"}
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		client.mode = "push" // the routing proxy downgraded on -32601
		return nil, errors.New("jsonrpc error -32601: method not found")
	}
	d := WriteDeps{Client: client, Diag: inv, PostWriteDiagWindow: 5 * time.Millisecond}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", false, baseline)
	if strings.Contains(out, "unverified") || strings.Contains(out, "failed") {
		t.Errorf("a downgrade must fall back to the push machinery, not degrade:\n%s", out)
	}
}

func TestPullPostWrite_CrossFile_WorkspacePullWhenAdvertised(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull", interFile: true, wsPull: true}
	otherURI := "file:///ws/other.go"
	client.wsRespond = func(protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
		return &protocol.WorkspaceDiagnosticReport{
			Items: []protocol.WorkspaceDocumentDiagnosticReport{{
				Kind:  protocol.DiagnosticReportFull,
				URI:   otherURI,
				Items: []protocol.Diagnostic{errAt("broke elsewhere", 3)},
			}},
		}, nil
	}
	d := WriteDeps{
		Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond,
		CrossFileDiag: true, WorkspaceFn: func() string { return "/ws" },
	}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", false, baseline)
	if client.wsCalls != 1 {
		t.Fatalf("expected one workspace pull, got %d", client.wsCalls)
	}
	if !strings.Contains(out, "introduced new errors in 1 other file") || !strings.Contains(out, "other.go") {
		t.Errorf("expected the cross-file break reported:\n%s", out)
	}
	if strings.Contains(out, "not exhaustive") {
		t.Errorf("an advertised workspace sweep must not carry the non-exhaustive note:\n%s", out)
	}
}

func TestPullPostWrite_CrossFile_RelatedDocsWithHonestNote(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull", interFile: true, wsPull: false}
	otherURI := "file:///ws/other.go"
	client.respond = func(params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind: protocol.DiagnosticReportFull,
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				otherURI: {
					Kind:  protocol.DiagnosticReportFull,
					Items: []protocol.Diagnostic{errAt("related break", 2)},
				},
			},
		}, nil
	}
	d := WriteDeps{
		Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond,
		CrossFileDiag: true, WorkspaceFn: func() string { return "/ws" },
	}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", false, baseline)
	if client.wsCalls != 0 {
		t.Errorf("workspace pull must not be issued without the capability")
	}
	if !strings.Contains(out, "other.go") || !strings.Contains(out, "related break") {
		t.Errorf("expected the related-document break reported:\n%s", out)
	}
	if !strings.Contains(out, "not exhaustive") {
		t.Errorf("expected the honest non-exhaustive note:\n%s", out)
	}
}

func TestPullPostWrite_CrossFile_WorkspacePullFailureIsExplicit(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull", interFile: true, wsPull: true}
	client.wsRespond = func(protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error) {
		return nil, errors.New("workspace pull broke")
	}
	d := WriteDeps{
		Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond,
		CrossFileDiag: true, WorkspaceFn: func() string { return "/ws" },
	}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", false, baseline)
	if !strings.Contains(out, "workspace pull failed") || !strings.Contains(out, "NOT re-checked") {
		t.Errorf("SAFETY: a failed sweep must say other files were not checked:\n%s", out)
	}
}

// TestPullPostWrite_CrossFile_CleanPullEmitsCleanPassNoHedge is the exact
// composed configuration review finding #1 caught: CrossFileDiag enabled +
// awaitFresh + gopls-like caps (pull advertised via DiagnosticsMode, but
// workspaceDiagnostics NOT advertised — gopls, the headline validated pull
// target, never does) + a clean pull (no new diagnostics anywhere, so the
// cross-file delta is empty). An empty delta has nothing to hedge — the
// edited file itself was just verified by this same pull — so the
// non-exhaustive note must stay silent and the documented ✓ clean-pass line
// must still appear.
func TestPullPostWrite_CrossFile_CleanPullEmitsCleanPassNoHedge(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull", interFile: true, wsPull: false} // gopls-like: no workspaceDiagnostics
	d := WriteDeps{
		Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond,
		CrossFileDiag: true, WorkspaceFn: func() string { return "/ws" },
	}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)
	if !strings.Contains(out, "✓ fresh diagnostics pass") {
		t.Errorf("an empty cross-file delta has nothing to hedge — the documented clean-pass line must still appear:\n%q", out)
	}
	if strings.Contains(out, "not exhaustive") {
		t.Errorf("an empty cross-file delta must not carry the non-exhaustive hedge:\n%q", out)
	}
}

// TestPullPostWrite_CrossFile_NonEmptyDeltaKeepsHedgeNote is the companion
// case: with the same gopls-like caps, a NON-empty cross-file delta must
// still carry the honest non-exhaustive note verbatim, and must not be
// reported as a clean pass.
func TestPullPostWrite_CrossFile_NonEmptyDeltaKeepsHedgeNote(t *testing.T) {
	inv := newPullInv(t)
	client := &pullModeLSP{mode: "pull", interFile: true, wsPull: false}
	otherURI := "file:///ws/other.go"
	client.respond = func(protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
		return &protocol.DocumentDiagnosticReport{
			Kind: protocol.DiagnosticReportFull,
			RelatedDocuments: map[string]protocol.DocumentDiagnosticReport{
				otherURI: {
					Kind:  protocol.DiagnosticReportFull,
					Items: []protocol.Diagnostic{errAt("related break", 2)},
				},
			},
		}, nil
	}
	d := WriteDeps{
		Client: client, Diag: inv, PostWriteDiagWindow: 50 * time.Millisecond,
		CrossFileDiag: true, WorkspaceFn: func() string { return "/ws" },
	}

	baseline := d.capturePreWriteBaseline(pwURI)
	out := d.postWriteDiagnostics(pwURI, "a", "b", true, baseline)
	if !strings.Contains(out, "not exhaustive") {
		t.Errorf("a non-empty cross-file delta must still carry the honest non-exhaustive note:\n%q", out)
	}
	if strings.Contains(out, "✓ fresh diagnostics pass") {
		t.Errorf("a real cross-file break is not a clean pass:\n%q", out)
	}
}
