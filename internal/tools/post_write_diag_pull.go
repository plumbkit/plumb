package tools

// post_write_diag_pull.go — the pull/hybrid half of post-write diagnostics.
// After a write is notified via DidChangeWatchedFiles, a push-mode connection
// waits for the server's next publishDiagnostics (post_write_diag.go); a
// pull/hybrid connection instead pulls the edited URI directly — the server
// processes the change notification before the pull on the same connection,
// so the answer reflects this write — and runs the SAME differential
// attribution (diffFileDiagnostics) on the pulled results. Related documents
// the pull carries are folded into the cache, and the cross-file sweep uses a
// bounded workspace pull ONLY when the server advertises both
// interFileDependencies and workspaceDiagnostics; otherwise the sweep is
// honestly labelled non-exhaustive.
//
// SAFETY INVARIANT: a failed pull never reads as a clean pass — the ✓ line is
// only emitted after a successful pull, and failures surface an explicit
// "state unverified" note instead of an empty (implicitly clean) suffix.

import (
	"context"
	"fmt"

	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// postWritePuller is the mode-and-pull surface the post-write path needs from
// WriteDeps.Client. The session routing proxy implements it; bare test
// clients do not, which routes them to the push machinery.
type postWritePuller interface {
	DiagnosticsMode(uri string) string
	Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error)
}

// postWriteWorkspacePuller adds the capability probe and workspace pull used
// by the cross-file sweep.
type postWriteWorkspacePuller interface {
	DiagnosticCapabilities(uri string) (interFileDependencies, workspaceDiagnostics bool)
	WorkspaceDiagnostic(ctx context.Context, uri string, params protocol.WorkspaceDiagnosticParams) (*protocol.WorkspaceDiagnosticReport, error)
}

// pullPostWriteDiagnostics runs the post-write diagnostics pass for a
// pull/hybrid-mode URI. handled=false hands the call back to the push
// machinery: the client is not mode-aware, the mode is not pull/hybrid, the
// post-write window is disabled, or the connection was downgraded (-32601)
// mid-call.
func (d WriteDeps) pullPostWriteDiagnostics(uri, before, content string, awaitFresh bool, baseline *diagBaseline) (out string, handled bool) {
	pp, ok := d.Client.(postWritePuller)
	if !ok || !pullModeActive(pp.DiagnosticsMode(uri)) {
		return "", false
	}
	ceiling := d.postWriteDiagWindow()
	if ceiling < 0 {
		return "", false // post-write diagnostics disabled: the push body's honest handling applies
	}
	if ceiling == 0 {
		ceiling = defaultPostWriteDiagWindow
	}
	if awaitFresh && ceiling < longPostWriteDiagWindow {
		ceiling = longPostWriteDiagWindow
	}
	ctx, cancel := context.WithTimeout(context.Background(), ceiling)
	defer cancel()

	pulled, err := d.pullEdited(ctx, pp, uri)
	if err != nil {
		if !pullModeActive(pp.DiagnosticsMode(uri)) {
			// Downgraded (-32601 on a negotiated pull): this is a push
			// connection now — let the push machinery finish this call.
			return "", false
		}
		// SAFETY INVARIANT: an explicit unverified note, never an empty
		// (implicitly clean) suffix and never the ✓ line.
		return fmt.Sprintf("\ndiagnostics: pull after write failed (%v) — state unverified; call diagnostics() to confirm", err), true
	}

	var pre []protocol.Diagnostic
	if baseline != nil {
		pre = baseline.editedPre
	}
	lo, hi, touched := changedLineRange(before, content)
	freshNew, likelyStale := diffFileDiagnostics(pre, pulled, lo, hi, touched)
	out = formatDifferentialDiagnostics(freshNew, likelyStale, lineCount(content))
	out += d.pullCrossFileDiagnostics(ctx, uri, baseline)
	if awaitFresh && out == "" {
		out = "\n✓ fresh diagnostics pass — this edit introduced no new errors or warnings"
	}
	out += formatStandingPreExistingNote(standingPreExistingErrors(pre, pulled, lo, hi, touched))
	return out, true
}

// pullEdited pulls the edited URI (previousResultId from the cache, unknown-ID
// retry rule applied, results — related documents included — recorded) and
// returns the diagnostics that now apply to it. A validated "unchanged" answer
// serves the cached snapshot, which the validation just proved current.
func (d WriteDeps) pullEdited(ctx context.Context, pp postWritePuller, uri string) ([]protocol.Diagnostic, error) {
	rec, _ := d.Diag.(pullStateSource)
	rep, err := pullAndRecord(ctx, pp, rec, uri)
	if err != nil {
		return nil, err
	}
	if rep == nil {
		return d.Diag.Diagnostics(uri), nil
	}
	return rep.Items, nil
}

// pullCrossFileDiagnostics is the pull-mode cross-file sweep. Gated exactly
// like the push sweep (enabled + baseline + a workspace-capable Diag source).
// When the owning server advertises interFileDependencies AND
// workspaceDiagnostics, a bounded workspace pull refreshes the cache first and
// the sweep is exhaustive; otherwise the sweep only sees the files this
// write's pull reported on (related documents), and says so — but only when
// there is actually a delta to hedge: an empty result means the sweep found
// nothing to report, so the non-exhaustive caveat would be pure noise (and
// would wrongly make the caller's out non-empty, suppressing the
// awaitFresh ✓ clean-pass line for every gopls-class server).
func (d WriteDeps) pullCrossFileDiagnostics(ctx context.Context, editedURI string, baseline *diagBaseline) string {
	if baseline == nil || !d.crossFileEnabled() {
		return ""
	}
	cf, ok := d.Diag.(crossFileDiagSource)
	if !ok {
		return ""
	}
	exhaustive := false
	failNote := ""
	if wp, ok := d.Client.(postWriteWorkspacePuller); ok {
		if interFile, wsPull := wp.DiagnosticCapabilities(editedURI); interFile && wsPull {
			if err := d.workspacePullInto(ctx, wp, editedURI); err != nil {
				failNote = fmt.Sprintf("\n(cross-file sweep incomplete: workspace pull failed (%v) — other files were NOT re-checked)", err)
			} else {
				exhaustive = true
			}
		}
	}
	root := ""
	if d.WorkspaceFn != nil {
		root = d.WorkspaceFn()
	}
	out := formatCrossFileDiagnostics(computeCrossFileDelta(baseline, cf.AllDiagnostics(), cf.AllDiagnosticTimes(), editedURI), root)
	if failNote != "" {
		return out + failNote
	}
	if !exhaustive && out != "" {
		out += "\n(cross-file check limited to files this pull reported on — not exhaustive in pull mode)"
	}
	return out
}

// workspacePullInto issues one bounded workspace/diagnostic request (previous
// result IDs from the cache) and records every per-document report. The
// caller's ctx bounds it — the same window that bounds the edited-file pull.
func (d WriteDeps) workspacePullInto(ctx context.Context, wp postWriteWorkspacePuller, uri string) error {
	rec, ok := d.Diag.(pullStateSource)
	if !ok {
		return fmt.Errorf("no pull-capable cache on this connection")
	}
	rep, err := wp.WorkspaceDiagnostic(ctx, uri, protocol.WorkspaceDiagnosticParams{
		PreviousResultIDs: rec.AllPullResultIDs(),
	})
	if err != nil {
		return err
	}
	for _, item := range rep.Items {
		rec.RecordPullResult(item.URI, protocol.DocumentDiagnosticReport{
			Kind:     item.Kind,
			ResultID: item.ResultID,
			Items:    item.Items,
		})
	}
	return nil
}
