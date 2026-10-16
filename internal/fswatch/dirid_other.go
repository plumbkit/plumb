//go:build !linux && !darwin

package fswatch

import "os"

// birthTime is unavailable here; identity falls back to os.SameFile alone.
func birthTime(string, os.FileInfo) (int64, bool) { return 0, false }
