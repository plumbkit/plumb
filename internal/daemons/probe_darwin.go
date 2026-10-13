//go:build darwin

package daemons

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/plumbkit/plumb/internal/paths"
)

// Darwin has no /proc, and it has the same fork-before-exec trap as Linux: until
// a forked child execs it still carries its parent's argv, so a daemon starting
// gopls or git looks like a second daemon. A command line cannot answer that on
// its own, so this probe splits the question in two:
//
//   - kern.proc.all (SysctlKinfoProcSlice) answers WHO: every process on the
//     machine with its uid, parent, start time and P_flag, cgo-free.
//   - `ps -axo pid=,command=` answers WHAT the arguments are, in one call.
//
// and only trusts the second for a process the first says has exec'd (P_EXEC).
// Without that filter a forked child would be counted; without the flag test the
// census would be a command-line grep.
//
// A second argv source exists — kern.procargs2, one sysctl per process — and is
// used only when ps cannot run: it is exact, but it costs about 0.44 s on a
// normal machine (measured on macOS arm64, ~1100 processes) against 0.05 s for
// the single ps call, and this census also runs inside daemon_info, which an
// agent calls routinely.
// helperTimeout bounds each external helper (ps, lsof) this probe runs. They are
// read-only and never interactive, but they are separate programs on a machine
// that may have a hung mount, and an unbounded wait here would block the caller
// — daemon_info runs this census, and an agent calls daemon_info routinely. A
// helper that does not answer in time is reported as an unknown reason, never as
// a clean answer.
//
// A var rather than a const so a test can shrink it to prove the bound exists.
var helperTimeout = 3 * time.Second

// platformProbe batches the lock and socket questions into ONE lsof: darwin has
// no /proc/locks, so lsof is the only way to say who has a file open, and a call
// per path costs a process launch each (~150 ms).
func platformProbe(opts Options) probe {
	if _, err := exec.LookPath("lsof"); err != nil {
		// Without lsof the only unanswered questions are who holds a lock and who
		// owns a socket. The argv census still works, and the census reports the
		// missing answers as unknown rather than as nobody.
		return probe{processes: osProcesses}
	}
	listing := newLsofListing(append([]string{opts.Authority}, opts.Sockets...))
	return probe{
		processes: osProcesses,
		owners: func(path string, candidates []int) ([]int, error) {
			pids, err := listing.openers(path)
			if err != nil {
				return nil, err
			}
			return filterPIDs(pids, candidates), nil
		},
	}
}

func osProcesses(uid int) ([]Daemon, []string) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, []string{fmt.Sprintf("cannot read kern.proc.all: %v", err)}
	}
	if len(procs) == 0 {
		return nil, []string{"kern.proc.all listed no processes at all, so no daemon could have been seen"}
	}
	known := make(map[int]Daemon, len(procs))
	unexec := make(map[int]bool, len(procs))
	for i := range procs {
		p := &procs[i]
		pid := int(p.Proc.P_pid)
		if pid <= 0 || int(p.Eproc.Ucred.Uid) != uid {
			continue
		}
		known[pid] = Daemon{
			PID:   pid,
			PPID:  int(p.Eproc.Ppid),
			Start: time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000),
		}
		if !hasExecd(p.Proc.P_flag) {
			// Forked, not yet exec'd: its argv still belongs to its parent. An fd
			// census could not tell this apart either, because a forked child
			// inherits the lock descriptor and CLOEXEC only acts at exec.
			unexec[pid] = true
		}
	}

	lines, err := psCommandLines()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// A ps that timed out is a host problem, not a slow path: falling back
			// to a sysctl per process would spend longer still and answer from the
			// same unhappy machine.
			return nil, []string{fmt.Sprintf("the process table could not be read: %v", err)}
		}
		return daemonsByProcArgs(known, unexec,
			fmt.Sprintf("ps could not be read (%v), so each process's arguments were read directly", err))
	}
	var found []Daemon
	var unknown []string
	for pid, line := range lines {
		d, ok := known[pid]
		if !ok || unexec[pid] {
			continue
		}
		_, command, _ := strings.Cut(line, " ")
		if !psLineMentionsPlumb(command) {
			continue // prefilter only: this keeps the exact read to a handful of pids
		}
		// ps is a PREFILTER, never the verdict: its joined command line cannot be
		// split into arguments when a path contains a space, and a shell that merely
		// quotes the words `plumb daemon` looks identical in it. The process's own
		// arguments decide, read for this pid alone — a handful of sysctls, so the
		// cost stays near the ps figure rather than near the 0.44 s it takes to read
		// every process's arguments.
		exe, args, err := procArgs(pid)
		if err != nil {
			unknown = append(unknown, fmt.Sprintf(
				"cannot read the arguments of pid %d, which ps shows running something named plumb: %v", pid, err))
			continue
		}
		if !isPlumbDaemonArgv(args) {
			continue
		}
		d.Argv = strings.Join(args, " ")
		d.Executable = exe
		d.Identity = IdentityArgv
		found = append(found, d)
	}
	return found, unknown
}

