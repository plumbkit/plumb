//go:build linux

package lsp

import (
	"syscall"
	"testing"
)

// TestSupervisor_NewCommand_ParentDeathSignal pins the Linux backstop for a
// daemon that dies without running any cleanup: the kernel SIGKILLs the server.
// The signal cannot be exercised by killing the test process, so this checks
// that the command asks for it.
func TestSupervisor_NewCommand_ParentDeathSignal(t *testing.T) {
	cmd := NewSupervisor("server", nil, nil, SupervisorOptions{}).newCommand(t.Context())
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Errorf("Pdeathsig = %+v, want SIGKILL", cmd.SysProcAttr)
	}
}
