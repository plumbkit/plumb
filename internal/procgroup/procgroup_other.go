//go:build !unix

package procgroup

import (
	"os"
	"os/exec"
)

// Set is a no-op: there are no process groups to join on this platform.
func Set(*exec.Cmd) {}

// Terminate stops the process with the given pid. There is no gentler signal
// here than Kill, and no group: only that one process is reached.
func Terminate(pid int) error { return Kill(pid) }

// Kill stops the process with the given pid. Only that process is reached.
func Kill(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
