//go:build darwin

package daemons

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestHelperCommandsAreBounded is the other half of the timeout: the bound must
// actually be applied, and a helper that does not answer in time must come back
// as a deadline error (which the census turns into an unknown reason) rather than
// hanging the caller.
func TestHelperCommandsAreBounded(t *testing.T) {
	old := helperTimeout
	helperTimeout = time.Nanosecond
	t.Cleanup(func() { helperTimeout = old })

	if _, err := psCommandLines(); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("psCommandLines with a 1ns bound = %v, want a deadline error", err)
	}

	listing := newLsofListing([]string{"/tmp/does-not-matter"})
	if _, err := listing.openers("/tmp/does-not-matter"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("lsof with a 1ns bound = %v, want a deadline error", err)
	}
}

// TestPS_TimeoutIsAnUnknownReason follows the deadline one step further: the
// probe must report an unreadable process table rather than answering "no
// daemons" from a helper that never ran. A clean answer here would be the exact
// fail-open the census exists to prevent.
func TestPS_TimeoutIsAnUnknownReason(t *testing.T) {
	old := helperTimeout
	helperTimeout = time.Nanosecond
	t.Cleanup(func() { helperTimeout = old })

	found, unknown := osProcesses(os.Getuid())
	if len(found) != 0 {
		t.Errorf("a timed-out ps produced %d daemons: %+v", len(found), found)
	}
	if len(unknown) == 0 {
		t.Fatal("a timed-out ps produced no unknown reason, so the census would read as clean")
	}
	if !strings.Contains(strings.Join(unknown, " "), "the process table could not be read") {
		t.Errorf("unknown reasons = %v, want one saying the process table could not be read", unknown)
	}
}

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
