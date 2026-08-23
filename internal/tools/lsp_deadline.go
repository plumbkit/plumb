package tools

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// withLSPDeadline bounds a single LSP operation so a slow, still-indexing, or
// wedged language server cannot hang the tool until the MCP client's own
// timeout fires. A non-positive timeout disables the cap; an existing deadline
// on ctx is left untouched (the caller already bounds the work). Mirrors
// applySearchDeadline in search_in_files.go.
func withLSPDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// lspAttemptDivisor splits the time available to a tool that can answer from
// the tree-sitter index instead of the language server: the server attempt gets
// 1/lspAttemptDivisor of it and the remainder is reserved for the fallback
// parse. Halving is deliberate rather than tuned — the fallback is a local
// parse measured in milliseconds, so the reserve only has to be non-zero, while
// the server still gets the larger practical share of a realistic budget.
const lspAttemptDivisor = 2

// withFallbackLSPDeadline bounds the language-server attempt of a tool that has
// a local tree-sitter fallback, and reports the budget it granted so the tool
// can name it in a timeout message.
//
// It differs from withLSPDeadline in the two ways that made the fallback
// unreachable for a server that is merely SLOW rather than broken (PLAN-390):
//
//   - The attempt ALWAYS ends strictly before the time available to the tool,
//     including when the caller already imposed a deadline. withLSPDeadline
//     passes an already-bounded ctx straight through, spending every last
//     nanosecond on the server, so no caller whose patience equals the tool's
//     budget can ever observe the fallback.
//   - The caller keeps the parent ctx to run the fallback on. The fallback must
//     NOT inherit the returned one: topology's safeExtract refuses to start a
//     parse on an expired context, so a fallback invoked with the timed-out LSP
//     context reports "unavailable" and the tool surfaces the timeout instead —
//     dead exactly where it was written to help.
//
// A non-positive timeout with no caller deadline disables the cap, matching
// withLSPDeadline; the reported budget is then zero.
func withFallbackLSPDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	avail := timeout
	if dl, ok := ctx.Deadline(); ok {
		if remaining := time.Until(dl); avail <= 0 || remaining < avail {
			avail = remaining
		}
	}
	if avail <= 0 {
		return ctx, func() {}, 0
	}
	budget := avail / lspAttemptDivisor
	lspCtx, cancel := context.WithTimeout(ctx, budget)
	return lspCtx, cancel, budget
}

// lspTimeoutErr wraps err with the tool name. A deadline-exceeded failure is
// rewritten into actionable guidance, because the raw "context deadline
// exceeded" leaves the caller with nothing to act on; other errors pass
// through wrapped unchanged.
func lspTimeoutErr(tool string, timeout time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return lspTimedOut(fmt.Errorf("%s: language server did not respond within %s "+
			"(it may still be indexing the workspace — retry shortly; %s)", tool, timeout, ColdLSPToolsHint))
	}
	return fmt.Errorf("%s: %w", tool, err)
}

// attemptBudget resolves the duration to quote to a caller after a language
// server missed its attempt: the budget actually granted, or the configured
// [lsp_query] timeout when the cap was disabled (granted is then zero).
func attemptBudget(granted, configured time.Duration) time.Duration {
	if granted <= 0 {
		return configured
	}
	return granted
}

// fallbackDeadlines splits a tool's time into the two contexts a
// language-server-with-tree-sitter-fallback tool needs, WITHOUT widening the
// tool's own budget:
//
//   - toolCtx keeps exactly the bound the tool has always had (withLSPDeadline,
//     the [lsp_query] timeout). Everything downstream — including a WRITE — stays
//     inside it. This is the deliberate half: a symbol-edit tool must not become
//     unbounded just because its lookup learned to give up earlier (PLAN-403).
//   - lspCtx is the server attempt, half of what remains of toolCtx, so the
//     fallback parse has both headroom and a LIVE context to run on.
//
// cancel releases both. waited is the attempt budget to quote in a timeout
// message, already resolved through attemptBudget.
func fallbackDeadlines(ctx context.Context, timeout time.Duration) (toolCtx, lspCtx context.Context, cancel context.CancelFunc, waited time.Duration) {
	toolCtx, cancelTool := withLSPDeadline(ctx, timeout)
	lspCtx, cancelLSP, granted := withFallbackLSPDeadline(toolCtx, timeout)
	return toolCtx, lspCtx, func() { cancelLSP(); cancelTool() }, attemptBudget(granted, timeout)
}
