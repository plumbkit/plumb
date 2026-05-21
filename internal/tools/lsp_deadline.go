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

// lspTimeoutErr wraps err with the tool name. A deadline-exceeded failure is
// rewritten into actionable guidance, because the raw "context deadline
// exceeded" leaves the caller with nothing to act on; other errors pass
// through wrapped unchanged.
func lspTimeoutErr(tool string, timeout time.Duration, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: language server did not respond within %s "+
			"(it may still be indexing the workspace — retry shortly)", tool, timeout)
	}
	return fmt.Errorf("%s: %w", tool, err)
}
