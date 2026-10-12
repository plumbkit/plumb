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
// parses to a positive duration (capped at maxDaemonStartWait), else 30 s.
func daemonStartWait() time.Duration {
	v := strings.TrimSpace(os.Getenv("PLUMB_DAEMON_START_TIMEOUT"))
	if v == "" {
		return defaultDaemonStartWait
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
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
	wait    time.Duration
	poll    time.Duration
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

// errDaemonHeldButSilent is wrapped into the timeout error when the authority
// was held by another daemon the whole time: it exists but did not accept.
var errDaemonHeldButSilent = errors.New("a plumb daemon holds the launch lock but did not accept connections")

// dialOrLaunch returns a connection to the daemon, starting one at most once,
// and only while holding the launch authority. launched reports whether this
// call started it. It never starts a daemon while another holds the authority,
// whether that one is serving, still starting, or wedged.
func (l daemonLauncher) dialOrLaunch(ctx context.Context) (conn net.Conn, launched bool, err error) {
	deadline := time.Now().Add(l.wait)
	held := false
	for {
		if conn, err := l.dial(); err == nil {
			return conn, launched, nil
		}
		if !launched {
			lock, err := l.acquire()
			switch {
			case err == nil:
				held = false
				if err := l.start(lock); err != nil {
					_ = lock.Close() // releases the authority: no child holds it
					return nil, false, fmt.Errorf("starting daemon: %w", err)
				}
				// The child holds the lock now; closing our copy does not unlock.
				_ = lock.Close()
				launched = true
			case errors.Is(err, errDaemonAlreadyRunning):
				held = true // running or starting: wait for it, never start another
			default:
				return nil, false, err
			}
		}
		if !time.Now().Before(deadline) {
			if held {
				return nil, false, fmt.Errorf("%w within %s", errDaemonHeldButSilent, l.wait)
			}
			return nil, launched, errDaemonStartTimeout
		}
		select {
		case <-ctx.Done():
			return nil, launched, ctx.Err()
		case <-time.After(l.poll):
		}
	}
}

// errDaemonStartTimeout marks a launch whose daemon never accepted in time;
// callers turn it into daemonStartTimeoutError with their own action text.
var errDaemonStartTimeout = errors.New("daemon start timed out")

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
// file at path (the same inode) and still held through this descriptor;
// anything else fails closed rather than falling back to a fresh acquisition,
// which could race a daemon that really holds it. The fd is made close-on-exec
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
