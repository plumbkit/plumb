package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Launch authority (PLAN-527, generalising #649).
//
// The daemon's two lifetime locks (lock.go) are also the right to launch one.
// Whoever starts a daemon (`plumb serve`, `plumb restart`) takes them in their
// one order — the account authority first, then the runtime lifetime lock —
// BEFORE starting the child, and hands them to it on fds 3 and 4; the child
// adopts them as its own, and the launcher only closes its copies, which does
// not unlock (flock locks belong to the open file description, which the child
// shares).
//
// So the locks are held from the moment the child exists, through its whole
// start-up, until it exits. A launcher that finds either held knows a daemon is
// running or still starting, and never starts another beside it: it keeps
// dialling until its deadline and then fails. Before #649, a serve whose daemon
// took longer than its 10 s wait gave up, released only the spawn lock, and the
// next serve started a second daemon, which lost the lifetime lock and exited:
// two `plumb daemon` processes at once, and an error for the client.
//
// The authority is account-wide and outside the runtime directory, so a launcher
// that finds it held by a daemon its own environment cannot reach dials the
// socket that daemon recorded in the authority metadata instead
// (dialHeldAuthority): a client under a different HOME reaches the one daemon
// rather than waiting out its deadline and failing.

const (
	// defaultDaemonStartWait is how long a launcher waits for the daemon to
	// accept connections. PLUMB_DAEMON_START_TIMEOUT (a Go duration) overrides
	// it, within (0, maxDaemonStartWait].
	defaultDaemonStartWait = 30 * time.Second
	maxDaemonStartWait     = 5 * time.Minute
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

// launchLocks are the two locks a launcher holds while it starts a daemon: the
// account authority and the runtime lifetime lock (lock.go). Not safe for
// concurrent use.
type launchLocks struct {
	Authority *os.File
	Lifetime  *os.File
}

// files returns the locks in handover order, which is the order of
// exec.Cmd.ExtraFiles: the authority becomes fd 3 and the lifetime lock fd 4. A
// nil lock is skipped, which shifts the later ones — the child then refuses the
// fd, because it does not name the file its env var says it should.
func (l launchLocks) files() []*os.File {
	var out []*os.File
	for _, f := range []*os.File{l.Authority, l.Lifetime} {
		if f != nil {
			out = append(out, f)
		}
	}
	return out
}

// closeAll closes both copies. Once the child holds the same descriptions this
// releases nothing; with no child started it releases both locks.
func (l launchLocks) closeAll() {
	for _, f := range l.files() {
		_ = f.Close()
	}
}

// empty reports whether the launcher holds neither lock, which is not a state a
// launch may proceed from.
func (l launchLocks) empty() bool {
	return l.Authority == nil && l.Lifetime == nil
}

// acquireLaunchLocks takes both locks, in the one order: the account authority
// first, then the runtime lifetime lock.
//
// If the lifetime lock is held, the authority is released before
// errDaemonAlreadyRunning is returned: a launcher that loses the race leaves the
// account's authority exactly as it found it, so a daemon that is only starting
// never appears to hold a lock it did not take.
func acquireLaunchLocks() (launchLocks, error) {
	authority, err := acquireAuthority()
	if err != nil {
		return launchLocks{}, err
	}
	lifetime, err := acquireDaemonLock()
	if err != nil {
		_ = authority.Close()
		return launchLocks{}, err
	}
	return launchLocks{Authority: authority, Lifetime: lifetime}, nil
}

// daemonLauncher dials the daemon, launching one only while it holds the launch
// locks. Its functions are fields so tests can drive it without a daemon. Not
// safe for concurrent use; build one per launch attempt.
type daemonLauncher struct {
	dial func() (net.Conn, error)
	// acquire takes both locks non-blocking; an error leaves neither of them
	// held, so a launcher that loses the race holds nothing at all.
	acquire func() (launchLocks, error)
	start   func(launchLocks) error
	// socket is the MCP socket this launcher is starting a daemon for. It is
	// recorded in the authority metadata for the length of the launch, so a
	// reader that cannot see the child yet still learns a daemon is coming.
	socket string
	// dialHeld, when set, dials the MCP socket the current holder of the
	// authority recorded in its metadata, and reports whether it connected.
	// It is how a client whose environment resolved elsewhere reaches the one
	// daemon, and a field so tests can drive that path without a daemon.
	dialHeld func() (net.Conn, bool)
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
		socket:   socketPath,
		dial:     func() (net.Conn, error) { return net.DialTimeout("unix", socketPath, time.Second) },
		acquire:  acquireLaunchLocks,
		start:    startDaemonProcess,
		dialHeld: dialHeldAuthority,
		wait:     daemonStartWait(),
		poll:     100 * time.Millisecond,
	}
}

// dialHeldAuthority dials the MCP socket the daemon holding the account
// authority recorded there, when there is a record worth dialling (see
// attachSocket). A missing or untrustworthy record reports false, and the
// launcher falls back to redialling its own sockets.
func dialHeldAuthority() (net.Conn, bool) {
	path, err := authorityPath()
	if err != nil {
		return nil, false
	}
	return dialAuthoritySocket(path)
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
	// errNoLaunchLocks: a launcher with neither lock in hand may not spawn.
	errNoLaunchLocks = errors.New("no launch locks are held")
)

// dialOrLaunch returns a connection to the daemon, starting one at most once,
// and only while holding both launch locks. launched reports whether this call
// started it. It never starts a daemon while another holds either lock, whether
// that one is serving, still starting, or wedged; a daemon it started that exits
// early is reported at once, not after the whole wait; and while another holds
// the authority it also tries the socket that holder recorded.
func (l daemonLauncher) dialOrLaunch(ctx context.Context) (conn net.Conn, launched bool, err error) {
	deadline := time.Now().Add(l.wait)
	var st launchState
	for {
		if conn, err := l.dial(); err == nil {
			return conn, st.launched, nil
		}
		held, err := l.claimOrWait(ctx, &st)
		if err != nil {
			return nil, st.launched, err
		}
		if held != nil {
			return held, st.launched, nil
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

// claimOrWait makes one pass at the launch locks: start a daemon if they are
// free and none was started yet, wait if another daemon holds them, and report a
// daemon this call started that has since released them by exiting. While
// another holds the authority it returns a connection to the socket that holder
// recorded, when there is one.
func (l daemonLauncher) claimOrWait(ctx context.Context, st *launchState) (net.Conn, error) {
	locks, err := l.acquire()
	switch {
	case err == nil && st.launched:
		// Our daemon held the locks until it exited.
		locks.closeAll()
		return nil, errDaemonExitedEarly
	case err == nil:
		return nil, l.launch(ctx, locks, st)
	case errors.Is(err, errDaemonAlreadyRunning):
		if !st.launched {
			// Running or starting: wait for it, never start another.
			if !st.held && l.onHeld != nil {
				l.onHeld()
			}
			st.held = true
			l.decide(st)
			if conn := l.attachToHolder(); conn != nil {
				return conn, nil
			}
		}
		return nil, nil
	default:
		return nil, err
	}
}

// attachToHolder dials the socket the current holder of the authority recorded,
// if it recorded one worth dialling. The authority is account-wide, so that
// socket is the only way a client whose own environment resolved elsewhere
// reaches the daemon that exists; an unreadable or untrustworthy record leaves
// the caller redialling its own sockets until its deadline.
//
// It runs whenever a launch lock was found held, the lifetime lock included. A
// record that passes attachSocket's checks names a live socket of ours: the
// recorded pid is alive, the recorded path is absolute, that path IS a unix
// socket, and this user owns it. There is no directory gate on the socket — the
// held account lock is the trust anchor, not the root the path sits under — so
// the worst case is attaching to a daemon that is not the one holding this
// runtime directory's lock: still the account's one daemon, which is the daemon
// this client wants.
func (l daemonLauncher) attachToHolder() net.Conn {
	if l.dialHeld == nil {
		return nil
	}
	conn, ok := l.dialHeld()
	if !ok || conn == nil {
		return nil
	}
	return conn
}

// launch starts a daemon that inherits locks, the two held locks. The child
// holds them from then on; closing these copies does not unlock. Any failure
// closes them, which releases both locks.
func (l daemonLauncher) launch(ctx context.Context, locks launchLocks, st *launchState) error {
	st.held = false
	if locks.empty() {
		// A launcher with neither lock may not spawn: starting a daemon beside
		// one that already exists is the defect the locks exist to prevent.
		return errNoLaunchLocks
	}
	if err := ctx.Err(); err != nil {
		locks.closeAll()
		return err
	}
	l.publishStarting(locks.Authority)
	if err := l.start(locks); err != nil {
		// The launch is over and there is no child: leave the record empty rather
		// than naming a process that is still alive but about to stop waiting.
		_ = locks.Authority.Truncate(0)
		locks.closeAll()
		return fmt.Errorf("starting daemon: %w", err)
	}
	locks.closeAll()
	st.launched = true
	l.decide(st)
	return nil
}

// publishStarting records, through the held authority fd, that a launch is in
// progress and which socket it is about to serve.
//
// The daemon publishes its own record once it LISTENS, and the gap in between —
// child started, config loading, socket not bound — is exactly when a client
// most needs to know a daemon is coming rather than absent. `plumb serve
// --attach-only` reads this record to decide whether to wait or to fail at once,
// and a launcher must not answer that question by taking the lock itself (a
// momentary hold makes every concurrent launcher wait out its whole deadline).
// The daemon overwrites the record with its own pid and version when it listens.
//
// A failure is a warning: the record is an optimisation for other clients, and
// the launch it describes proceeds either way.
func (l daemonLauncher) publishStarting(authority *os.File) {
	if authority == nil || l.socket == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		exe = "" // best effort: the record still names the pid and the socket
	}
	if err := publishAuthority(authority, authorityMetadata{
		PID:        os.Getpid(),
		Version:    Version,
		Executable: exe,
		Socket:     l.socket,
	}); err != nil {
		slog.Warn("launcher: could not record that a daemon is starting", "err", err)
	}
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
