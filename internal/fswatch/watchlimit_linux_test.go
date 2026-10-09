//go:build linux

package fswatch

import (
	"os"
	"testing"
)

// TestInotifyLimitReached_NotAtTheLimit: on a system with watches to spare,
// the probe reports no limit, and it leaves nothing behind: a thousand probes
// end with as many open descriptors as they started with.
func TestInotifyLimitReached_NotAtTheLimit(t *testing.T) {
	dir := t.TempDir()
	before := openFDs(t)
	for range 1000 {
		if inotifyLimitReached(dir) {
			t.Fatal("reported the inotify watch limit on a system that is not at it")
		}
	}
	if after := openFDs(t); after != before {
		t.Errorf("a thousand probes left %d open descriptors, want %d", after, before)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}
