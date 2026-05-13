package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Concurrency contract
//
// Two distinct advisory file locks serialise plumb processes:
//
//   plumb.spawn.lock   — held briefly by `plumb serve` around the dial-or-spawn
//                        decision. Without it, two serves can both observe
//                        "no daemon" and each spawn one. flock serialises them
//                        so the second one re-dials after the first has spawned
//                        and finds the daemon already running.
//
//   plumb.daemon.lock  — held by `plumb daemon` for the lifetime of the process.
//                        Acquired non-blocking; if a second daemon tries to
//                        start (manual invocation, missed serve-side flock,
//                        whatever) it exits immediately rather than stealing
//                        the socket path from the existing daemon.
//
// Both locks are released automatically when the holding fd closes — including
// on process crash — so there is no stale-lock cleanup problem.

// spawnLockPath returns the path of the lock used to serialise daemon spawns.
func spawnLockPath() string {
	return filepath.Join(plumbRuntimeDir(), "plumb.spawn.lock")
}

// daemonLockPath returns the path of the lock the daemon holds for its lifetime.
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

// acquireDaemonLock takes a non-blocking exclusive flock on daemonLockPath.
// Returns errDaemonAlreadyRunning if another daemon already holds it.
//
// The returned *os.File must be held for the lifetime of the daemon process.
// Releasing the lock (closing the file) while the daemon is still serving
// would defeat the singleton guarantee.
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

// errDaemonAlreadyRunning is returned by acquireDaemonLock when another daemon
// already holds the lifetime lock. Sentinel so callers can distinguish from
// genuine open/lock errors.
var errDaemonAlreadyRunning = errors.New("another plumb daemon is already running")
