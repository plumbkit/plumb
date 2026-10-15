//go:build linux

package procgroup

import (
	"os/exec"
	"syscall"
)

// DieWithParent asks the kernel to SIGKILL cmd's direct child if the process
// that started it dies without cleaning up (a SIGKILLed daemon, a crash). It
// is a backstop behind the group kill, which only runs while the parent is
// alive to perform it. Call it before Start.
//
// Two limits, both deliberate:
//
//   - It reaches only the direct child. Its descendants are reparented to init
//     and exit only if they notice the loss of their parent or their pipes.
//   - The kernel delivers the signal when the OS THREAD that forked the child
//     exits, not when the process does. Go keeps its threads for the life of
//     the process unless a goroutine that called runtime.LockOSThread exits
//     still locked, and the supervisor does not lock, so in practice this
//     fires on process death. If a forking thread were ever retired the child
//     would be killed early, which a supervisor treats as a crash and restarts:
//     a spurious restart, never an orphan.
func DieWithParent(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
