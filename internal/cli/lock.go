package cli

import (
	"context"
	"encoding/json"
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
// starts another. Those are the numbers THIS binary's launcher writes; the
// daemon adopts the fd each env var NAMES instead, because the launcher that
// started it may be an older binary whose handover was not this pair — see
// holdDaemonLock. A daemon started any other way acquires them non-blocking, in
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
//
// They are what this binary's launcher writes, not a requirement on the daemon
// that reads them: the fd itself is taken from the env var, and may be any
// handover fd — see holdDaemonLock for why #649's single-lock handover must
// still start a daemon.
const (
	// authorityLockFDEnv tells `plumb daemon` that the account authority is
	// already held, on the fd the variable names (this launcher writes
	// inheritedAuthorityLockFD, exec.Cmd.ExtraFiles[0]).
	authorityLockFDEnv       = "PLUMB_DAEMON_AUTHORITY_FD"
	inheritedAuthorityLockFD = 3
	// daemonLockFDEnv tells `plumb daemon` that its runtime lifetime lock is
	// already held, on the fd the variable names (this launcher writes
	// inheritedDaemonLockFD, exec.Cmd.ExtraFiles[1]).
	daemonLockFDEnv       = "PLUMB_DAEMON_LOCK_FD"
	inheritedDaemonLockFD = 4
	// minHandoverFD is the lowest fd a handover can name: 0, 1 and 2 are the
	// child's own stdio, and exec.Cmd.ExtraFiles starts at 3, so a value below
	// this names no lock at all and must not be adopted.
	minHandoverFD = 3
)

// spawnLockPath returns the path of the lock used to serialise daemon spawns.
func spawnLockPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.spawn.lock")
}

// daemonLockPath returns the path of the runtime lifetime lock the daemon holds
// for its lifetime, and that every launcher takes before starting one.
func daemonLockPath() string {
	return currentLocation().Lock
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

// daemonLocks are the flocks a daemon holds for its whole life: the account
// authority, then the runtime lifetime lock (see the contract above), then every
// LEGACY lifetime lock the launcher fenced (PLAN-530). The first two are non-nil
// in a daemon that got as far as serving.
type daemonLocks struct {
	Authority *os.File
	Lifetime  *os.File
	// Legacy are the lifetime locks of the runtime locations an older binary
	// could be serving from. Holding them is what makes an old launcher lose at
	// every location it knows about, so no instant exists in which it can start a
	// legacy successor beside this daemon.
	Legacy []heldLegacyLock
}

// closeAll releases every lock the daemon holds. runDaemon defers it, and the
// failure paths of daemonLaunchLocks call it so a daemon that cannot take a later
// lock does not sit on the earlier ones.
func (l daemonLocks) closeAll() {
	for _, f := range []*os.File{l.Authority, l.Lifetime} {
		if f != nil {
			_ = f.Close()
		}
	}
	closeHeldLegacyLocks(l.Legacy)
}

// lockHandover describes one of those locks and how this process comes to hold
// it: the env var a launcher writes when it hands the held fd over, the file the
// lock must be, and how to take it when nothing was handed over. The fd number
// is the one the variable carries, deliberately not a field here: no handover
// this binary did not make may be refused for using a different number.
type lockHandover struct {
	env  string
	path string
	// other is the path of the daemon's OTHER lock. It is used only to make a
	// mismatch error name the file an inherited fd actually is when a launcher
	// has swapped the two: a descriptor carries no portable path of its own, so
	// the other lock is the one name the OS can confirm.
	other   string
	acquire func() (*os.File, error)
}

// legacyLocksEnv names the LEGACY lifetime locks a launcher handed its daemon, as
// JSON: [{"fd":5,"path":"/…/plumb.daemon.lock"}, …] (PLAN-530).
//
// The paths travel with the fds because the daemon cannot recompute them: a
// canonical daemon is launched with the SERVICE PROFILE's environment, so the
// runtime locations its launcher fenced — the client's own — are not the ones its
// own environment resolves. Each fd is adopted against the path beside it
// (adoptDaemonLock), so a handover that names the wrong file fails closed instead
// of fencing a directory nobody is in.
const legacyLocksEnv = "PLUMB_DAEMON_LEGACY_LOCKS"

// inheritedLegacyLockFDBase is the child's first legacy-lock fd: 3 is the
// authority, 4 the runtime lifetime lock, and the legacy locks follow in the
// order the launcher resolved them — exec.Cmd.ExtraFiles order.
const inheritedLegacyLockFDBase = 5

// handedLegacyLock is one entry of that handover.
type handedLegacyLock struct {
	FD   int    `json:"fd"`
	Path string `json:"path"`
}

// heldLegacyLock is a legacy lifetime lock this process holds, with the path an
// older launcher would open to contend for it.
type heldLegacyLock struct {
	Path string
	File *os.File
}

// acquireLegacyLocks takes the lifetime lock of every known legacy runtime
// location without blocking, in a fixed order, and returns them all — the FENCE
// of PLAN-530, and the reason the handover has no gap.
//
// A lock that is HELD is a legacyLockHeldError: an older launcher's daemon owns
// that location, so the caller must start nothing and say so. Any other failure —
// a directory that cannot be created, a lock file that cannot be opened — is
// returned as itself: a launcher that cannot establish the fence does not launch,
// because the alternative is a duplicate daemon. The distinction matters: one is
// a fact about another process, the other a fact about this one.
//
// This CREATES the lock file when it is absent, which is the one place in the
// legacy path that may: a launcher has to own the file before its child exists,
// and a fence that only works once somebody else has made the file is no fence —
// the old launcher would create it and win. Everything that only LOOKS
// (probeLegacyDaemons, the census) opens nothing.
func acquireLegacyLocks() ([]heldLegacyLock, error) {
	var out []heldLegacyLock
	for _, loc := range legacyLocations() {
		held, err := acquireLegacyLock(loc.Lock)
		if err != nil {
			closeHeldLegacyLocks(out)
			return nil, err
		}
		out = append(out, held)
	}
	return out, nil
}

// acquireLegacyLock takes the lifetime lock at path, non-blocking.
func acquireLegacyLock(path string) (heldLegacyLock, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return heldLegacyLock{}, fmt.Errorf("creating the legacy runtime directory %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return heldLegacyLock{}, fmt.Errorf("opening the legacy lifetime lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return heldLegacyLock{}, legacyLockHeldError{lock: path}
		}
		return heldLegacyLock{}, fmt.Errorf("acquiring the legacy lifetime lock %s: %w", path, err)
	}
	return heldLegacyLock{Path: path, File: f}, nil
}

