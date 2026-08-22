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
