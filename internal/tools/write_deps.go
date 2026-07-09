package tools

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/paths"
)

// QualityReportFn is a function that runs post-write quality analysis on path
// and returns a formatted findings section ("\ncode quality (...):\n  ...").
// Returns "" when analysis is disabled, no analyser supports the file, or
// there are no findings. nil is a no-op. Intended to be assigned to
// WriteDeps.QualityReport.
type QualityReportFn = func(ctx context.Context, path string) string

// TopologyNotifyFn is called after a successful write to enqueue the written
// path for topology re-indexing. path is the absolute path to the written file.
// nil is a no-op.
type TopologyNotifyFn = func(path string)

// WriteDeps bundles the dependencies every file-mutating tool needs.
// Pulling these into one struct stops the constructor signatures from
// growing every time we add a cross-cutting concern (rate limit, strict
// mode, post-write diagnostics, etc.).
//
// All fields are optional and nil-safe: a freshly-constructed WriteDeps{}
// behaves like "no LSP client, no cache, no diagnostics, no limit, no
// strict mode, no read tracking" — useful for unit tests.
//
// Concurrency: WriteDeps is read-only after construction; embedded objects
// (RateLimiter, ReadTracker, *cache.Cache) have their own locking.
type WriteDeps struct {
	// Client is the LSP client used to send didChangeWatchedFiles. nil
	// skips LSP notification (tests, headless mode).
	Client lsp.Client
	// Cache is the symbol cache that gets invalidated by URI after every
	// successful write. nil skips invalidation.
	Cache *cache.Cache
	// Diag is the diagnostics source polled after a write so the tool can
	// append fresh errors/warnings to its output. nil skips the post-write
	// diagnostics window.
	Diag postWriteDiagSource
	// Limiter caps how many write operations a session may issue per
	// minute. nil disables rate limiting.
	Limiter *RateLimiter
	// Strict, when non-nil, reports whether strict mode applies to this
	// call (per the resolved per-workspace [edits].strict config and the
	// PLUMB_STRICT_EDITS env var). nil falls back to env-only.
	Strict StrictModeFn
	// Reads is the per-session ReadTracker that record_file populates and
	// edit_file consults in strict mode. nil disables per-session
	// tracking (strict mode becomes a no-op for the requires-read check).
	Reads *ReadTracker
	// Writes is the per-session WriteTracker recording paths plumb has written
	// this session. The dirty-guard consults it so re-editing a file plumb
	// itself wrote is never blocked, while pre-existing uncommitted work still
	// requires dirty_ok. nil disables session-awareness (every dirty file then
	// blocks unless dirty_ok is set).
	Writes *WriteTracker
	// Boundary rejects paths outside the workspace pinned to this MCP
	// connection. nil disables boundary checks (tests / unattached sessions).
	Boundary BoundaryGuard
	// PostWriteDiagWindow is how long write/edit tools wait for the LSP
	// server to re-publish diagnostics after a successful write. Zero means
	// "use the 300 ms default" (back-compat for test setups that use
	// WriteDeps{}). Negative means "disabled — skip the wait entirely".
	PostWriteDiagWindow time.Duration
	// PostWriteDiagWindowFn, when set, overrides PostWriteDiagWindow at call
	// time. The daemon uses this so project config loaded after tool
	// registration affects subsequent writes.
	PostWriteDiagWindowFn func() time.Duration
	// DiagWait, when non-nil, adapts the post-write diagnostics wait to the
	// language server's observed re-publish latency — the configured window is
	// treated as a ceiling. Shared per connection so all write tools learn
	// together. nil disables adaptation (the full window is always used).
	DiagWait *DiagWaitEstimator
	// CrossFileDiag enables the cross-file post-write diagnostics sweep: after a
	// write, plumb reports NEW errors the edit introduced in OTHER files (see
	// post_write_diag_crossfile.go). Off in a bare WriteDeps{}; the daemon sets
	// CrossFileDiagFn from [edits].post_write_cross_file.
	CrossFileDiag bool
	// CrossFileDiagFn, when set, overrides CrossFileDiag at call time so project
	// config loaded after tool registration takes effect.
	CrossFileDiagFn func() bool
	// CrossFileSettle is the bounded grace the cross-file sweep waits for
	// dependent-file diagnostics to land before comparing. Zero compares
	// immediately.
	CrossFileSettle time.Duration
	// CrossFileSettleFn, when set, overrides CrossFileSettle at call time.
	CrossFileSettleFn func() time.Duration
	// ConcurrentWriteSkew is the clock-skew allowance for edit_file's
	// post-rename mtime check. Zero falls back to the 100 ms compiled-in
	// default. Increase on slow filesystems (network mounts, FUSE).
	ConcurrentWriteSkew time.Duration
	// ConcurrentWriteSkewFn, when set, overrides ConcurrentWriteSkew at call
	// time.
	ConcurrentWriteSkewFn func() time.Duration
	// WorkspaceFn returns the resolved workspace root for the current session.
	// Used by transaction_apply to locate .plumb/tx-log/ for the durable
	// rollback log. nil or a function returning "" disables the txlog.
	WorkspaceFn func() string
	// ShowWriteDiff, when true, appends a unified diff of the change to
	// write_file and edit_file responses. Defaults to true (zero value of
	// bool is false, but callers should set this from the resolved config).
	// Set to false for implicit-verification mode (tokens matter more than
	// inline confirmation).
	ShowWriteDiff bool
	// ShowWriteDiffFn, when set, overrides ShowWriteDiff at call time.
	ShowWriteDiffFn func() bool
	// BlockDirtyFn reports whether the dirty-guard is enabled for this call
	// (the resolved [edits].block_dirty_writes / PLUMB_BLOCK_DIRTY_WRITES). When
	// it returns false the guard is a no-op — a destructive write to a
	// pre-existing dirty file is allowed without dirty_ok. nil means "enabled"
	// (blockDirty defaults to true) so a bare WriteDeps{} keeps the safe default.
	BlockDirtyFn func() bool
	// PostWriteNotifyFn, when non-nil, is called after a successful
	// notifyLSP to perform any adapter-specific post-write notification.
	// The daemon sets this for Java workspaces to send DidOpen + DidClose so
	// jdtls emits diagnostics for modified files (unlike gopls/pyright, jdtls
	// only publishes diagnostics for open documents). nil is a no-op.
	PostWriteNotifyFn func(ctx context.Context, path string) error
	// QualityReport, when non-nil, is called after a successful write to run
	// offline quality analysis on the written path. Returns a formatted
	// findings section appended to the tool response, or "" if no findings
	// or analysis is disabled for this session.
	QualityReport QualityReportFn
	// TopologyNotify, when non-nil, is called after a successful write to
	// enqueue the written path for topology re-indexing. path is the absolute
	// path to the written file. nil is a no-op.
	TopologyNotify TopologyNotifyFn
	// Undo, when non-nil, records the single most recent revertible write per
	// path so undo_edit can safely revert it. nil disables undo capture.
	Undo *UndoStore
}

