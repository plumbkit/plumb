//go:build !linux && !darwin

package fswatch

// platformRecordsBirth: dirID reads no birth time on this platform.
func platformRecordsBirth(string) bool { return false }
