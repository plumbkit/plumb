//go:build darwin

package fswatch

// platformRecordsBirth: darwin's stat always carries a birth time.
func platformRecordsBirth(string) bool { return true }
