//go:build darwin

package daemons

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestProcArgsOverridesThePSLine is the test for darwin's two-step rule: ps is
// only a prefilter, and the process's own arguments are what decide. The
// stand-in is a shell whose command line contains the words `plumb daemon` —
// exactly what ps cannot distinguish from a real daemon — and the kernel's argv
// for it must refuse it.
//
// It starts a shell, never a daemon, so it is safe on a developer's machine.
func TestProcArgsOverridesThePSLine(t *testing.T) {
	// Two commands, so the shell does not replace itself with the last one: the
	// argv has to keep the words that make ps offer it as a candidate.
	cmd := exec.Command("/bin/sh", "-c", "sleep 5; true # plumb daemon")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in shell: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		lines, err := psCommandLines()
		if err != nil {
			t.Fatalf("psCommandLines: %v", err)
		}
		if line, ok := lines[cmd.Process.Pid]; ok {
			_, command, _ := strings.Cut(line, " ")
			// ps must offer this process as worth confirming, or the test would
			// pass without exercising the rule it exists for.
			if !psLineMentionsPlumb(command) {
				t.Fatalf("ps did not show the stand-in as mentioning plumb: %q", command)
			}
			_, args, err := procArgs(cmd.Process.Pid)
			if err != nil {
				t.Fatalf("procArgs for pid %d: %v", cmd.Process.Pid, err)
			}
			if isPlumbDaemonArgv(args) {
				t.Fatalf("the kernel's argv %q was accepted as a daemon; ps must never be the verdict", args)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the stand-in shell never appeared in ps")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
