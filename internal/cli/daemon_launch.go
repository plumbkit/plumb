package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Launch authority (PLAN-528).
//
// The daemon's lifetime lock (plumb.daemon.lock) is also the right to launch
// one. Whoever starts a daemon (`plumb serve`, `plumb restart`) takes that lock
// BEFORE starting the child and hands the locked file to it as fd 3; the child
// adopts it as its own lifetime lock, and the launcher only closes its copy,
// which does not unlock (flock locks belong to the open file description, which
// the child shares).
//
// So the lock is held from the moment the child exists, through its whole
// start-up, until it exits. A launcher that finds the lock held knows a daemon
// is running or still starting, and never starts another beside it: it keeps
// dialling until its deadline and then fails. Before this, a serve whose
// daemon took longer than its 10 s wait gave up, released only the spawn lock,
// and the next serve started a second daemon, which lost the lifetime lock and
// exited: two `plumb daemon` processes at once, and an error for the client.

const (
	// defaultDaemonStartWait is how long a launcher waits for the daemon to
	// accept connections. PLUMB_DAEMON_START_TIMEOUT (a Go duration) overrides
	// it, within (0, maxDaemonStartWait].
	defaultDaemonStartWait = 30 * time.Second
	maxDaemonStartWait     = 5 * time.Minute

	// daemonLockFDEnv tells `plumb daemon` that its lifetime lock is already
	// held, on fd inheritedDaemonLockFD (exec.Cmd.ExtraFiles[0]).
	daemonLockFDEnv       = "PLUMB_DAEMON_LOCK_FD"
	inheritedDaemonLockFD = 3
)

// daemonStartWait is the launcher deadline: PLUMB_DAEMON_START_TIMEOUT when it
// parses to a positive Go duration or a whole number of seconds (capped at
// maxDaemonStartWait), else 30 s.
func daemonStartWait() time.Duration {
	v := strings.TrimSpace(os.Getenv("PLUMB_DAEMON_START_TIMEOUT"))
	if v == "" {
		return defaultDaemonStartWait
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		secs, serr := strconv.Atoi(v)
		if serr != nil {
			return defaultDaemonStartWait
		}
		d = time.Duration(secs) * time.Second
	}
	if d <= 0 {
		return defaultDaemonStartWait
	}
	return min(d, maxDaemonStartWait)
}

// daemonLauncher dials the daemon, launching one only while it holds the
// launch authority. Its functions are fields so tests can drive it without a
// daemon. Not safe for concurrent use; build one per launch attempt.
type daemonLauncher struct {
	dial    func() (net.Conn, error)
	acquire func() (*os.File, error) // the lifetime lock, non-blocking
	start   func(lock *os.File) error
	// decided, when set, runs once the launch decision is made: this call
	// started a daemon, or found one holding the authority. Callers release
	// their spawn lock there, so other launchers do not queue behind the wait.
	decided func()
	// onHeld, when set, runs the first time the authority is found held.
	onHeld func()
	wait   time.Duration
	poll   time.Duration
}

func newDaemonLauncher(socketPath string) daemonLauncher {
	return daemonLauncher{
		dial:    func() (net.Conn, error) { return net.DialTimeout("unix", socketPath, time.Second) },
		acquire: acquireDaemonLock,
		start:   startDaemonProcess,
		wait:    daemonStartWait(),
		poll:    100 * time.Millisecond,
	}
}

// The ways a launch can end without a connection; launchError turns each into
// the message the user sees.
var (
	// errDaemonStartTimeout: the daemon this call started never accepted.
	errDaemonStartTimeout = errors.New("daemon start timed out")
	// errDaemonHeldButSilent: another daemon held the authority the whole time
	// and never accepted.
	errDaemonHeldButSilent = errors.New("a plumb daemon holds the launch lock but did not accept connections")
	// errDaemonExitedEarly: the daemon this call started released the
	// authority, so it exited, before it accepted a connection.
	errDaemonExitedEarly = errors.New("the daemon exited before accepting connections")
)

// dialOrLaunch returns a connection to the daemon, starting one at most once,
// and only while holding the launch authority. launched reports whether this
// call started it. It never starts a daemon while another holds the authority,
// whether that one is serving, still starting, or wedged; and a daemon it
// started that exits early is reported at once, not after the whole wait.
func (l daemonLauncher) dialOrLaunch(ctx context.Context) (conn net.Conn, launched bool, err error) {
	deadline := time.Now().Add(l.wait)
	var st launchState
	for {
		if conn, err := l.dial(); err == nil {
			return conn, st.launched, nil
		}
		if err := l.claimOrWait(ctx, &st); err != nil {
			return nil, st.launched, err
		}
		if !time.Now().Before(deadline) {
			if st.held {
				return nil, false, errDaemonHeldButSilent
			}
			return nil, st.launched, errDaemonStartTimeout
		}
		select {
		case <-ctx.Done():
			return nil, st.launched, ctx.Err()
		case <-time.After(l.poll):
		}
	}
}

