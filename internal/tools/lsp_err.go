package tools

import (
	"context"
	"errors"
	"fmt"
)

// positionErr wraps a raw LSP error that occurred at a cursor position the
// CALLER supplied, with a hint about coordinate conventions so it can
// self-correct. A deadline-exceeded failure is reported as a timeout instead —
// the coordinate hint would be misleading when the server simply never answered.
//
// Use resolvedSymbolErr instead when plumb, not the caller, chose the position.
func positionErr(tool string, err error) error {
	if timeout := lspTimeout(tool, err); timeout != nil {
		return timeout
	}
	return fmt.Errorf("%s: %w\n\nHint: line and character are 0-based. Ensure the cursor is on a valid identifier and within the file's line length", tool, err)
}

// resolvedSymbolErr wraps a raw LSP error for a query whose position plumb
// resolved from a symbol name via the document-symbol tree. The caller passed no
// coordinates, so positionErr's 0-based hint would send it to inspect an
// argument it never supplied. What the failure actually means is that the server
// rejected a position taken from its own symbol tree — almost always because that
// tree is stale — so the hint points there instead.
func resolvedSymbolErr(tool, name string, err error) error {
	if timeout := lspTimeout(tool, err); timeout != nil {
		return timeout
	}
	return fmt.Errorf("%s: %w\n\nHint: the position was resolved from symbol %q via the language server's own document-symbol tree, so it is not a coordinate you passed. The server rejecting it usually means its index is stale after recent edits: call diagnostics to let it re-analyse, then retry", tool, err, name)
}

// lspTimeout returns a timeout error when err is a deadline overrun, else nil.
func lspTimeout(tool string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: language server did not respond in time "+
			"(it may still be indexing the workspace — retry shortly)", tool)
	}
	return nil
}