// holdLegacyLocks returns the legacy lifetime locks a daemon holds: the ones its
// launcher handed over, or — for a `plumb daemon` started any other way (a
// service manager, a container CMD) — every one its own environment can name.
//
// A handed-over fd is adopted exactly like the two launch locks: same device and
// inode as the path it claims, flock re-checked, close-on-exec, and a mismatch
// FAILS CLOSED rather than falling back to a fresh acquisition. Falling back
// would take a second lock at the expected path while the launcher still holds
// the first, which is exactly the fence it is supposed to be.
func holdLegacyLocks() ([]heldLegacyLock, error) {
	v, ok := os.LookupEnv(legacyLocksEnv)
	if !ok {
		return acquireLegacyLocks()
	}
	// The daemon's own children (language servers, git) must not see it.
	_ = os.Unsetenv(legacyLocksEnv)
	var handed []handedLegacyLock
	if err := json.Unmarshal([]byte(v), &handed); err != nil {
		return nil, fmt.Errorf("%s: refusing to start: the legacy lock handover cannot be read: %w", legacyLocksEnv, err)
	}
	out := make([]heldLegacyLock, 0, len(handed))
	for _, h := range handed {
		if h.FD < minHandoverFD || h.Path == "" {
			closeHeldLegacyLocks(out)
			return nil, fmt.Errorf("%s: refusing to start: %q names no legacy lock to adopt (fd %d, path %q)",
				legacyLocksEnv, v, h.FD, h.Path)
		}
		f, err := adoptDaemonLock(os.NewFile(uintptr(h.FD), h.Path), h.Path, h.FD, "")
		if err != nil {
			closeHeldLegacyLocks(out)
			return nil, err
		}
		out = append(out, heldLegacyLock{Path: h.Path, File: f})
	}

	// The handed set is the LAUNCHER's, and in canonical mode the launcher is the
	// client while this daemon runs with the service profile's environment, so the
	// two name different runtime locations. Holding only the handed ones would
	// leave THIS daemon's own legacy location free — and that is where an ordinary
	// pre-527 proxy dials and respawns: an old binary knows nothing of the account
	// authority, so the legacy lifetime lock is the only thing that stops it
	// starting a successor beside this daemon. Hold the union, which is what the
	// probe (refuseBesideLegacyDaemon) and the census already reason about.
	for _, loc := range legacyLocations() {
		if heldLegacyLockAt(out, loc.Lock) {
			continue
		}
		held, err := acquireLegacyLock(loc.Lock)
		if err != nil {
			closeHeldLegacyLocks(out)
			return nil, err
		}
		out = append(out, held)
	}
	return out, nil
}

// heldLegacyLockAt reports whether locks already covers path. Two names for one
// file — a symlinked /tmp on darwin, a relative spelling — must not be acquired
// twice: flock is per open file description, so a second acquisition on the same
// file returns EWOULDBLOCK and the daemon would refuse to start against its own
// fence.
func heldLegacyLockAt(locks []heldLegacyLock, path string) bool {
	for _, l := range locks {
		if l.Path == path || filepath.Clean(l.Path) == filepath.Clean(path) {
			return true
		}
		a, err := os.Stat(l.Path)
		if err != nil {
			continue
		}
		if b, err := os.Stat(path); err == nil && os.SameFile(a, b) {
			return true
		}
	}
	return false
}