// launchState is what one dialOrLaunch call has seen so far.
type launchState struct {
	launched bool // this call started a daemon
	held     bool // another daemon held the authority at the last look
	decided  bool // the decided callback has run
}

// claimOrWait makes one pass at the launch authority: start a daemon if it is
// free and none was started yet, wait if another daemon holds it, and report
// a daemon this call started that has since released it by exiting.
func (l daemonLauncher) claimOrWait(ctx context.Context, st *launchState) error {
	lock, err := l.acquire()
	switch {
	case err == nil && st.launched:
		// Our daemon held the authority until it exited.
		_ = lock.Close()
		return errDaemonExitedEarly
	case err == nil:
		return l.launch(ctx, lock, st)
	case errors.Is(err, errDaemonAlreadyRunning):
		if !st.launched {
			// Running or starting: wait for it, never start another.
			if !st.held && l.onHeld != nil {
				l.onHeld()
			}
			st.held = true
			l.decide(st)
		}
		return nil
	default:
		return err
	}
}

// launch starts a daemon that inherits lock, the held authority. The child
// holds it from then on; closing this copy does not unlock. Any failure
// closes it, which releases the authority.
func (l daemonLauncher) launch(ctx context.Context, lock *os.File, st *launchState) error {
	st.held = false
	if err := ctx.Err(); err != nil {
		_ = lock.Close()
		return err
	}
	if err := l.start(lock); err != nil {
		_ = lock.Close()
		return fmt.Errorf("starting daemon: %w", err)
	}
	_ = lock.Close()
	st.launched = true
	l.decide(st)
	return nil
}

func (l daemonLauncher) decide(st *launchState) {
	if !st.decided && l.decided != nil {
		l.decided()
	}
	st.decided = true
}

// launchError is the message a failed dialOrLaunch gives the user; action
// says what did not happen ("start", "come back up").
func launchError(action, socketPath string, wait time.Duration, err error) error {
	switch {
	case errors.Is(err, errDaemonStartTimeout), errors.Is(err, context.DeadlineExceeded):
		return daemonStartTimeoutError(action, socketPath, wait)
	case errors.Is(err, errDaemonHeldButSilent):
		return fmt.Errorf("a plumb daemon holds %s but did not accept connections within %s (socket: %s); "+
			"if it is wedged, see %s, and `plumb stop` stops it%s",
			daemonLockPath(), wait, socketPath, daemonLogPath(), socketPathLengthHint(socketPath))
	case errors.Is(err, errDaemonExitedEarly):
		return fmt.Errorf("the daemon exited before accepting connections (socket: %s); see %s%s",
			socketPath, daemonLogPath(), socketPathLengthHint(socketPath))
	}
	return err
}

// daemonLifetimeLock is the lock runDaemon holds for its lifetime: the one a
// launcher handed over on fd 3, or, for a `plumb daemon` started any other way,
// a fresh non-blocking acquisition.
func daemonLifetimeLock() (*os.File, error) {
	v, ok := os.LookupEnv(daemonLockFDEnv)
	if !ok {
		return acquireDaemonLock()
	}
	// Children of the daemon (language servers, git) must not see it.
	_ = os.Unsetenv(daemonLockFDEnv)
	fd, err := strconv.Atoi(v)
	if err != nil || fd != inheritedDaemonLockFD {
		return nil, fmt.Errorf("%s=%q: refusing to start without the launch lock", daemonLockFDEnv, v)
	}
	return adoptDaemonLock(os.NewFile(uintptr(fd), daemonLockPath()), daemonLockPath())
}

// adoptDaemonLock takes over an inherited lifetime lock. It must be the lock
// file at path (the same inode), and no other description may hold it: if one
// does, it fails closed rather than falling back to a fresh acquisition, which
// could race a daemon that really holds it. (A free lock is simply taken.)
// Nothing may exec between process start and the CloseOnExec below: the
// pre-run (config.Load, theme) only reads files. The fd is made close-on-exec
// so the daemon's own children cannot keep the authority after it dies.
func adoptDaemonLock(f *os.File, path string) (*os.File, error) {
	if f == nil {
		return nil, fmt.Errorf("inherited daemon lock: no fd %d", inheritedDaemonLockFD)
	}
	syscall.CloseOnExec(int(f.Fd()))
	got, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("inherited daemon lock: %w", err)
	}
	want, err := os.Stat(path)
	if err != nil || !os.SameFile(got, want) {
		_ = f.Close()
		return nil, fmt.Errorf("inherited daemon lock is not %s: refusing to start", path)
	}
	// Re-locking a description that already holds the lock succeeds at once.
	// If another daemon holds it through a different description, this fails.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("inherited daemon lock is not held: %w", err)
	}
	return f, nil
}
