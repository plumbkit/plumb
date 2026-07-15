//go:build integration

package gopls_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/adapters/gopls"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// goFixtureWS copies the go-fixture (go.mod + main.go) into a fresh temp
// workspace so a test can mutate it without dirtying testdata/.
func goFixtureWS(t *testing.T) string {
	t.Helper()
	fixtureSrc := filepath.Join(repoRoot(t), "testdata", "go-fixture")
	ws := t.TempDir()
	for _, name := range []string{"go.mod", "main.go"} {
		src, err := os.ReadFile(filepath.Join(fixtureSrc, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ws, name), src, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// enablePullInit initialises ad against ws with the LSP 3.17 pull model forced
// on — exactly the Task 3 mechanism the pool uses when [lsp.go] diagnostics =
// "pull": ClientCapabilitiesFor(true) plus gopls's pullDiagnostics init option,
// both applied by the adapter's EnablePullDiagnostics. Returns the initialize
// result so callers can inspect the negotiated server capabilities.
func enablePullInit(t *testing.T, ad *gopls.Adapter, ws string) *protocol.InitializeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	params := gopls.DefaultInitParams(protocol.FileURI(ws))
	ad.EnablePullDiagnostics(&params)
	res, err := ad.Initialize(ctx, params)
	if err != nil {
		t.Fatal("initialize (pull):", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized (pull):", err)
	}
	return res
}

// TestIntegration_ForcedPull_DocumentDiagnostics drives the real gopls binary
// through the negotiated pull path (mode=pull) and both ASSERTS the contract
// gopls guarantees and RECORDS the empirical observations the auto-default
// decision rests on. Contractual assertions: the pull capability is advertised;
// a broken file yields the compiler diagnostic via textDocument/diagnostic; a
// clean file yields an empty report. Recorded-only (logged, never asserted,
// because gopls does not contractually guarantee them): whether gopls emits
// result IDs, and whether it advertises workspace/diagnostic.
func TestIntegration_ForcedPull_DocumentDiagnostics(t *testing.T) {
	ad := startGopls(t)
	ws := goFixtureWS(t)
	res := enablePullInit(t, ad, ws)

	// (a) The pull capability is advertised under forced-pull negotiation.
	if !ad.SupportsPullDiagnostics() {
		t.Fatal("gopls did not advertise diagnosticProvider under forced pull caps — " +
			"the Task 3 negotiation mechanism is broken")
	}
	opts, hasOpts := res.Capabilities.DiagnosticOptions()
	t.Logf("OBSERVED gopls diagnosticProvider advertised=%v opts=%+v", hasOpts, opts)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// (b) A broken file returns its diagnostic via a document pull.
	brokenPath := filepath.Join(ws, "broken.go")
	broken := "package main\n\nfunc broken() int { return \"oops\" }\n"
	if err := os.WriteFile(brokenPath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	brokenURI := protocol.FileURI(brokenPath)
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: brokenURI, Type: protocol.FileCreated}},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}
	rep, err := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: brokenURI},
	})
	if err != nil {
		t.Fatalf("pull Diagnostic (broken): %v", err)
	}
	if rep == nil || len(rep.Items) == 0 {
		t.Fatalf("expected pulled diagnostics for the broken file, got %+v", rep)
	}
	t.Logf("OBSERVED broken pull: kind=%q resultId=%q items=%d firstMsg=%q",
		rep.Kind, rep.ResultID, len(rep.Items), rep.Items[0].Message)

	// (d) Result-ID / unchanged flow — RECORDED, not asserted. gopls v0.23.0
	// does not emit a resultId, so it cannot answer "unchanged"; a second pull
	// carrying the (empty) previousResultId returns a full report again. We
	// assert only that the second pull succeeds — never that gopls implements
	// result-id caching (it does not).
	rep2, err := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument:     protocol.TextDocumentIdentifier{URI: brokenURI},
		PreviousResultID: rep.ResultID,
	})
	if err != nil {
		t.Fatalf("pull Diagnostic (second, prevID=%q): %v", rep.ResultID, err)
	}
	switch {
	case rep.ResultID == "":
		t.Logf("OBSERVED result IDs: gopls emits NO resultId — every pull is a full "+
			"report (second pull kind=%q items=%d); result-id caching / 'unchanged' "+
			"is not implemented by gopls v0.23.0", rep2.Kind, len(rep2.Items))
	case rep2.Kind == protocol.DiagnosticReportUnchanged:
		t.Logf("OBSERVED result IDs: gopls emitted resultId=%q and answered the "+
			"follow-up pull 'unchanged'", rep.ResultID)
	default:
		t.Logf("OBSERVED result IDs: gopls emitted resultId=%q but the follow-up pull "+
			"was a full report (kind=%q items=%d), not 'unchanged'",
			rep.ResultID, rep2.Kind, len(rep2.Items))
	}

	// (c) A clean file returns an empty report.
	cleanPath := filepath.Join(ws, "clean.go")
	if err := os.WriteFile(cleanPath, []byte("package main\n\nfunc clean() int { return 0 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanURI := protocol.FileURI(cleanPath)
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: cleanURI, Type: protocol.FileCreated}},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles (clean):", err)
	}
	cleanRep, err := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: cleanURI},
	})
	if err != nil {
		t.Fatalf("pull Diagnostic (clean): %v", err)
	}
	if cleanRep == nil || len(cleanRep.Items) != 0 {
		t.Fatalf("expected an empty pull report for the clean file, got %+v", cleanRep)
	}
	t.Logf("OBSERVED clean pull: kind=%q resultId=%q items=%d (adapter normalises "+
		"gopls's omitted kind to full)", cleanRep.Kind, cleanRep.ResultID, len(cleanRep.Items))

	// (f) workspace/diagnostic — RECORDED. gopls advertises workspaceDiagnostics
	// = false, so a workspace pull is expected to fail with -32601. We assert
	// only the consistency between the advertised capability and the call
	// outcome, never that gopls supports workspace pull.
	wsAdvertised := hasOpts && opts != nil && opts.WorkspaceDiagnostics
	_, wErr := ad.WorkspaceDiagnostic(ctx, protocol.WorkspaceDiagnosticParams{
		PreviousResultIDs: []protocol.PreviousResultID{},
	})
	t.Logf("OBSERVED workspace/diagnostic: advertised=%v callErr=%v", wsAdvertised, wErr)
	if !wsAdvertised && wErr == nil {
		t.Errorf("gopls did not advertise workspaceDiagnostics yet answered a workspace "+
			"pull without error — inconsistent (err=%v)", wErr)
	}
}

