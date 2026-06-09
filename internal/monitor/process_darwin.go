//go:build darwin

package monitor

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
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

// processChildRSS samples an arbitrary process's RSS on macOS. Getrusage only
// reports the calling process, and kern.proc Xrssize is unreliable on modern
// macOS, so the portable path is `ps -o rss=` (reports RSS in KiB).
func processChildRSS(pid int) (uint64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	//nolint:gosec // G204: pid is an OS process ID (int formatted via strconv), not user-controlled input
	out, err := exec.CommandContext(ctx, "ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	kb, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return kb * 1024, true
}
