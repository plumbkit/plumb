//go:build darwin

package daemons

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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
	listing := newLsofListing(append(append([]string{opts.Authority}, opts.Sockets...), opts.LegacyLocks...))
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
	return ParseProcArgs(raw)
}

// lsofListing is one `lsof -F pn` over every path the census cares about, so the
// lock-holder and socket-owner questions are answered from a single process
// launch. It runs on the first question, so a census with nothing to ask never
// pays for it, and it is built once per census rather than once per path.
type lsofListing struct {
	paths []string
	once  sync.Once
	// owners maps a wanted path to the pids that have it open.
	owners map[string][]int
	// unreadable names each path the census could not even ask about, with the
	// reason: its existence could not be established, so "nobody has it open" is
	// not an answer this listing is entitled to give.
	unreadable map[string]error
	// partial records that lsof listed some paths and complained while doing so.
	// A path no record names is then not proven unheld — lsof stopped at the path
	// it could not read, and which one that was is not something its stderr says
	// reliably — so the path is reported as unknown rather than as nobody.
	partial bool
	err     error
}

func newLsofListing(paths []string) *lsofListing {
	return &lsofListing{paths: paths, unreadable: make(map[string]error)}
}

func (l *lsofListing) openers(path string) ([]int, error) {
	if path == "" {
		return nil, nil
	}
	l.once.Do(l.run)
	if l.err != nil {
		return nil, l.err
	}
	if err := l.unreadable[path]; err != nil {
		return nil, err
	}
	if pids := l.owners[path]; len(pids) > 0 {
		return pids, nil
	}
	if pids := l.owners[paths.Canonical(path)]; len(pids) > 0 {
		return pids, nil
	}
	if l.partial {
		return nil, fmt.Errorf("lsof listed some of the paths it was given but not %s, and complained while listing: it does not answer for it", path)
	}
	return nil, nil
}

func (l *lsofListing) run() {
	wanted := make(map[string]bool, len(l.paths))
	args := []string{"-w", "-F", "pn", "--"}
	for _, p := range l.paths {
		if p == "" || wanted[p] {
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Not there: nobody has it open, by definition, so it is dropped
				// rather than asked about. lsof answers a missing path with a
				// status error on stderr and exit 1, which is indistinguishable
				// from a failure — and an ordinary idle account (its lock file
				// never created, its socket unlinked) would then read as unknown
				// instead of as "no daemon is running".
				continue
			}
			// Its existence could not even be established — a parent directory
			// that denies search, say. That is a gap, not the answer "nobody has
			// it open", and the caller must hear it as one.
			l.unreadable[p] = fmt.Errorf("cannot tell whether %s is there: %w", p, err)
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
	// stderr is captured rather than discarded: what lsof complains about is the
	// only thing that separates its exit 1 as an ANSWER ("nothing has any of
	// these open") from its exit 1 as a FAILURE.
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "lsof", args...) //nolint:gosec // G204: lsof with fixed flags and paths this process resolved itself
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if l.err = lsofOutcome(ctx, err, out, stderr.Bytes()); l.err != nil {
		return
	}
	// Output that came WITH a complaint is partial: lsof stopped at the path it
	// could not read (measured: a live socket beside an unreadable symlink is
	// listed, with the status error on stderr), so a path no record names is not
	// thereby proven unheld and the listing must say so rather than answer
	// "nobody".
	l.partial = len(out) > 0 && stderr.Len() > 0
	l.owners = parseLsofOpeners(string(out), wanted)
}

// lsofOutcome decides what one lsof run means: nil for an answer — including
// partial output — or the error the census must report as unknown.
//
// lsof exits 1 both when it cannot read a path it was given and when nothing has
// any of its paths open, so the exit status alone cannot tell an answer from a
// failure. The complaint on stderr is what separates them, and the difference is
// everything: an empty listing read as the answer "nobody holds the authority"
// becomes a violation against a daemon that is holding it perfectly well.
func lsofOutcome(ctx context.Context, err error, out, stderr []byte) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Killed at the deadline: a helper that never finished did not answer.
		return fmt.Errorf("lsof: %w", ctxErr)
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return fmt.Errorf("lsof: %w", err)
	}
	if len(out) > 0 {
		// Usable partial output. lsof gives up on the paths it could not read but
		// still prints the ones it could (measured: a missing lock beside an open
		// socket still lists the socket), and discarding that would lose real
		// attribution.
		return nil
	}
	if msg := firstLine(string(stderr)); msg != "" {
		// Empty output AND a complaint is an error, whatever the exit status.
		// This is the case that must never be read as "nobody has it open".
		if err == nil {
			return errors.New("lsof: " + msg)
		}
		return fmt.Errorf("lsof: %w: %s", err, msg)
	}
	return nil // exit 1, nothing open, nothing complained about: an answer
}

// firstLine keeps a helper's complaint to its first line: lsof follows a status
// error with its whole usage text, and this message is rendered into a one-line
// verdict.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
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

// ProcessStart reports when process pid started, read from the kernel; false
// when there is no such process. A pid plus its start time names one process
// for life, which a pid alone does not once it is recycled.
func ProcessStart(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || p == nil || int(p.Proc.P_pid) != pid {
		return time.Time{}, false
	}
	return time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000), true
}
