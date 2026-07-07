//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunArgv_KillsProcessGroupOnTimeout proves the timeout kills the whole
// process group, not just the direct child. The shell backgrounds a grandchild
// that would create a marker after 2s; if only the shell were killed the
// grandchild would survive and the marker would appear. With group-kill it dies
// first and the marker never appears.
func TestRunArgv_KillsProcessGroupOnTimeout(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild-ran")
	// Background a delayed touch, then block so the group is still alive at timeout.
	script := "( sleep 2; touch " + marker + " ) & sleep 10"

	res, err := RunArgv(context.Background(), dir, []string{"sh", "-c", script}, 400*time.Millisecond)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}
	// Wait past the grandchild's 2s mark; if the group was killed it never runs.
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("grandchild survived the timeout — the process group was not killed")
	}
}
