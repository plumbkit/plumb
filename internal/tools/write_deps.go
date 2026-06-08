package tools

import (
	"context"
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
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
// gap in internal/feedbacks.md. A globally-disabled post-write window
// (negative) is still honoured (await is a no-op).
func (d WriteDeps) postWriteDiagnostics(uri, content string, awaitFresh bool) string {
	if d.Diag == nil {
		return ""
	}
	ceiling := d.postWriteDiagWindow()
	if awaitFresh && ceiling >= 0 && ceiling < longPostWriteDiagWindow {
		ceiling = longPostWriteDiagWindow
	}
	diags, fresh := awaitDiagnosticsRefresh(d.Diag, uri, ceiling, d.DiagWait)
	out := formatPostWriteDiagnostics(diags, fresh, lineCount(content))
	if awaitFresh && fresh && out == "" {
		return "\n✓ fresh diagnostics pass — no errors or warnings"
	}
	return out
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

func (d WriteDeps) checkBoundary(path string) error {
	return d.Boundary.check(path)
}
