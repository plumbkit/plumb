//go:build !unix

package tools

import "os/exec"

// sysproc_other.go is the fallback for non-Unix platforms: no process-group
// semantics, so cancellation kills only the direct child (the stdlib default).

func setProcessGroup(*exec.Cmd) {}

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process != nil {
		return cmd.Process.Kill()
	}
	return nil
}
