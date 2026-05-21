package tools

import (
	"context"
	"errors"
	"fmt"
)

// positionErr wraps a raw LSP error that occurred at a cursor position with a
// hint about coordinate conventions, so the caller can self-correct. A
// deadline-exceeded failure is reported as a timeout instead — the coordinate
// hint would be misleading when the server simply never answered.
func positionErr(tool string, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: language server did not respond in time "+
			"(it may still be indexing the workspace — retry shortly)", tool)
	}
	return fmt.Errorf("%s: %w\n\nHint: line and character are 0-based. Ensure the cursor is on a valid identifier and within the file's line length", tool, err)
}
