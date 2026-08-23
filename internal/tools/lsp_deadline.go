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
// withLSPDeadline; the reported budget is then zero. A caller whose deadline has
// ALREADY passed is a different thing and is reported as attemptExpired: no
// attempt is made and no time is spent, so quoting the configured timeout would
// claim a wait that never happened.
func withFallbackLSPDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	avail := timeout
	dl, hasDeadline := ctx.Deadline()
	if hasDeadline {
		if remaining := time.Until(dl); avail <= 0 || remaining < avail {
			avail = remaining
		}
	}
	if avail <= 0 {
		if hasDeadline {
			return ctx, func() {}, attemptExpired
		}
		return ctx, func() {}, 0
	}
	budget := avail / lspAttemptDivisor
	lspCtx, cancel := context.WithTimeout(ctx, budget)
	return lspCtx, cancel, budget
}

// attemptExpired is the budget withFallbackLSPDeadline reports when the CALLER's
// own deadline had already passed: the server was never given any time, which
// attemptBudget must not conflate with the cap being disabled (0).
const attemptExpired time.Duration = -1

// lspTimeoutErr wraps err with the tool name. A deadline-exceeded failure is
// rewritten into actionable guidance, because the raw "context deadline
// exceeded" leaves the caller with nothing to act on; other errors pass
// through wrapped unchanged.
func lspTimeoutErr(tool string, timeout time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		// A non-positive budget means there was no wait to name: the caller's
		// deadline had already passed, or [lsp_query] is 0. Naming one anyway
		// prints "did not respond within 0s".
		if timeout <= 0 {
			return lspTimedOut(fmt.Errorf("%s: language server did not respond before the deadline "+
				"(it may still be indexing the workspace — retry shortly; %s)", tool, ColdLSPToolsHint))
		}
		return lspTimedOut(fmt.Errorf("%s: language server did not respond within %s "+
			"(it may still be indexing the workspace — retry shortly; %s)", tool, roundedDuration(timeout), ColdLSPToolsHint))
	}
	return fmt.Errorf("%s: %w", tool, err)
}

// attemptBudget resolves the duration to quote to a caller after a language
// server missed its attempt: the budget actually granted, the configured
// [lsp_query] timeout when the cap was disabled (granted is then zero), or ZERO
// when the caller's deadline had already passed (attemptExpired) — there the
// tool waited essentially no time at all, and quoting 30s would be a message
// that is literally false. A zero result means "no wait to name"; every caller
// treats it as such rather than printing it.
func attemptBudget(granted, configured time.Duration) time.Duration {
	switch {
	case granted > 0:
		return granted
	case granted == attemptExpired:
		return 0
	default:
		return configured
	}
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
//     fallback parse has both headroom and a LIVE context to run on. The ONE
//     exception is a disabled cap ([lsp_query] = 0) with an unbounded caller:
//     there is nothing to take half of, so lspCtx == toolCtx and there is no
//     headroom — the documented "0 disables" contract, pinned deliberately by
//     TestWithFallbackLSPDeadline_LeavesHeadroom/no bound at all.
//
// cancel releases both. waited is the attempt budget to quote in a timeout
// message, already resolved through attemptBudget — zero when there was no wait
// to name.
func fallbackDeadlines(ctx context.Context, timeout time.Duration) (toolCtx, lspCtx context.Context, cancel context.CancelFunc, waited time.Duration) {
	toolCtx, cancelTool := withLSPDeadline(ctx, timeout)
	lspCtx, cancelLSP, granted := withFallbackLSPDeadline(toolCtx, timeout)
	return toolCtx, lspCtx, func() { cancelLSP(); cancelTool() }, attemptBudget(granted, timeout)
}
