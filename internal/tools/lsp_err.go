package tools

import "fmt"

// positionErr wraps a raw LSP error that occurred at a cursor position with a
// hint about coordinate conventions, so the caller can self-correct.
func positionErr(tool string, err error) error {
	return fmt.Errorf("%s: %w\n\nHint: line and character are 0-based. Ensure the cursor is on a valid identifier and within the file's line length", tool, err)
}
