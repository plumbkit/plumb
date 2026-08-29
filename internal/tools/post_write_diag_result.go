package tools

// post_write_diag_result.go — the post-write diagnostics observation itself:
// wait (or pull), difference against the pre-write baseline, and render one
// LABELLED block plus, on request, the structured delta.
//
// Split out of write_deps.go in PLAN-362 PR2, when the function stopped
// returning a bare string: callers now need the freshness verdict and the
// structured delta (fail_on_new_errors decides a ROLLBACK on them), and
// write_deps.go is the dependency bundle, not the diagnostics logic.

import (
	"context"
	"time"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// postWriteDiagOpts is what the calling write tool knows that the diagnostics
// pass cannot work out for itself.
type postWriteDiagOpts struct {
	// awaitFresh extends the wait to longPostWriteDiagWindow and states a clean
	// pass explicitly instead of implying it by silence (await_diagnostics).
	awaitFresh bool
	// structured appends the machine-parseable delta line. Set whenever the
	// caller asked for a confirmed answer, so the default write path is
	// unchanged.
	structured bool
	// lspNotifyFailed reports that didChangeWatchedFiles for this write FAILED.
	// The server therefore does not know the file changed, so any diagnostics
	// arriving during the wait are about the PREVIOUS content — coincidence, not
	// confirmation. Never fresh, and never grounds for a rollback.
	lspNotifyFailed bool
}

// postWriteDiagResult is one post-write diagnostics pass: the rendered block
// (already labelled, "" when there is nothing to print) and the structured
// delta whose Fresh field is the freshness verdict call sites act on.
type postWriteDiagResult struct {
	text  string
	delta diagDelta
}

// postWriteDiagnostics waits for the language server to re-publish diagnostics
// for the just-written uri, differences them against the pre-write baseline, and
// returns the rendered block plus the structured delta. content is the new file
// content; its line count down-ranks provably-stale, past-EOF diagnostics.
//
// Mode-aware: on a pull/hybrid connection the refresh is an on-demand pull of
// the edited URI (post_write_diag_pull.go) rather than a wait for the next
// publishDiagnostics push — which, per the Invalidator's waiter contract, a pull
// would never wake. Push mode (and any client without mode awareness) keeps the
// wait-based behaviour below.
//
// Every non-empty block carries one of the four fixed labels, and the delta says
// per scope how the answer was obtained. Three ways to get NO confirmation are
// distinguished rather than blurred, because they call for different responses:
// no diagnostics source at all, a failed post-write notification, and a wait
// that expired (or was disabled).
func (d WriteDeps) postWriteDiagnostics(uri, before, content string, opt postWriteDiagOpts, baseline *diagBaseline) postWriteDiagResult {
	if d.Diag == nil {
		// No language server for this file (or none attached). Historically this
		// printed nothing, and silence elsewhere in the block means "clean" — so
		// a caller who explicitly asked gets told the difference.
		return notAnalysedResult(opt, diagScopeNoSource,
			"diagnostics: no diagnostics source for this file — nothing was analysed (silence here is not a clean bill of health)")
	}
	if opt.lspNotifyFailed {
		return notAnalysedResult(opt, diagScopeUnconfirmed,
			"diagnostics: not analysed — the language server could not be told this file changed, so any result would describe the previous content; call diagnostics() to confirm")
	}
	if r, handled := d.pullPostWriteDiagnostics(uri, before, content, opt, baseline); handled {
		return r
	}
	ceiling := d.postWriteDiagWindow()
	disabled := ceiling < 0
	if opt.awaitFresh && !disabled && ceiling < longPostWriteDiagWindow {
		ceiling = longPostWriteDiagWindow
	}
	diags, fresh := awaitDiagnosticsRefresh(d.Diag, uri, ceiling, d.DiagWait)
	if !fresh {
		return staleDiagResult(diags, opt, disabled)
	}
	return d.freshDiagResult(uri, before, content, opt, baseline, diags)
}

// notAnalysedResult is the answer when no post-write analysis was even
// attempted. It never carries findings and never claims freshness; on the
// default path (no explicit ask) it stays silent, exactly as before.
func notAnalysedResult(opt postWriteDiagOpts, scope, line string) postWriteDiagResult {
	r := postWriteDiagResult{delta: unconfirmedDelta(scope)}
	if !opt.awaitFresh && !opt.structured {
		return r
	}
	r.text = postWriteDiagLabel(postWriteDiagLabelNotAnalysed) + "\n" + line
	if opt.structured {
		r.text += r.delta.line()
	}
	return r
}

// staleDiagResult is the answer when the server has not re-published since the
// write: the snapshot predates it, so a differential would be empty and
// misleading. Surface one honest pending line rather than the pre-edit findings,
// which read as fresh breakage (the recurring dogfooding friction).
//
// disabled distinguishes "the wait expired" from "there was no wait" — the
// post-write window is switched off ([edits] post_write_diagnostics_ms = 0), so
// telling the caller their result did not arrive "within the wait" would name a
// wait that never ran.
func staleDiagResult(diags []protocol.Diagnostic, opt postWriteDiagOpts, disabled bool) postWriteDiagResult {
	r := postWriteDiagResult{delta: unconfirmedDelta(diagScopeUnconfirmed)}
	if len(diags) == 0 && !opt.awaitFresh && !opt.structured {
		// No explicit ask for confirmation and nothing cached: silence is the
		// existing, unchanged default-path behaviour.
		return r
	}
	var line string
	switch {
	case disabled && len(diags) == 0:
		line = "diagnostics: post-write re-analysis is disabled ([edits] post_write_diagnostics_ms = 0) — nothing was analysed and nothing is cached; call diagnostics() to confirm"
	case disabled:
		line = "diagnostics: post-write re-analysis is disabled ([edits] post_write_diagnostics_ms = 0) — the last-known state may predate this write; call diagnostics() to confirm"
	case len(diags) == 0:
		line = "diagnostics: not re-analysed within the wait — nothing cached either; call diagnostics() to confirm"
	default:
		line = "diagnostics: pending — LSP not yet re-analysed; call diagnostics() to confirm"
	}
	r.text = postWriteDiagLabel(postWriteDiagLabelSnapshot) + "\n" + line
	if opt.structured {
		r.text += r.delta.line()
	}
	return r
}

// freshDiagResult builds the confirmed-fresh answer: the differential block, the
// cross-file sweep, the standing pre-existing note, and the structured delta.
func (d WriteDeps) freshDiagResult(uri, before, content string, opt postWriteDiagOpts, baseline *diagBaseline, diags []protocol.Diagnostic) postWriteDiagResult {
	var pre []protocol.Diagnostic
	if baseline != nil {
		pre = baseline.editedPre
	}
	lo, hi, touched := changedLineRange(before, content)
	freshNew, likelyStale := diffFileDiagnostics(pre, diags, lo, hi, touched)
	errs, warns, stale := splitDifferential(freshNew, likelyStale, lineCount(content))

	out := renderDifferential(errs, warns, stale)
	crossText, breaks, crossScope := d.crossFileDiagnostics(uri, true, baseline)
	out += crossText
	if opt.awaitFresh && out == "" {
		out = "\n✓ fresh diagnostics pass — this edit introduced no new errors or warnings"
	}
	// Standing pre-existing errors are correctly dropped from the delta, but a
	// clean "no new errors" result would otherwise hide them — an agent could
	// commit over them. Append a count so the file's full state is not implied
	// clean by silence.
	preExisting := standingPreExistingErrors(pre, diags, lo, hi, touched)
	out += formatStandingPreExistingNote(preExisting)

	r := postWriteDiagResult{delta: buildDelta(uri, errs, pre, diags, breaks, crossScope, preExisting)}
	if opt.structured {
		out += r.delta.line()
	}
	if out == "" {
		return r
	}
	// fresh is true on every path that reaches here, so the block reflects a
	// genuine post-write re-analysis: label it authoritative.
	r.text = postWriteDiagLabel(postWriteDiagLabelAuthoritative) + out
	return r
}

// buildDelta assembles the structured delta from the same classification the
// prose block was rendered from — one computation, two renderings, so the two
// can never disagree.
func buildDelta(uri string, newErrs, pre, post []protocol.Diagnostic, breaks []crossFileBreak, crossScope string, preExisting int) diagDelta {
	entries, omittedNew := newDiagEntries(uri, newErrs)
	resolved, omittedResolved := newDiagEntries(uri, resolvedDiagnostics(pre, post))
	return diagDelta{
		Fresh:              true,
		Scopes:             diagScopes{EditedFile: diagScopeFresh, CrossFile: crossScope},
		NewErrors:          entries,
		CrossFileNewErrors: crossFileEntries(breaks),
		Resolved:           resolved,
		PreExisting:        preExisting,
		OmittedNewErrors:   omittedNew,
		OmittedResolved:    omittedResolved,
	}
}

// crossFileDiagnostics runs the bounded cross-file sweep and renders any NEW
// errors this write introduced in files other than the one edited, alongside the
// structured breaks and the scope's freshness. It is a no-op unless the sweep is
// enabled and a pre-write baseline was captured; the caller only reaches it once
// the edited file itself re-published fresh (else the server is lagging and any
// delta is unreliable). The settle grace lets dependent files re-publish before
// the comparison; the single-file result is already built and is never delayed
// or dropped by this step. The grace is a CEILING, not a fixed sleep — see
// waitForCrossFileSettle.
//
// The push sweep is never "fresh": it attributes only files the server happened
// to re-publish after the baseline, so a dependent file still being analysed is
// invisible to it. It reports diagScopeIncomplete when it ran at all.
func (d WriteDeps) crossFileDiagnostics(editedURI string, fresh bool, baseline *diagBaseline) (string, []crossFileBreak, string) {
	if baseline == nil || !fresh || !d.crossFileEnabled() {
		return "", nil, diagScopeNotChecked
	}
	cf, ok := d.Diag.(crossFileDiagSource)
	if !ok {
		return "", nil, diagScopeNotChecked
	}
	if settle := d.crossFileSettleWindow(); settle > 0 {
		waitForCrossFileSettle(d.Diag, settle)
	}
	breaks := computeCrossFileDelta(baseline, cf.AllDiagnostics(), cf.AllDiagnosticTimes(), editedURI)
	return formatCrossFileDiagnostics(breaks, d.workspaceRoot()), breaks, diagScopeIncomplete
}

// anyDiagnosticsWaiter is the optional capability that lets the settle grace end
// as soon as a dependent file actually re-publishes.
type anyDiagnosticsWaiter interface {
	WaitForAnyDiagnostics(ctx context.Context) error
}

// waitForCrossFileSettle waits up to settle for a dependent file to re-publish
// its diagnostics, returning as soon as one does.
//
// This used to be a flat `<-time.After(settle)` — an unconditional 200 ms sleep
// on every edit whose file re-published fresh, with post_write_cross_file on by
// default. Together with the adaptive publish wait it put a ~275-500 ms floor
// under edit_file before any I/O, which is most of the measured ~683 ms average.
// The ceiling and the default are unchanged; only the common case gets shorter,
// and a source that cannot signal falls back to the original sleep.
func waitForCrossFileSettle(src postWriteDiagSource, settle time.Duration) {
	waiter, ok := src.(anyDiagnosticsWaiter)
	if !ok {
		<-time.After(settle)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), settle)
	defer cancel()
	_ = waiter.WaitForAnyDiagnostics(ctx)
}
