package tools

import (
	"os/exec"

	"github.com/plumbkit/plumb/internal/procgroup"
)

// sysproc.go gives RunArgv proper process-group cleanup. Without it, killing the
// direct child on timeout leaves grandchildren (e.g. the compiler a `go test`
// forked, or a `sleep` a shell backgrounded) orphaned. The child leads its own
// process group and cancellation signals the whole group; the mechanism lives in
// internal/procgroup, shared with the language-server supervisor.

// setProcessGroup makes the command the leader of a new process group.
func setProcessGroup(cmd *exec.Cmd) { procgroup.Set(cmd) }

// killProcessGroup SIGKILLs the command's whole process group (child + any
// descendants). The child is the group leader, so its pid names the group.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return procgroup.Kill(cmd.Process.Pid)
}