func (d WriteDeps) postWriteDiagWindow() time.Duration {
	if d.PostWriteDiagWindowFn != nil {
		return d.PostWriteDiagWindowFn()
	}
	return d.PostWriteDiagWindow
}

// longPostWriteDiagWindow is the wait used when a write tool requests an
// authoritative post-write diagnostics pass (await_diagnostics:true). It is far
// longer than the default adaptive window so the language server almost always
// re-publishes within it — giving the agent a trustworthy "did my change
// compile?" answer without shelling out to a build.
const longPostWriteDiagWindow = 5 * time.Second

// postWriteDiagnostics waits for the language server to re-publish diagnostics
// for the just-written uri, then renders them as a compact suffix ("" when the
// source is unset or there is nothing to report). content is the new file
// content; its line count down-ranks provably-stale, past-EOF diagnostics.
//
// When awaitFresh is set the wait is extended to longPostWriteDiagWindow so the
// result is trustworthy, and a clean fresh pass is stated explicitly rather than
// implied by silence — closing the "I had to shell out to go build to be sure"
// gap reported in dogfooding. A globally-disabled post-write window
// (negative) is still honoured (await is a no-op).
func (d WriteDeps) postWriteDiagnostics(uri, before, content string, awaitFresh bool, baseline *diagBaseline) string {
	if d.Diag == nil {
		return ""
	}
	ceiling := d.postWriteDiagWindow()
	if awaitFresh && ceiling >= 0 && ceiling < longPostWriteDiagWindow {
		ceiling = longPostWriteDiagWindow
	}
	diags, fresh := awaitDiagnosticsRefresh(d.Diag, uri, ceiling, d.DiagWait)
	if !fresh {
		// The server has not re-published since the write, so the snapshot
		// predates it and a differential would be empty and misleading. Surface a
		// single honest pending line rather than the pre-edit findings, which read
		// as fresh breakage (the recurring dogfooding friction).
		if len(diags) == 0 {
			return ""
		}
		return "\ndiagnostics: pending — LSP not yet re-analysed; call diagnostics() to confirm"
	}
	var pre []protocol.Diagnostic
	if baseline != nil {
		pre = baseline.editedPre
	}
	lo, hi, touched := changedLineRange(before, content)
	freshNew, likelyStale := diffFileDiagnostics(pre, diags, lo, hi, touched)
	out := formatDifferentialDiagnostics(freshNew, likelyStale, lineCount(content))
	out += d.crossFileDiagnostics(uri, fresh, baseline)
	if awaitFresh && out == "" {
		out = "\n✓ fresh diagnostics pass — this edit introduced no new errors or warnings"
	}
	// Standing pre-existing errors are correctly dropped from the delta, but a
	// clean "no new errors" result would otherwise hide them — an agent could
	// commit over them. Append a count so the file's full state is not implied
	// clean by silence.
	out += formatStandingPreExistingNote(standingPreExistingErrors(pre, diags, lo, hi, touched))
	return out
}

