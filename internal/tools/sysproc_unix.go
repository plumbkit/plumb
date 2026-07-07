//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// sysproc_unix.go gives RunArgv proper process-group cleanup on Unix. Without it,
// killing the direct child on timeout leaves grandchildren (e.g. the compiler a
// `go test` forked, or a `sleep` a shell backgrounded) orphaned. Setpgid puts the
// child in its own process group; killing the negative pid signals the whole
// group.

// setProcessGroup makes the command the leader of a new process group.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs the command's whole process group (child + any
// descendants). The child is the group leader (Setpgid made pgid == its pid), so
// -pid addresses the group.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
