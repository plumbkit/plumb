package tools

import (
	"time"

	"github.com/golimpio/plumb/internal/cache"
	"github.com/golimpio/plumb/internal/lsp"
)

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
	Client lsp.LSPClient
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
	// PostWriteDiagWindow is how long write/edit tools wait for the LSP
	// server to re-publish diagnostics after a successful write. Zero means
	// "use the 300 ms default" (back-compat for test setups that use
	// WriteDeps{}). Negative means "disabled — skip the wait entirely".
	PostWriteDiagWindow time.Duration
	// PostWriteDiagWindowFn, when set, overrides PostWriteDiagWindow at call
	// time. The daemon uses this so project config loaded after tool
	// registration affects subsequent writes.
	PostWriteDiagWindowFn func() time.Duration
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
}

func (d WriteDeps) postWriteDiagWindow() time.Duration {
	if d.PostWriteDiagWindowFn != nil {
		return d.PostWriteDiagWindowFn()
	}
	return d.PostWriteDiagWindow
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
