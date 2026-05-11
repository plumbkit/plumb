package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the background daemon",
	RunE:  runStop,
}

func runStop(_ *cobra.Command, _ []string) error {
	pid := findDaemonPID()
	if pid == 0 {
		fmt.Println("Daemon is not running.")
		return nil
	}
	return stopByPID(pid)
}

// findDaemonPID locates the daemon PID using three strategies in order:
//  1. PID file written by the current binary.
//  2. lsof on the daemon socket (covers binary-path changes).
//  3. pgrep on the command-line pattern (covers socket-path changes, older
//     binaries, and any other fallback case).
func findDaemonPID() int {
	if pid := readDaemonPID(); pid > 0 && processAlive(pid) {
		return pid
	}
	// Clean up stale PID file if present.
	_ = os.Remove(daemonPIDPath())

	if pid := findPIDViaSocket(daemonSocketPath()); pid > 0 {
		return pid
	}

	return findDaemonByArgs()
}

// findDaemonByArgs uses pgrep to find a "plumb daemon" process regardless of
// which socket or PID file path it was started with.
func findDaemonByArgs() int {
	out, err := exec.Command("pgrep", "-f", "plumb daemon").Output()
	if err != nil {
		return 0
	}
	self := os.Getpid()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && pid > 0 && pid != self {
			return pid
		}
	}
	return 0
}

// processAlive returns true if a process with the given PID exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// readDaemonPID reads the PID file written by the current daemon. Returns 0
// if the file does not exist or cannot be parsed.
func readDaemonPID() int {
	data, err := os.ReadFile(daemonPIDPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// findPIDViaSocket uses lsof to find the PID of the process that owns
// socketPath. Works on macOS and Linux without root privileges.
func findPIDViaSocket(socketPath string) int {
	out, err := exec.Command("lsof", "-t", socketPath).Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// stopByPID sends SIGTERM to pid and waits up to 5 seconds for it to exit.
func stopByPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.Signal(0)) != nil {
		fmt.Println("Daemon is not running (stale reference cleaned up).")
		_ = os.Remove(daemonPIDPath())
		return nil
	}

	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("sending SIGTERM to daemon (PID %d): %w", pid, err)
	}

	fmt.Printf("Stopping daemon (PID %d)", pid)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if proc.Signal(syscall.Signal(0)) != nil {
			fmt.Println(" done.")
			return nil
		}
		fmt.Print(".")
	}
	fmt.Println()
	fmt.Printf("Warning: daemon (PID %d) did not stop within 5 seconds.\n", pid)
	return nil
}
