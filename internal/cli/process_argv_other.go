//go:build !linux

package cli

import (
	"os/exec"
	"strconv"
	"strings"
)

// processArgv reads a process's argument vector on the platforms that are not
// Linux — in practice darwin and the BSDs, where /proc does not exist and `ps`
// always does.
//
// The command line is split on whitespace, as `plumb stop`'s pgrep verification
// always has: a path containing a space would defeat this split, which is
// acceptable for the question it answers (is this process OURS) and is recorded
// here rather than hidden. Linux — where plumb is most often deployed into
// images that may carry no procps at all — reads /proc instead
// (process_argv_linux.go, PLAN-553).
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
