//go:build darwin

package monitor

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func readProcessMetrics(pid int) (processMetrics, error) {
	var out processMetrics

	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil {
		pageSize := uint64(os.Getpagesize())
		if kp.Eproc.Xrssize > 0 {
			out.RSSBytes = uint64(kp.Eproc.Xrssize) * pageSize
			out.RSSAvailable = true
		}
		if kp.Eproc.Xsize > 0 {
			out.VMSBytes = uint64(kp.Eproc.Xsize) * pageSize
			out.VMSAvailable = true
		}
	}

	if pid == os.Getpid() {
		var usage unix.Rusage
		if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err == nil {
			out.CPUTime = time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
			out.CPUTimeAvailable = true
		}
	}

	return out, nil
}
