//go:build unix

package procgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Set makes cmd the leader of a new process group (pgid == its pid), so a later
// Terminate or Kill of that pid reaches the whole group. Call it before Start.
func Set(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// Terminate sends SIGTERM to every process in group pgid, the polite request a
// server can use to shut itself and its helpers down.
func Terminate(pgid int) error { return signalGroup(pgid, syscall.SIGTERM) }

// Kill sends SIGKILL to every process in group pgid.
func Kill(pgid int) error { return signalGroup(pgid, syscall.SIGKILL) }

// validGroup refuses group ids that kill(2) would give a different meaning.
// With a non-positive pid kill does not address a group at all (-1 signals every
// process the caller may signal, 0 the caller's own group), and an id equal to
// the caller's own group would signal the caller and everything it started
// alongside. A pgid that was never set up with Set, or a zero read from a
// cleared field, must be refused rather than passed through.
func validGroup(pgid int) error {
	if pgid <= 1 {
		return fmt.Errorf("procgroup: refusing to signal group %d", pgid)
	}
	if pgid == syscall.Getpgrp() {
		return fmt.Errorf("procgroup: refusing to signal our own group %d", pgid)
	}
	return nil
}

// signalGroup delivers sig to group pgid. It returns os.ErrProcessDone when the
// group no longer exists, the value exec.Cmd.Cancel treats as "already gone".
func signalGroup(pgid int, sig syscall.Signal) error {
	if err := validGroup(pgid); err != nil {
		return err
	}
	if err := syscall.Kill(-pgid, sig); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return nil
}
