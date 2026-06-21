//go:build darwin

package monitor

import (
	"os"
	"testing"
)

// TestReadProcessMetrics_OwnPidCurrentRSS verifies that the daemon's own RSS is
// sourced as a current sample (via `ps -o rss=`) rather than peak Maxrss, so the
// value is both available and sane. An exact current-vs-peak assertion is not
// feasible, so this checks availability plus a positive byte count.
func TestReadProcessMetrics_OwnPidCurrentRSS(t *testing.T) {
	pm, err := readProcessMetrics(os.Getpid())
	if err != nil {
		t.Fatalf("readProcessMetrics returned error: %v", err)
	}
	if !pm.RSSAvailable {
		t.Fatal("expected RSSAvailable to be true for own pid")
	}
	if pm.RSSBytes == 0 {
		t.Fatal("expected RSSBytes > 0 for own pid")
	}
	if !pm.CPUTimeAvailable {
		t.Fatal("expected CPUTimeAvailable to be true for own pid")
	}
}
