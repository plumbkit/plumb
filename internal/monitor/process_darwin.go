//go:build darwin

package monitor

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)


func readProcessMetrics(pid int) (processMetrics, error) {
	var out processMetrics

	if pid == os.Getpid() {
		var usage unix.Rusage
		if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err == nil {
			out.CPUTime = time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
			out.CPUTimeAvailable = true
			// Maxrss on macOS is peak RSS in bytes since process start.
			// kern.proc.pid Xrssize is unreliable on modern macOS (always 0).
			if usage.Maxrss > 0 {
				out.RSSBytes = uint64(usage.Maxrss)
				out.RSSAvailable = true
			}
		}
	}

	return out, nil
}