// TestIntegration_ForcedPull_PushObservation records the classification-critical
// observation: after pull is negotiated, does gopls STILL push
// publishDiagnostics? Empirically (v0.23.0) it does — pull is additive, so the
// resolved connection mode is "hybrid", not "pull". Like the result-ID and
// workspace-support items in the neighbouring test, this is OBSERVE-ONLY: gopls
// makes no contractual promise about push scheduling under pull caps, so either
// outcome is logged, never failed on (CI installs gopls@latest — a future gopls
// that reschedules or drops the push stream under pull negotiation is a
// classification change to record, not a plumb defect).
func TestIntegration_ForcedPull_PushObservation(t *testing.T) {
	ad := startGopls(t)
	ws := goFixtureWS(t)
	brokenPath := filepath.Join(ws, "broken.go")
	brokenURI := protocol.FileURI(brokenPath)

	// Subscribe BEFORE init so no push is missed.
	diagCh := make(chan int, 16)
	ad.Subscribe(func(method string, raw json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if json.Unmarshal(raw, &p) != nil || p.URI != brokenURI {
			return
		}
		errs := 0
		for _, d := range p.Diagnostics {
			if d.Severity == protocol.SevError {
				errs++
			}
		}
		select {
		case diagCh <- errs:
		default:
		}
	})

	enablePullInit(t, ad, ws)
	if !ad.SupportsPullDiagnostics() {
		t.Fatal("gopls did not advertise pull under forced pull caps")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	broken := []byte("package main\n\nfunc broken() int { return \"oops\" }\n")
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: brokenURI, Type: protocol.FileCreated}},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	deadline := time.After(15 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				t.Logf("OBSERVED push-continues=true — gopls kept pushing " +
					"publishDiagnostics after pull was negotiated ⇒ resolved mode = HYBRID")
				return
			}
		case <-deadline:
			t.Logf("OBSERVED push-continues=false — no pushed error diagnostics for " +
				"broken.go within 15s under pull caps; this gopls build appears to " +
				"suppress or defer push under pull negotiation ⇒ would classify as " +
				"PULL rather than hybrid (observation only, not a failure)")
			return
		}
	}
}

