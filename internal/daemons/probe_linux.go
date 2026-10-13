//go:build linux

package daemons

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/plumbkit/plumb/internal/paths"
)

// Linux answers every question from the processes' own files, so the census
// needs no helper binary and cannot be fooled by a command line that merely
// contains the words. /proc is also the only source of the two facts that make
// an argv census exact: the flags that mark a forked-but-not-yet-exec'd child
// (stat field 9) and the process's own argv (cmdline, which after exec belongs
// to nobody else).
//
// Each trap here cost somebody a false duplicate before:
//
//   - comm may contain spaces and parentheses, so the header is matched to the
//     LAST ") " rather than the first;
//   - a process that exits mid-scan makes a read fail while its directory is
//     still listed — that is a skip, but a read that fails while the directory
//     still exists is unknown, never "gone";
//   - "no processes listed" is not "no processes": this process is one.

const (
	// userHZ is the kernel's fixed USER_HZ. /proc reports starttime in clock
	// ticks sized by USER_HZ, which Linux defines as 100 whatever the configured
	// kernel HZ is, so no sysconf call is needed to turn it into seconds.
	userHZ = 100
)

// platformProbe answers every question per path: the descriptors of the
// candidate processes are read directly, so there is nothing to batch on Linux.
func platformProbe(Options) probe {
	return probe{processes: osProcesses, owners: osOwners, lockHeld: osLockHeld}
}

// osLockHeld reports whether path is held by a flock, read from /proc/locks.
// Existence only — see lockHeldBy for why /proc/locks cannot name the holder.
func osLockHeld(path string) (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	data, err := os.ReadFile("/proc/locks")
	if err != nil {
		return false, fmt.Errorf("cannot read /proc/locks: %w", err)
	}
	want := fmt.Sprintf("%02x:%02x:%d", unix.Major(st.Dev), unix.Minor(st.Dev), st.Ino)
	return lockHeldBy(string(data), want), nil
}

func osProcesses(uid int) ([]Daemon, []string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, []string{fmt.Sprintf("cannot list /proc: %v", err)}
	}
	boot := bootTime()
	var found []Daemon
	var unknown []string
	listed := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		listed++
		d, reason := readProc(pid, uid, boot)
		if reason != "" {
			unknown = append(unknown, reason)
		}
		if d != nil {
			found = append(found, *d)
		}
	}
	if listed == 0 {
		unknown = append(unknown, "/proc lists no processes at all, so no daemon could have been seen")
	}
	return found, unknown
}

// readProc reports what /proc/<pid> says. A returned reason is a fact that could
// not be read; a nil Daemon with no reason means the process is simply not a
// plumb daemon of this user (or exited mid-scan).
func readProc(pid, uid int, boot time.Time) (*Daemon, string) {
	base, reason := procBase(pid, uid)
	if base == "" {
		return nil, reason
	}
	fields, reason := procStatFields(base, pid)
	if fields == nil {
		return nil, reason
	}
	flags, err := strconv.ParseUint(fields[statFlagsField], 10, 32)
	if err != nil {
		return nil, fmt.Sprintf("%s/stat: flags %q is not a number", base, fields[statFlagsField])
	}
	if forkedNotYetExec(flags) {
		return nil, "" // forked, not yet exec'd: its argv is still its parent's
	}
	args, reason := procArgv(base)
	if args == nil {
		return nil, reason
	}

	d := Daemon{PID: pid, Argv: strings.Join(args, " "), Identity: IdentityArgv}
	if ppid, err := strconv.Atoi(fields[1]); err == nil {
		d.PPID = ppid
	}
	if ticks, err := strconv.ParseUint(fields[statStartField], 10, 64); err == nil && !boot.IsZero() {
		//nolint:gosec // G115: ticks is a count of clock ticks since boot and cannot approach math.MaxInt64 (that would be some 2.9e9 years of uptime)
		d.Start = boot.Add(time.Duration(ticks) * time.Second / userHZ)
	}
	if exe, err := os.Readlink(base + "/exe"); err == nil {
		d.Executable = exe
	}
	return &d, ""
}