func (d WriteDeps) crossFileEnabled() bool {
	if d.CrossFileDiagFn != nil {
		return d.CrossFileDiagFn()
	}
	return d.CrossFileDiag
}

func (d WriteDeps) crossFileSettleWindow() time.Duration {
	if d.CrossFileSettleFn != nil {
		return d.CrossFileSettleFn()
	}
	return d.CrossFileSettle
}

// capturePreWriteBaseline snapshots the language server's pre-write state for a
// write to uri. It ALWAYS records the edited file's own diagnostics (used by the
// single-file differential block) when a Diag source is wired, and ADDITIONALLY
// records a whole-workspace error baseline (for the cross-file sweep) when that
// sweep is enabled and the source can serve one. Returns nil only when no Diag
// source is wired. Callers MUST invoke this BEFORE the write mutates the file, so
// the baseline reflects the pre-edit language-server state.
func (d WriteDeps) capturePreWriteBaseline(uri string) *diagBaseline {
	if d.Diag == nil {
		return nil
	}
	var b *diagBaseline
	if d.crossFileEnabled() {
		b = newDiagBaseline(d.Diag) // nil when the source cannot serve a whole-workspace snapshot
	}
	if b == nil {
		b = &diagBaseline{at: time.Now()}
	}
	b.editedURI = uri
	b.editedPre = d.Diag.Diagnostics(uri)
	return b
}

// crossFileDiagnostics runs the bounded cross-file sweep and renders any NEW
// errors this write introduced in files other than the one edited. It is a no-op
// unless the sweep is enabled, a pre-write baseline was captured, and the edited
// file itself re-published fresh (else the server is lagging and any delta is
// unreliable). The settle grace lets dependent files re-publish before the
// comparison; the single-file result is already built and is never delayed or
// dropped by this step.
func (d WriteDeps) crossFileDiagnostics(editedURI string, fresh bool, baseline *diagBaseline) string {
	if baseline == nil || !fresh || !d.crossFileEnabled() {
		return ""
	}
	cf, ok := d.Diag.(crossFileDiagSource)
	if !ok {
		return ""
	}
	if settle := d.crossFileSettleWindow(); settle > 0 {
		<-time.After(settle)
	}
	breaks := computeCrossFileDelta(baseline, cf.AllDiagnostics(), cf.AllDiagnosticTimes(), editedURI)
	root := ""
	if d.WorkspaceFn != nil {
		root = d.WorkspaceFn()
	}
	return formatCrossFileDiagnostics(breaks, root)
}

