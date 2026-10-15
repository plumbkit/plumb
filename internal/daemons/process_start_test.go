package daemons

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestProcessStart reads this process's own start (before now, and stable across
// reads) and reports a reaped child as gone — the two facts an owner-liveness
// hint is built from.
func TestProcessStart(t *testing.T) {
	start, ok := ProcessStart(os.Getpid())
	if !ok {
		t.Skip("no process table on this platform")
	}
	if start.After(time.Now()) || start.IsZero() {
		t.Fatalf("own start = %v, want a past time", start)
	}
	again, _ := ProcessStart(os.Getpid())
	if !again.Equal(start) {
		t.Fatalf("start changed between reads: %v then %v", start, again)
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	if _, ok := ProcessStart(cmd.Process.Pid); ok {
		t.Fatalf("a reaped child (pid %d) still reports a start", cmd.Process.Pid)
	}
	if _, ok := ProcessStart(0); ok {
		t.Fatal("pid 0 reported a start")
	}
}