// procBase returns /proc/<pid> when that process belongs to uid. A blank path
// with no reason means "not ours, or gone" — a skip, not a failure.
func procBase(pid, uid int) (base string, reason string) {
	base = "/proc/" + strconv.Itoa(pid)
	info, err := os.Stat(base)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "" // exited between the listing and now
		}
		return "", fmt.Sprintf("cannot stat %s: %v", base, err)
	}
	// The directory's owner IS the process's real uid, which is one stat cheaper
	// than /proc/<pid>/status and never touches another user's file.
	if st, ok := info.Sys().(*unix.Stat_t); ok && int(st.Uid) != uid {
		return "", ""
	}
	return base, ""
}

// procStatFields reads and parses /proc/<pid>/stat, reporting a reason when a
// process that still exists cannot be read.
func procStatFields(base string, pid int) (fields []string, reason string) {
	stat, err := os.ReadFile(base + "/stat")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, fmt.Sprintf("cannot read %s/stat: %v", base, err)
	}
	fields, err = statFields(pid, string(stat))
	if err != nil {
		return nil, fmt.Sprintf("%s/stat: %v", base, err)
	}
	return fields, ""
}

// procArgv reads the process's own arguments, which after exec belong to nobody
// else. A nil result with no reason means the process is not a plumb daemon.
func procArgv(base string) (args []string, reason string) {
	cmdline, err := os.ReadFile(base + "/cmdline")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ""
		}
		return nil, fmt.Sprintf("cannot read %s/cmdline: %v", base, err)
	}
	args = splitNUL(cmdline)
	if !isPlumbDaemonArgv(args) {
		return nil, ""
	}
	return args, ""
}

// bootTime reads btime from /proc/stat, the anchor /proc's per-process start
// ticks are measured from. A zero time means "unknown": the caller leaves the
// start time unset rather than reporting a start time relative to nothing.
func bootTime() time.Time {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		v, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return time.Time{}
		}
		return time.Unix(secs, 0)
	}
	return time.Time{}
}

// splitNUL splits a /proc cmdline (NUL-separated, trailing NUL) into arguments.
func splitNUL(b []byte) []string {
	s := strings.TrimRight(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

// osOwners names which of candidates has the file at path open.
//
// This is NOT /proc/locks, and the difference is the whole reason the function
// exists. A flock belongs to the open file DESCRIPTION, and /proc/locks keeps
// naming the process that acquired it: with #649's inherited lock, that is the
// `plumb serve` launcher, while the daemon — whose fd 3 shares that description
// and which holds it for its whole life — is the process that can still be
// observed holding the file open. Measured in a container: /proc/locks named the
// launcher while the daemon's fd 3 was the only open descriptor.
//
// Only candidates are scanned, never the whole process table: a scan of every
// descriptor on a busy machine costs thousands of reads, and this runs on every
// `daemon_info` call.
func osOwners(path string, candidates []int) ([]int, error) {
	targets, err := fileTargets(path)
	if err != nil || len(targets) == 0 {
		return nil, err
	}
	var pids []int
	for _, pid := range candidates {
		dir := "/proc/" + strconv.Itoa(pid) + "/fd"
		fds, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // it exited
			}
			return nil, fmt.Errorf("cannot read %s: %w", dir, err)
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(dir, fd.Name()))
			if err != nil {
				continue // closed between the listing and the read
			}
			if targets[target] {
				pids = append(pids, pid)
				break
			}
		}
	}
	return pids, nil
}

// fileTargets returns the descriptor spellings that identify path: a regular
// file appears under its own name (and its resolved name), while a unix socket
// appears as "socket:[<inode>]" — where the inode is the SOCKET's, not the
// directory entry's, so the two cannot be compared and /proc/net/unix is the
// only place that mapping exists.
func fileTargets(path string) (map[string]bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil // nothing is there, so nobody has it open
		}
		return nil, err
	}
	if st.Mode()&os.ModeSocket != 0 {
		data, err := os.ReadFile("/proc/net/unix")
		if err != nil {
			return nil, fmt.Errorf("cannot read /proc/net/unix: %w", err)
		}
		ino, found := parseProcNetUnix(string(data), path)
		if !found {
			return nil, nil // the socket file exists but nothing is listening on it
		}
		return map[string]bool{"socket:[" + strconv.FormatUint(ino, 10) + "]": true}, nil
	}
	out := map[string]bool{path: true}
	if resolved := paths.Canonical(path); resolved != path {
		out[resolved] = true
	}
	return out, nil
}
