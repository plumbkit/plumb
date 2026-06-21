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
		}
	}

	// Maxrss from Getrusage is *peak* RSS since process start (monotonic, never
	// reclaimed after a GC), so it overstates live memory. Source current RSS
	// from the same `ps -o rss=` path used for child processes — for our own pid
	// too — so idle-reclaim is reflected and the reported value is a true sample.
	if rss, ok := processChildRSS(pid); ok {
		out.RSSBytes = rss
		out.RSSAvailable = true
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
