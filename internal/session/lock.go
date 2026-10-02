package session

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

// sessionDirMu holds one in-process mutex per session directory (keyed by the
// cleaned path), taken by withSessionDirLock BEFORE it opens the lock file.
//
// Without it every pending writer opened its own descriptor on .sessions.lock
// and blocked in flock, so a slow holder in another process (a stuck Stop hook,
// a hung CLI) left the daemon with one goroutine AND one open file per queued
// write — hundreds under contention (#583), unbounded and heading for the fd
// limit. None of them could progress before the holder released anyway. With
// the mutex at most one descriptor per directory waits on flock and the rest
// wait in memory; the flock still serialises against other processes.
var sessionDirMu sync.Map // string -> *sync.Mutex

func sessionDirMutex(dir string) *sync.Mutex {
	key := filepath.Clean(dir)
	if mu, ok := sessionDirMu.Load(key); ok {
		return mu.(*sync.Mutex)
	}
	mu, _ := sessionDirMu.LoadOrStore(key, new(sync.Mutex))
	return mu.(*sync.Mutex)
}

// withSessionDirLock runs fn holding the session directory's in-process mutex
// and then its exclusive .sessions.lock flock. It is not reentrant: see
// listLocked.
func withSessionDirLock(dir string, fn func() error) error {
	mu := sessionDirMutex(dir)
	mu.Lock()
	defer mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".sessions.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // the fd is closed on return either way, which releases the lock
	return fn()
}
