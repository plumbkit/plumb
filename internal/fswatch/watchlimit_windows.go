//go:build windows

package fswatch

// inotifyLimitReached never reports a limit on Windows: ReadDirectoryChangesW
// watches the whole tree through one handle, so there is no per-directory
// watch to run out of.
func inotifyLimitReached(string) bool { return false }