// TestIntegration_ForcedPull_Latency measures, over the same hybrid gopls
// session and fixture, the latency of the two diagnostics paths after an edit:
//
//	pull  — the forced textDocument/diagnostic request→response duration.
//	push  — the interval from DidChangeWatchedFiles to the publishDiagnostics
//	        arrival for the same edit.
//
// The two are measured in SEPARATE phases (pull-only, then push-only) of one
// session so a concurrent pull cannot artificially accelerate the push it is
// being compared against. Each iteration alternates broken/clean content (so
// the diagnostic set changes every edit and gopls is guaranteed to recompute
// and re-push) with a unique comment (so the bytes differ every write). The
// median and p95 for both paths are logged for the card's decision record.
func TestIntegration_ForcedPull_Latency(t *testing.T) {
	ad := startGopls(t)
	ws := goFixtureWS(t)
	measurePath := filepath.Join(ws, "measure.go")
	measureURI := protocol.FileURI(measurePath)

	// Push arrival timestamps for measureURI (installed before init).
	pushAt := make(chan time.Time, 64)
	ad.Subscribe(func(method string, raw json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if json.Unmarshal(raw, &p) != nil || p.URI != measureURI {
			return
		}
		select {
		case pushAt <- time.Now():
		default:
		}
	})

	enablePullInit(t, ad, ws)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	content := func(i int) []byte {
		if i%2 == 0 {
			return fmt.Appendf(nil, "package main\n\n// iter %d\nfunc measure() int { return \"oops\" }\n", i)
		}
		return fmt.Appendf(nil, "package main\n\n// iter %d\nfunc measure() int { return 0 }\n", i)
	}
	writeAndNotify := func(i int) {
		if err := os.WriteFile(measurePath, content(i), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
			Changes: []protocol.FileEvent{{URI: measureURI, Type: protocol.FileChanged}},
		}); err != nil {
			t.Fatal("DidChangeWatchedFiles:", err)
		}
	}

	const iters = 25
	const warmup = 3

	// Warm-up: let gopls settle (first analyses are cold outliers).
	for i := 0; i < warmup; i++ {
		writeAndNotify(i)
		if _, err := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: measureURI},
		}); err != nil {
			t.Fatalf("warm-up pull: %v", err)
		}
	}

	// Phase PULL — time the forced document pull request→response.
	pullDurs := make([]time.Duration, 0, iters)
	for i := 0; i < iters; i++ {
		writeAndNotify(warmup + i)
		start := time.Now()
		if _, err := ad.Diagnostic(ctx, protocol.DocumentDiagnosticParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: measureURI},
		}); err != nil {
			t.Fatalf("pull iteration %d: %v", i, err)
		}
		pullDurs = append(pullDurs, time.Since(start))
	}

	// Drain any pushes accumulated during the pull phase before measuring push.
	drain := time.After(500 * time.Millisecond)
