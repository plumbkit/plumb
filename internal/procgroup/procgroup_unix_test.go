//go:build unix

package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

// startSleeper starts a long-lived child that leads its own group. Callers must
// not signal pids that are not their own, so the tests only ever use this one.
func startSleeper(t *testing.T) *exec.Cmd {
	t.Helper()
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no 'sleep' binary on PATH")
	}
	cmd := exec.Command(path, "30")
	Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func exitSignal(t *testing.T, cmd *exec.Cmd) syscall.Signal {
	t.Helper()
	_ = cmd.Wait()
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		t.Fatalf("child did not die from a signal: %v", cmd.ProcessState)
	}
	return status.Signal()
}

func TestSet_ChildLeadsItsOwnGroup(t *testing.T) {
	cmd := startSleeper(t)
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("child's group = %d, want its own pid %d", pgid, cmd.Process.Pid)
	}
}

// TestTerminateAndKill_DeliverTheirOwnSignal pins which signal each sends: the
// escalation from a polite request to a kill depends on them being different.
func TestTerminateAndKill_DeliverTheirOwnSignal(t *testing.T) {
	for _, tc := range []struct {
		name string
		send func(int) error
		want syscall.Signal
	}{
		{"Terminate", Terminate, syscall.SIGTERM},
		{"Kill", Kill, syscall.SIGKILL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := startSleeper(t)
			if err := tc.send(cmd.Process.Pid); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got := exitSignal(t, cmd); got != tc.want {
				t.Errorf("child died from %v, want %v", got, tc.want)
			}
		})
	}
}

// TestKill_ReportsAGoneGroupAsProcessDone pins the value exec.Cmd.Cancel needs
// to treat a lost race with the process's own exit as success.
func TestKill_ReportsAGoneGroupAsProcessDone(t *testing.T) {
	cmd := startSleeper(t)
	pid := cmd.Process.Pid
	if err := Kill(pid); err != nil {
		t.Fatalf("first Kill: %v", err)
	}
	_ = cmd.Wait() // reap it, so the group is gone rather than a zombie
	if err := Kill(pid); !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("Kill of a gone group = %v, want os.ErrProcessDone", err)
	}
}

// TestValidGroup_RefusesIDsKillGivesAnotherMeaning checks the guard directly. It
// must not be exercised through Kill: with the guard missing, those ids would
// signal every process the user owns, or the test binary's own group.
func TestValidGroup_RefusesIDsKillGivesAnotherMeaning(t *testing.T) {
	for _, pgid := range []int{-1, 0, 1, syscall.Getpgrp()} {
		if err := validGroup(pgid); err == nil {
			t.Errorf("validGroup(%d) = nil, want a refusal", pgid)
		}
	}
	if err := validGroup(syscall.Getpgrp() + 1_000_000); err != nil {
		t.Errorf("validGroup of an ordinary id = %v, want nil", err)
	}
}
