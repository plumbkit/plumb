//go:build linux

package cli

import (
	"fmt"
	"os"
	"strings"
)

// processArgv reads a process's argument vector on Linux.
//
// It reads /proc directly rather than shelling out, and that is the whole point
// of this file: `ps` is not present in every environment plumb runs in —
// debian:bookworm-slim and minimal container images generally ship no procps —
// and a `plumb restart` that cannot identify a daemon because a helper binary is
// missing stops nothing and then correctly refuses to start a successor. The
// result is fail-closed and the daemon count still never exceeds one, but the
// migration silently never happens: exactly what the PLAN-518 flip probe caught
// in its §5 upgrade section.
//
// /proc/<pid>/cmdline is the kernel's own NUL-separated argv, with a trailing
// NUL. A process that exited between the caller's liveness check and this read
// has no entry, and "no argv" is reported as not-a-daemon, which is the safe
// direction: this answer only ever licenses SIGNALLING a process, so a miss is
// recoverable and a false positive is not.
func processArgv(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)) //nolint:gosec // G703: pid is an int formatted with %d, so this path cannot traverse anywhere — there is no string input to taint
	if err != nil || len(data) == 0 {
		return nil, false
	}
	argv := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(argv) == 0 || argv[0] == "" {
		return nil, false
	}
	return argv, true
}
