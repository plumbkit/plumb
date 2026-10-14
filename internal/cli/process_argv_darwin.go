//go:build darwin

package cli

import "golang.org/x/sys/unix"

// processArgv reads a process's argument vector on darwin from the kernel
// (sysctl kern.procargs2), not from `ps`.
//
// `ps` was the darwin reader until the PLAN-552 review measured what it costs:
// its output is argv flattened on whitespace, so a binary whose path contains a
// space reads as a different argv[0] and a healthy daemon as "not a plumb
// daemon", and a sandbox that denies the exec answers "unreadable" for every pid.
// Either way a migration silently stops nothing (the PLAN-553 defect class). The
// kernel hands over each argument intact and needs no helper binary.
func processArgv(pid int) ([]string, bool) {
	if pid <= 0 {
		return nil, false
	}
	buf, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, false
	}
	return parseProcArgs2(buf)
}
