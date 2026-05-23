//go:build linux

package monitor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const linuxClockTicksPerSecond = 100

func readProcessMetrics(pid int) (processMetrics, error) {
	var out processMetrics
	if err := readLinuxStatm(pid, &out); err != nil {
		return out, err
	}
	if err := readLinuxCPUTime(pid, &out); err != nil {
		return out, nil
	}
	return out, nil
}

func readLinuxStatm(pid int, out *processMetrics) error {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return nil
	}
	pageSize := uint64(os.Getpagesize()) //nolint:gosec // G115: os.Getpagesize returns a small positive power of two; never negative
	if size, err := strconv.ParseUint(fields[0], 10, 64); err == nil {
		out.VMSBytes = size * pageSize
		out.VMSAvailable = true
	}
	if resident, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
		out.RSSBytes = resident * pageSize
		out.RSSAvailable = true
	}
	return nil
}

func readLinuxCPUTime(pid int, out *processMetrics) error {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return err
	}
	stat := string(data)
	closeParen := strings.LastIndexByte(stat, ')')
	if closeParen < 0 || closeParen+2 >= len(stat) {
		return nil
	}
	fields := strings.Fields(stat[closeParen+2:])
	if len(fields) <= 12 {
		return nil
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return err
	}
	ticks := utime + stime
	//nolint:gosec // G115: ticks is a CPU clock-tick count that cannot approach math.MaxInt64 (≈2.9e9 years of CPU time)
	out.CPUTime = time.Duration(ticks) * time.Second / linuxClockTicksPerSecond
	out.CPUTimeAvailable = true
	return nil
}
