//go:build linux

package procgroup

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestDieWithParent_SetsTheKillSignal pins what the kernel is asked for. The
// signal itself cannot be observed without killing the test process.
func TestDieWithParent_SetsTheKillSignal(t *testing.T) {
	cmd := exec.Command("true")
	Set(cmd)
	DieWithParent(cmd)
	if cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Errorf("Pdeathsig = %v, want SIGKILL", cmd.SysProcAttr.Pdeathsig)
	}
	if !cmd.SysProcAttr.Setpgid {
		t.Error("DieWithParent cleared the process-group setting Set made")
	}
}