draining:
	for {
		select {
		case <-pushAt:
		case <-drain:
			break draining
		}
	}

	// Phase PUSH — time DidChangeWatchedFiles → publishDiagnostics arrival.
	pushDurs := make([]time.Duration, 0, iters)
	misses := 0
	for i := 0; i < iters; i++ {
		// Drain stragglers so we time this edit's push, not a previous one.
		for {
			select {
			case <-pushAt:
				continue
			default:
			}
			break
		}
		sent := time.Now()
		writeAndNotify(warmup + iters + i)
		select {
		case at := <-pushAt:
			pushDurs = append(pushDurs, at.Sub(sent))
		case <-time.After(15 * time.Second):
			misses++
			t.Logf("push iteration %d: no publishDiagnostics within 15s", i)
		}
	}

	pMed, pP95 := median(pullDurs), percentile(pullDurs, 95)
	qMed, qP95 := median(pushDurs), percentile(pushDurs, 95)
	t.Logf("LATENCY gopls v0.23.0 (hybrid session, n=%d edits/path):", iters)
	t.Logf("LATENCY   forced-pull  request→response : median=%s p95=%s (n=%d)",
		pMed, pP95, len(pullDurs))
	t.Logf("LATENCY   push         edit→arrival      : median=%s p95=%s (n=%d, misses=%d)",
		qMed, qP95, len(pushDurs), misses)
}

func median(d []time.Duration) time.Duration { return percentile(d, 50) }

func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(p/100*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// TestIntegration_PullAdditions_NoPushRegression is the regression guard: the
// pull-diagnostics additions must not disturb the validated PUSH path under the
// DEFAULT (push-first) negotiation. It mirrors TestIntegration_DidChangeWatchedFiles
// — a broken file announced via DidChangeWatchedFiles must still produce pushed
// publishDiagnostics when pull is NOT negotiated (DefaultInitParams, no
// EnablePullDiagnostics), so gopls advertises no diagnosticProvider and stays on
// the push stream.
func TestIntegration_PullAdditions_NoPushRegression(t *testing.T) {
	ad := startGopls(t)
	ws := goFixtureWS(t)
	brokenPath := filepath.Join(ws, "broken.go")
	brokenURI := protocol.FileURI(brokenPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	diagCh := make(chan int, 16)
	ad.Subscribe(func(method string, raw json.RawMessage) {
		if method != "textDocument/publishDiagnostics" {
			return
		}
		var p protocol.PublishDiagnosticsParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return
		}
		if p.URI != brokenURI {
			return
		}
		errs := 0
		for _, d := range p.Diagnostics {
			if d.Severity == protocol.SevError {
				errs++
			}
		}
		select {
		case diagCh <- errs:
		default:
		}
	})

	if _, err := ad.Initialize(ctx, gopls.DefaultInitParams(protocol.FileURI(ws))); err != nil {
		t.Fatal("initialize:", err)
	}
	if err := ad.Initialized(ctx); err != nil {
		t.Fatal("initialized:", err)
	}
	// Under the default negotiation gopls must NOT advertise pull.
	if ad.SupportsPullDiagnostics() {
		t.Fatal("gopls advertised pull under DEFAULT caps — the default negotiation " +
			"must stay push-first")
	}

	broken := []byte("package main\n\nfunc broken( { } // missing param/return\n")
	if err := os.WriteFile(brokenPath, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ad.DidChangeWatchedFiles(ctx, protocol.DidChangeWatchedFilesParams{
		Changes: []protocol.FileEvent{{URI: brokenURI, Type: protocol.FileCreated}},
	}); err != nil {
		t.Fatal("DidChangeWatchedFiles:", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case errs := <-diagCh:
			if errs > 0 {
				return // push path intact — no regression
			}
		case <-deadline:
			t.Fatal("gopls did not push error diagnostics for broken.go within deadline — " +
				"the pull-diagnostics additions regressed the push path")
		}
	}
}