func (d WriteDeps) concurrentWriteSkew() time.Duration {
	if d.ConcurrentWriteSkewFn != nil {
		return d.ConcurrentWriteSkewFn()
	}
	return d.ConcurrentWriteSkew
}

func (d WriteDeps) showWriteDiff() bool {
	if d.ShowWriteDiffFn != nil {
		return d.ShowWriteDiffFn()
	}
	return d.ShowWriteDiff
}

// blockDirty reports whether the dirty-guard is enabled for this call. A nil
// BlockDirtyFn defaults to true so a bare WriteDeps{} (tests, headless) keeps
// the safe default; the daemon wires it to the resolved [edits].block_dirty_writes.
func (d WriteDeps) blockDirty() bool {
	if d.BlockDirtyFn != nil {
		return d.BlockDirtyFn()
	}
	return true
}

// reportQuality invokes QualityReport if set and returns its output.
func (d WriteDeps) reportQuality(ctx context.Context, path string) string {
	if d.QualityReport == nil {
		return ""
	}
	return d.QualityReport(ctx, path)
}

// notifyTopology enqueues path for topology re-indexing when TopologyNotify is wired.
func (d WriteDeps) notifyTopology(path string) {
	if d.TopologyNotify != nil {
		d.TopologyNotify(path)
	}
}

// recordUndo snapshots a successful write so undo_edit can revert it. before is
// the file content prior to the write ("" with existedBefore=false for a
// creation); after is the content plumb wrote. A before larger than the
// snapshot cap is skipped, so undo is simply unavailable for very large files.
// nil-safe (no-op when Undo is unwired).
func (d WriteDeps) recordUndo(path, before, after string, existedBefore bool, tool string) {
	if d.Undo == nil {
		return
	}
	if len(before) > maxUndoSnapshotBytes {
		return
	}
	d.Undo.Record(path, undoSnapshot{
		before:        before,
		existedBefore: existedBefore,
		afterSHA:      sha256OfString(after),
		tool:          tool,
	})
}

func (d WriteDeps) checkBoundary(path string) error {
	return d.Boundary.check(path)
}

// resolvePath resolves a path argument against the connection's pinned
// workspace. It strips a leading file:// scheme and, when the remainder is
// relative and WorkspaceFn resolves a root, anchors it there
// (filepath.Join(root, path)). An absolute path is returned unchanged; a
// relative path with no resolvable workspace is returned cleaned but still
// relative, so the boundary check rejects it rather than the tool silently
// touching a daemon-CWD-relative file. nil/empty WorkspaceFn is a no-op,
// preserving WriteDeps{} test setups. The resolved path must feed BOTH the
// boundary check and the filesystem operation.
func (d WriteDeps) resolvePath(path string) string {
	p := paths.URIToPath(path)
	if filepath.IsAbs(p) {
		return p
	}
	var base string
	if d.WorkspaceFn != nil {
		base = d.WorkspaceFn()
	}
	if base == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(base, p)
}

// recordWritten marks path as written by plumb this session in BOTH per-session
// trackers. It is the single "this session now knows path's current state" seam,
// called on every successful content write (edit_file, write_file, find_replace,
// rename/copy destination, transaction).
//
//   - WriteTracker: powers the dirty-guard (re-editing a file plumb wrote is not
//     blocked) and the read-time concurrent-edit warning.
//   - ReadTracker: a write is as authoritative as a read for the file's current
//     content, so recording the post-write mtime means (a) strict mode does not
//     demand a re-read after the session's own edit, and (b) the
//     changedSinceSessionRead staleness guard does not false-positive on the
//     session's own consecutive writes (read → edit → edit no longer warns).
//
// Both calls stat under the caller's held per-path lock, so they observe the
// same post-write mtime. nil-safe on both trackers.
func (d WriteDeps) recordWritten(path string) {
	d.Writes.Record(path)
	if d.Reads != nil {
		if info, err := os.Stat(path); err == nil {
			sha, _ := fileSHA256(path) // best-effort; empty on error
			d.Reads.Record(path, info.ModTime(), sha)
		}
	}
}