// daemonsByProcArgs is the fallback for a machine where ps cannot run: one
// kern.procargs2 per exec'd process of this user. It reports the degraded source
// as an unknown reason, because a census that fell back should say so rather
// than look exactly like a cheap one.
func daemonsByProcArgs(known map[int]Daemon, unexec map[int]bool, reason string) ([]Daemon, []string) {
	var found []Daemon
	for pid, d := range known {
		if unexec[pid] {
			continue
		}
		exe, args, err := procArgs(pid)
		if err != nil {
			continue // a process whose arguments cannot be read is not evidence of a daemon
		}
		if !isPlumbDaemonArgv(args) {
			continue
		}
		d.Argv = strings.Join(args, " ")
		d.Executable = exe
		d.Identity = IdentityArgv
		found = append(found, d)
	}
	return found, []string{reason}
}

// psCommandLines runs one `ps` and maps pid to its "pid command" line. An empty
// listing is an error even though ps exited zero: this machine always has
// processes, so no processes means ps did not really answer.
func psCommandLines() (map[int]string, error) {
	if _, err := exec.LookPath("ps"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	// CommandContext kills the helper at the deadline and returns an error that
	// wraps ctx.Err(), so the bound needs no extra branch here: the caller sees a
	// deadline either way, and reports it as an unknown reason.
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "pid=,command=").Output() //nolint:gosec // G204: ps with fixed flags, no user input
	if err != nil {
		return nil, fmt.Errorf("ps could not be read: %w", err)
	}
	lines := make(map[int]string)
	for line := range strings.SplitSeq(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pidText, rest, _ := strings.Cut(line, " ")
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid <= 0 {
			continue
		}
		lines[pid] = pidText + " " + rest
	}
	if len(lines) == 0 {
		return nil, errors.New("ps listed no processes")
	}
	return lines, nil
}

// procArgs reads one process's exec path and arguments from kern.procargs2: the
// fallback argv source for a machine where ps cannot run.
func procArgs(pid int) (exe string, args []string, err error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", nil, err
	}
	return parseProcArgs(raw)
}

// lsofListing is one `lsof -F pn` over every path the census cares about, so the
// lock-holder and socket-owner questions are answered from a single process
// launch. It runs on the first question, so a census with nothing to ask never
// pays for it, and it is built once per census rather than once per path.
type lsofListing struct {
	paths  []string
	once   sync.Once
	owners map[string][]int
	err    error
}

func newLsofListing(paths []string) *lsofListing { return &lsofListing{paths: paths} }

func (l *lsofListing) openers(path string) ([]int, error) {
	if path == "" {
		return nil, nil
	}
	l.once.Do(l.run)
	if l.err != nil {
		return nil, l.err
	}
	if pids := l.owners[path]; len(pids) > 0 {
		return pids, nil
	}
	return l.owners[paths.Canonical(path)], nil
}

func (l *lsofListing) run() {
	wanted := make(map[string]bool, len(l.paths))
	args := []string{"-w", "-F", "pn", "--"}
	for _, p := range l.paths {
		if p == "" || wanted[p] {
			continue
		}
		wanted[p] = true
		args = append(args, p)
	}
	if len(wanted) == 0 {
		return
	}
	// -w drops warnings. NOT -b: it is the flag for a machine whose mounts are
	// misbehaving, and on macOS it also suppresses the very attribution this call
	// exists for — measured, `lsof -b -w -F pn -- <path>` returns nothing for a
	// socket or lock file that the same command without -b attributes to the
	// daemon. The hang risk -b was meant to cover is bounded by the context
	// deadline instead, which is the part that actually protects the caller.
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", args...).Output() //nolint:gosec // G204: lsof with fixed flags and paths this process resolved itself
	if err != nil {
		// A helper killed at the deadline returns an error wrapping ctx.Err().
		var exit *exec.ExitError
		// lsof exits 1 when nothing has any of the files open, which is an answer.
		if !errors.As(err, &exit) || len(out) > 0 {
			l.err = fmt.Errorf("lsof: %w", err)
			return
		}
	}
	l.owners = parseLsofOpeners(string(out), wanted)
}

// filterPIDs keeps only the pids in candidates, or all of them when candidates
// is empty.
func filterPIDs(pids, candidates []int) []int {
	if len(candidates) == 0 {
		return pids
	}
	want := make(map[int]bool, len(candidates))
	for _, pid := range candidates {
		want[pid] = true
	}
	var out []int
	for _, pid := range pids {
		if want[pid] {
			out = append(out, pid)
		}
	}
	return out
}
