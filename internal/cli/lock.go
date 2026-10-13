package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Concurrency contract
//
// Three distinct advisory file locks serialise plumb processes. Two of them are
// the daemon's, and it holds both for its whole life:
//
//   plumb.spawn.lock      — held briefly by `plumb serve` around the
//                           dial-or-spawn decision. Without it, two serves can
//                           both observe "no daemon" and each spawn one. flock
//                           serialises them so the second one re-dials after
//                           the first has spawned and finds the daemon already
//                           running.
//
//   daemon.authority.lock — the ACCOUNT authority (PLAN-527), under the
//                           account's data directory. It is outside the runtime
//                           subtree and derived from the OS account database
//                           rather than the environment, so a client whose HOME
//                           or XDG resolves elsewhere still contends for this
//                           one file — which the runtime lock alone could not
//                           do, and how every fake-HOME client became a legal
//                           second daemon.
//
//   plumb.daemon.lock     — the RUNTIME lifetime lock. It stays where it always
//                           was, because an older binary's launcher knows only
//                           this file: it is what makes a pre-527 daemon lose to
//                           the daemon that holds the authority.
//
// Both daemon locks are also the right to launch one: `plumb serve` and
// `plumb restart` take them BEFORE starting a daemon and hand them over on fds 3
// (authority) and 4 (lifetime) — see daemon_launch.go — so they are held from
// the moment the daemon exists, and a launcher that finds either held never
// starts another. A daemon started any other way acquires them non-blocking, in
// the same order, and exits at once if another daemon holds either.
//
// ONE ORDER: the account authority first, then the runtime lifetime lock, in
// every process that takes both (daemonLaunchLocks and acquireLaunchLocks). A
// launcher that loses the second lock releases the first before returning, so
// nothing ever holds one of the pair while it waits on the other.
//
// Every lock is released automatically when the holding fd closes — including on
// process crash — so there is no stale-lock cleanup problem.

// The fds a launcher hands its daemon, and the env vars that name them. The
// numbers are the child's own fd numbers, so they are positional in the
// ExtraFiles that startDaemonProcess builds: authority 3, lifetime 4.
const (
	// authorityLockFDEnv tells `plumb daemon` that the account authority is
	// already held, on fd inheritedAuthorityLockFD (exec.Cmd.ExtraFiles[0]).
	authorityLockFDEnv       = "PLUMB_DAEMON_AUTHORITY_FD"
	inheritedAuthorityLockFD = 3
	// daemonLockFDEnv tells `plumb daemon` that its runtime lifetime lock is
	// already held, on fd inheritedDaemonLockFD (exec.Cmd.ExtraFiles[1]).
	daemonLockFDEnv       = "PLUMB_DAEMON_LOCK_FD"
	inheritedDaemonLockFD = 4
)

// spawnLockPath returns the path of the lock used to serialise daemon spawns.
func spawnLockPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.spawn.lock")
}

// daemonLockPath returns the path of the runtime lifetime lock the daemon holds
// for its lifetime, and that every launcher takes before starting one.
func daemonLockPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.daemon.lock")
}

// acquireSpawnLock takes an exclusive flock on spawnLockPath, blocking until
// the lock is acquired or ctx is cancelled. The caller must Close the returned
// file to release the lock.
//
// Implemented as a non-blocking try-in-loop so context cancellation is honoured
// promptly — syscall.Flock in blocking mode would otherwise wedge the goroutine
// past Ctrl-C.
func acquireSpawnLock(ctx context.Context) (*os.File, error) {
	path := spawnLockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening spawn lock %s: %w", path, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("acquiring spawn lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// acquireDaemonLock takes a non-blocking exclusive flock on daemonLockPath, the
// runtime lifetime lock. Returns errDaemonAlreadyRunning if another daemon
// already holds it.
//
// The returned *os.File must be held for the lifetime of the daemon process, or
// until the child that inherited it takes over. Releasing the lock (closing the
// file) while the daemon is still serving would defeat the singleton guarantee.
func acquireDaemonLock() (*os.File, error) {
	path := daemonLockPath()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening daemon lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errDaemonAlreadyRunning
		}
		return nil, fmt.Errorf("acquiring daemon lock: %w", err)
	}
	return f, nil
}

// errDaemonAlreadyRunning is returned by acquireDaemonLock (and by
// acquireAuthority) when another daemon already holds the lock. Sentinel so
// callers can distinguish from genuine open/lock errors.
var errDaemonAlreadyRunning = errors.New("another plumb daemon is already running")

// daemonLocks are the two flocks a daemon holds for its whole life: the account
// authority, then the runtime lifetime lock (see the contract above). Both are
// non-nil in a daemon that got as far as serving.
type daemonLocks struct {
	Authority *os.File
	Lifetime  *os.File
}

