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

// budgetWaitHook is invoked once for every read of a fallback-capable tool's
// budget context Done channel. TWO contexts carry a budget: the tool's own
// (toolCtx, the [lsp_query] bound a write also runs under) and the shorter
// language-server attempt (lspCtx). Production installs a no-op; an internal
// test substitutes a counter and drives the real tool paths, so the warm-path
// guard asserts the MECHANISM ("did anything block on a budget?") with no
// clock, and cannot flake on a loaded runner.
//
// What is observed is a READ of Done, a deliberate over-approximation of a wait:
// every way of blocking on a budget channel goes through Done — a bare
// `<-ctx.Done()` and a bounded `select` with a time.After escape both read it —
// but a read can also happen without any wait. context.WithTimeout(child) reads
// its parent's Done while registering for cancellation, which is exactly why the
// wrapper is installed on the OUTPUT of the context constructors and is never
// handed back in as a parent (see budgetContext).
//
// OUTSIDE this mechanism, and NOT caught: a delay that never touches a budget
// channel at all — a plain time.Sleep, or time.Sleep(time.Until(ctx.Deadline()))
// derived from the deadline rather than from Done. The wall-clock bound this
// replaced would have caught those; this guard does not claim to.
//
// Package-level and mutable, so it has the same contract as syncFileHook: not
// safe for parallel tests. Production never writes it after init.
var budgetWaitHook = func() {}

// budgetContext reports reads of Done to budgetWaitHook. It embeds the real
// budget context and returns that context's own Done channel, so Deadline, Err,
// Value and cancellation are unchanged; only the observation is added.
//
// The wrap happens on the OUTPUT of the context constructors, deliberately:
// passing the wrapper back in as a parent would make the context package read
// Done during cancellation registration, reporting a wait that never happened.
type budgetContext struct{ context.Context }

func (c budgetContext) Done() <-chan struct{} {
	budgetWaitHook()
	return c.Context.Done()
}

// observeBudgetWait wraps a budget context so reads of its Done channel reach
// budgetWaitHook. Apply it LAST: every context derived from the budget must be
// constructed before the wrap, or the derivation itself is observed.
func observeBudgetWait(ctx context.Context) context.Context {
	return budgetContext{Context: ctx}
}

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
	return observeBudgetWait(lspCtx), cancel, budget
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
	rawToolCtx, cancelTool := withLSPDeadline(ctx, timeout)
	// The attempt is derived from the UNWRAPPED tool budget: deriving it from
	// the observer below would read Done during cancellation registration and
	// report a phantom wait, the same reason withFallbackLSPDeadline wraps only
	// its output.
	lspCtx, cancelLSP, granted := withFallbackLSPDeadline(rawToolCtx, timeout)
	return observeBudgetWait(rawToolCtx), lspCtx, func() { cancelLSP(); cancelTool() }, attemptBudget(granted, timeout)
}
