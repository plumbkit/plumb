package tools

import (
	"context"
	"strings"
	"time"
)

// serverNotReadyRetryDelay is how long retryOnServerNotReady waits before
// re-issuing a query that failed because the language server rejected a
// document plumb's adapter just opened. sourcekit-lsp and zls only answer
// per-document requests once their own state — sourcekit-lsp's Swift build
// graph, in particular — catches up with a didOpen that already landed; that
// race is distinct from the daemon-level "still warming" signal (WarmupStatus/
// LSPWarmupFn), which reports the LSP CONNECTION as ready while the server's
// per-document state is not. Short, because this is a narrow indexing race
// observed to resolve within a few hundred milliseconds, not a cold start: one
// bounded retry, not a poll loop.
const serverNotReadyRetryDelay = 400 * time.Millisecond

// isServerNotReadyErr reports whether err is one of the specific per-document
// "not ready yet" rejections observed from the non-Go adapters in the 2026-08
// error autopsy (private/docs/internal/error-autopsy-2026-08.md) —
// sourcekit-lsp's "No language service for '<uri>'" (jsonrpc -32001, returned
// when a query lands before Swift's build graph has resolved a just-opened
// file) and "Failed to find snapshot for '<uri>'" (the same race surfacing
// from PrepareRename/Rename). Distinct from isPositionMissErr (the position
// was fine, there is just no identifier there) and from a hard timeout
// (lspTimeout): matched narrowly on the observed exact phrasing so a retry
// never masks an unrelated failure.
func isServerNotReadyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no language service for") ||
		strings.Contains(msg, "failed to find snapshot for")
}

// retryOnServerNotReady re-issues call once, after serverNotReadyRetryDelay,
// when the first attempt fails with isServerNotReadyErr — the narrow race
// where a non-Go language server rejects a query against a document plumb's
// adapter just opened, before the server's own per-document (or, for
// sourcekit-lsp, build-graph) state has caught up. ctx governs the wait: a
// context that ends first returns the ORIGINAL result without retrying, so
// this can never make a caller wait past its own deadline. Any other error is
// returned unchanged after the first attempt — this is a targeted retry for a
// specific known race, not a general one.
func retryOnServerNotReady[T any](ctx context.Context, call func() (T, error)) (T, error) {
	res, err := call()
	if err == nil || !isServerNotReadyErr(err) {
		return res, err
	}
	select {
	case <-time.After(serverNotReadyRetryDelay):
	case <-ctx.Done():
		return res, err
	}
	return call()
}