// closeAll releases both locks. runDaemon defers it, and the failure paths of
// daemonLaunchLocks call it so a daemon that cannot take the second lock does
// not sit on the first.
func (l daemonLocks) closeAll() {
	for _, f := range []*os.File{l.Authority, l.Lifetime} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// lockHandover describes one of those locks and how this process comes to hold
// it: the env var a launcher writes when it hands the held fd over, the fd
// number that handover uses, the file the lock must be, and how to take it when
// nothing was handed over.
type lockHandover struct {
	env  string
	fd   int
	path string
	// other is the path of the daemon's OTHER lock. It is used only to make a
	// mismatch error name the file an inherited fd actually is when a launcher
	// has swapped the two: a descriptor carries no portable path of its own, so
	// the other lock is the one name the OS can confirm.
	other   string
	acquire func() (*os.File, error)
}

// holdDaemonLock returns the lock h describes: the file inherited on h.fd when a
// launcher named one, else a fresh non-blocking acquisition.
//
// A named-but-unusable fd FAILS CLOSED. It never falls back to acquiring: a
// substituted fd must not become a second lock at the expected path, and a
// mismatch means this process was not handed the lock it believes it holds.
func holdDaemonLock(h lockHandover) (*os.File, error) {
	v, ok := os.LookupEnv(h.env)
	if !ok {
		return h.acquire()
	}
	// Children of the daemon (language servers, git) must not see it.
	_ = os.Unsetenv(h.env)
	fd, err := strconv.Atoi(v)
	if err != nil || fd != h.fd {
		return nil, fmt.Errorf("%s=%q: refusing to start without the lock it names on fd %d (%s)", h.env, v, h.fd, h.path)
	}
	return adoptDaemonLock(os.NewFile(uintptr(fd), h.path), h.path, h.fd, h.other)
}

// daemonLaunchLocks is how a daemon comes to hold both locks: each inherited
// from the launcher that started it, or — for a `plumb daemon` started any other
// way (a service manager, a container CMD) — acquired here non-blocking. They
// are taken in the one order, authority first.
//
// Losing the lifetime lock releases the authority before the error is returned:
// a daemon that is exiting over a held lifetime lock must not sit on the
// account's authority while it does.
func daemonLaunchLocks() (daemonLocks, error) {
	authorityPath, err := authorityPath()
	if err != nil {
		return daemonLocks{}, err
	}
	authority, err := holdDaemonLock(lockHandover{
		env:     authorityLockFDEnv,
		fd:      inheritedAuthorityLockFD,
		path:    authorityPath,
		other:   daemonLockPath(),
		acquire: acquireAuthority,
	})
	if err != nil {
		return daemonLocks{}, err
	}
	lifetime, err := holdDaemonLock(lockHandover{
		env:     daemonLockFDEnv,
		fd:      inheritedDaemonLockFD,
		path:    daemonLockPath(),
		other:   authorityPath,
		acquire: acquireDaemonLock,
	})
	if err != nil {
		_ = authority.Close()
		return daemonLocks{}, err
	}
	return daemonLocks{Authority: authority, Lifetime: lifetime}, nil
}

// adoptDaemonLock takes over an inherited lock. It must be the lock file at path
// — the same device and inode, because a path spelled the same way is not
// evidence about a file — and no other description may hold it: if one does, it
// fails closed rather than falling back to a fresh acquisition, which could race
// a daemon that really holds it. (A free lock is simply taken.)
//
// Nothing may exec between process start and the CloseOnExec below: the pre-run
// only reads files. The fd is made close-on-exec so the daemon's own children
// cannot keep the lock after it dies.
func adoptDaemonLock(f *os.File, path string, fd int, other string) (*os.File, error) {
	if f == nil {
		return nil, fmt.Errorf("inherited lock: no fd %d", fd)
	}
	syscall.CloseOnExec(int(f.Fd()))
	got, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("inherited lock on fd %d: %w", fd, err)
	}
	want, err := os.Stat(path)
	if err != nil || !os.SameFile(got, want) {
		_ = f.Close()
		return nil, fmt.Errorf("inherited lock on fd %d is not %s%s: refusing to start", fd, path, otherLockNote(got, other))
	}
	// Re-locking a description that already holds the lock succeeds at once.
	// If another daemon holds it through a different description, this fails.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("inherited lock on fd %d is not held: %w", fd, err)
	}
	return f, nil
}

// otherLockNote names the daemon's other lock when an inherited fd turned out to
// be that file, which is what a launcher that handed the two fds over in the
// wrong order produces — the one mismatch whose other path this process can
// name, because it already knows it. A file from anywhere else is reported by
// the expected path and the fd alone.
func otherLockNote(got os.FileInfo, other string) string {
	if other == "" {
		return ""
	}
	info, err := os.Stat(other)
	if err != nil || !os.SameFile(got, info) {
		return ""
	}
	return " but " + other
}
