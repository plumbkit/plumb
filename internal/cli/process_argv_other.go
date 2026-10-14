//go:build !linux && !darwin

package cli

import (
	"os/exec"
	"strconv"
	"strings"
)

// processArgv reads a process's argument vector on the platforms that are
// neither Linux nor darwin — in practice the BSDs, where /proc may not exist and
// `ps` does.
//
// The command line is split on whitespace, as `plumb stop`'s pgrep verification
// always has: a path containing a space would defeat this split, which is
// acceptable for the question it answers (is this process OURS) and is recorded
// here rather than hidden. Linux reads /proc (process_argv_linux.go, PLAN-553)
// and darwin asks the kernel (process_argv_darwin.go, PLAN-552).
func processArgv(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output() //nolint:gosec // G204: pid is a strconv-formatted int, not user input
	if err != nil {
		return nil, false
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return nil, false
	}
	return fields, true
}