// encodeLegacyHandover renders the held legacy locks as the JSON handover a
// daemon reads, numbering them from inheritedLegacyLockFDBase in ExtraFiles order.
func encodeLegacyHandover(locks []heldLegacyLock) (string, error) {
	out := make([]handedLegacyLock, 0, len(locks))
	for i, l := range locks {
		out = append(out, handedLegacyLock{FD: inheritedLegacyLockFDBase + i, Path: l.Path})
	}
	data, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encoding the legacy lock handover: %w", err)
	}
	return string(data), nil
}

// closeHeldLegacyLocks releases locks this process holds. It is what a failure
// path calls so a launcher that cannot complete the fence does not sit on half of
// it.
func closeHeldLegacyLocks(locks []heldLegacyLock) {
	for _, l := range locks {
		if l.File != nil {
			_ = l.File.Close()
		}
	}
}

// holdDaemonLock returns the lock h describes: the file inherited on the fd
// h.env names when a launcher named one, else a fresh non-blocking acquisition.
//
// The fd NUMBER comes from the environment rather than being required to match
// the one this binary's launcher writes. #649's `plumb serve` had a single lock
// to hand over — the runtime lifetime lock — and wrote PLUMB_DAEMON_LOCK_FD=3
// for it. That binary is still the serve a client proxies through on an upgraded
// host: the client keeps it until it restarts, and when the daemon is replaced it
// respawns os.Executable() — the NEW binary — with the same environment, naming
// no authority at all. A daemon that required the 3/4 pair would refuse to start
// there, and every pre-upgrade client would lose its daemon for good.
//
// Accepting the named fd costs nothing: the fd is evidence only once
// adoptDaemonLock has confirmed it, with os.SameFile against the expected path,
// and then the flock. A number that names some other file — the other lock of the
// pair included, which otherLockNote reports by name — still refuses.
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
	if err != nil || fd < minHandoverFD {
		return nil, fmt.Errorf("%s=%q: refusing to start: it names no fd for the inherited lock %s "+
			"(a handover is fd %d or above)", h.env, v, h.path, minHandoverFD)
	}
	return adoptDaemonLock(os.NewFile(uintptr(fd), h.path), h.path, fd, h.other)
}

// daemonLaunchLocks is how a daemon comes to hold every lock: each inherited
// from the launcher that started it, or — for a `plumb daemon` started any other
// way (a service manager, a container CMD) — acquired here non-blocking. They are
// taken in the one order, authority first, the legacy fence last.
//
// Losing a later lock releases the earlier ones before the error is returned: a
// daemon that is exiting over a held lifetime lock must not sit on the account's
// authority while it does, and one exiting over a held legacy lock must not sit
// on either of the first two.
func daemonLaunchLocks() (daemonLocks, error) {
	authorityPath, err := authorityPath()
	if err != nil {
		return daemonLocks{}, err
	}
	authority, err := holdDaemonLock(lockHandover{
		env:     authorityLockFDEnv,
		path:    authorityPath,
		other:   daemonLockPath(),
		acquire: acquireAuthority,
	})
	if err != nil {
		return daemonLocks{}, err
	}
	lifetime, err := holdDaemonLock(lockHandover{
		env:     daemonLockFDEnv,
		path:    daemonLockPath(),
		other:   authorityPath,
		acquire: acquireDaemonLock,
	})
	if err != nil {
		_ = authority.Close()
		return daemonLocks{}, err
	}
	locks := daemonLocks{Authority: authority, Lifetime: lifetime}
	legacy, err := holdLegacyLocks()
	if err != nil {
		locks.closeAll()
		return daemonLocks{}, err
	}
	locks.Legacy = legacy
	return locks, nil
}

// daemonStartLocks is how runDaemon comes to hold every lock it needs, and the
// point at which it refuses to serve beside a daemon an older binary left
// running (PLAN-530).
//
// The locks fence the LAUNCH side: while this process holds them no old launcher
// can start a successor. They say nothing about a daemon that is already serving
// at a legacy runtime location, which is answered here, before a socket is bound
// or any state is opened — a second daemon is the defect whatever the two builds
// are. A refusal releases everything taken, so a daemon that exits over it does
// not sit on the account's authority while it does.
func daemonStartLocks() (daemonLocks, error) {
	locks, err := daemonLaunchLocks()
	if err != nil {
		return daemonLocks{}, err
	}
	if err := refuseBesideLegacyDaemon(locks.Legacy); err != nil {
		locks.closeAll()
		return daemonLocks{}, err
	}
	return locks, nil
}

// isAlreadyRunning reports whether err means another plumb daemon is already
// there, so this one should exit quietly rather than treat it as a failure: a
// launch lock it must hold is held, or a daemon of an older binary owns a legacy
// runtime location.
func isAlreadyRunning(err error) bool {
	return errors.Is(err, errDaemonAlreadyRunning) || isLegacyLockHeld(err)
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
