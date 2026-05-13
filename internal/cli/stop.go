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
	PrintLogo("s ᴛ ᴏ ᴘ")
	pids := findAllDaemonPIDs()
	if len(pids) == 0 {
		fmt.Println("Daemon is not running.")
		return nil
	}
	if len(pids) > 1 {
		fmt.Printf("Found %d daemon process(es) — stopping all.\n", len(pids))
	}
	var lastErr error
	for _, pid := range pids {
		if err := stopByPID(pid); err != nil {
			lastErr = err
		}
	}
	return lastErr
}

// findAllDaemonPIDs locates every running daemon PID using three strategies,
// deduplicating across all sources so each process is stopped exactly once:
//  1. PID file written by the current binary.
//  2. lsof on the daemon socket (covers binary-path changes).
//  3. pgrep on the command-line pattern (covers socket-path changes, older
//     binaries, and any other fallback case).
func findAllDaemonPIDs() []int {
	seen := make(map[int]bool)
	var pids []int

	add := func(pid int) {
		if pid > 0 && !seen[pid] && processAlive(pid) {
			seen[pid] = true
			pids = append(pids, pid)
		}
	}

	// 1. PID file.
	if pid := readDaemonPID(); pid > 0 {
		add(pid)
	}
	// Clean up stale PID file if the recorded process is gone.
	if filePID := readDaemonPID(); filePID > 0 && !seen[filePID] {
		_ = os.Remove(daemonPIDPath())
	}

	// 2. lsof on the socket.
	add(findPIDViaSocket(daemonSocketPath()))

	// 3. pgrep fallback — returns all matches.
	for _, pid := range findAllDaemonByArgs() {
		add(pid)
	}

	return pids
}

// findAllDaemonByArgs uses pgrep to find ALL "plumb daemon" processes
// regardless of which socket or PID file path they were started with.
func findAllDaemonByArgs() []int {
	out, err := exec.Command("pgrep", "-f", "plumb daemon").Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && pid > 0 && pid != self {
			pids = append(pids, pid)
		}
	}
	return pids
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
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
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

	fmt.Printf("Stopping daemon (PID %d) ...", pid)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		if proc.Signal(syscall.Signal(0)) != nil {
			fmt.Println(" stopped.")
			return nil
		}
		fmt.Print(".")
	}
	fmt.Println()
	fmt.Printf("Warning: daemon (PID %d) did not stop within 5 seconds; it may still be running.\n", pid)
	return nil
}
