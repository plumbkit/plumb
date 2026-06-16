package tools

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// repoLockEntry is the per-repository serialisation primitive. Unlike the plain
// sync.Mutex behind pathLocks, the lock token lives in a buffered channel of
// capacity one so an acquirer can wait on it with select — honouring context
// cancellation and a bounded timeout rather than blocking unconditionally.
//
// Concurrency: free is created pre-filled with one token; lockRepo receives the
// token to acquire and the release closure sends it back. lastUsedNs is the
// idle-sweep timestamp, read/written atomically.
type repoLockEntry struct {
	free       chan struct{}
	lastUsedNs atomic.Int64
}

// repoLocks serialises index/ref-mutating git operations against the same
// repository across all concurrent connections in this singleton daemon. Two
// sessions sharing one worktree would otherwise run `git commit` concurrently
// and collide on .git/index.lock (git's exclusive per-index mutex); this turns
// that collision into a bounded, ordered wait.
//
// The key is the worktree's resolved top-level (git rev-parse --show-toplevel,
// via findGitRoot) — distinct per linked worktree, so two worktrees committing
// different branches never serialise against each other. Read-tier git ops
// (status/log/diff) are never locked, so a query never queues behind a slow
// commit. Entries are evicted by StartRepoLockSweep after repoLockIdleExpiry.
//
// Scope: this serialises plumb's OWN git operations. An external git process
// (the user's shell, an IDE, CI) is invisible to this lock and can still
// collide — that case is unchanged.
var repoLocks sync.Map // map[string]*repoLockEntry

const (
	repoLockSweepInterval = 5 * time.Minute
	repoLockIdleExpiry    = 1 * time.Hour
	// repoLockMaxWait bounds a queued git op so a wedged holder cannot pin a
	// connection indefinitely when the caller's context carries no deadline. A
	// legitimate holder is another git op (possibly a slow pre-commit hook), so
	// the bound is generous.
	repoLockMaxWait = 2 * time.Minute
)

// lockRepo acquires the per-repository serialisation lock for gitDir, waiting up
// to repoLockMaxWait (or until ctx is done, whichever is sooner). It returns a
// release closure on success, or an error when the wait is cut short — the
// caller surfaces that as "another git operation is in progress" rather than
// wedging. gitDir must be the resolved repository root (see findGitRoot).
func lockRepo(ctx context.Context, gitDir string) (func(), error) {
	now := time.Now().UnixNano()
	fresh := &repoLockEntry{free: make(chan struct{}, 1)}
	fresh.free <- struct{}{} // one token: the lock starts free
	fresh.lastUsedNs.Store(now)
	v, _ := repoLocks.LoadOrStore(gitDir, fresh)
	e := v.(*repoLockEntry)
	// Stamp before blocking so the sweep never evicts an entry being waited on.
	e.lastUsedNs.Store(now)

	timer := time.NewTimer(repoLockMaxWait)
	defer timer.Stop()
	select {
	case <-e.free:
		return func() {
			e.lastUsedNs.Store(time.Now().UnixNano())
			e.free <- struct{}{}
		}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("another git operation is in progress on this repository; %w", ctx.Err())
	case <-timer.C:
		return nil, fmt.Errorf("another git operation is in progress on this repository; timed out after %s waiting for it to finish", repoLockMaxWait)
	}
}

// StartRepoLockSweep launches a background goroutine that evicts idle entries
// from repoLocks every repoLockSweepInterval. Called once from the daemon run
// loop with the daemon's lifetime context (mirrors StartPathLockSweep).
func StartRepoLockSweep(ctx context.Context) {
	go func() {
		t := time.NewTicker(repoLockSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweepRepoLocks(time.Now())
			}
		}
	}()
}

// sweepRepoLocks removes entries idle for longer than repoLockIdleExpiry. It
// drains the free token (the non-blocking equivalent of TryLock) to skip an
// entry that is currently held, and re-checks idleness after acquiring to guard
// the Range→drain race.
func sweepRepoLocks(now time.Time) {
	repoLocks.Range(func(key, value any) bool {
		e := value.(*repoLockEntry)
		if now.Sub(time.Unix(0, e.lastUsedNs.Load())) < repoLockIdleExpiry {
			return true // recently used — keep
		}
		select {
		case <-e.free:
			// Acquired the free token: the lock was not held. Re-check idleness
			// in case it was used between the Range read and this drain.
			if now.Sub(time.Unix(0, e.lastUsedNs.Load())) < repoLockIdleExpiry {
				e.free <- struct{}{}
				return true
			}
			repoLocks.Delete(key)
			// Token intentionally not returned — the entry is gone.
		default:
			// Held by an in-flight git op — skip.
		}
		return true
	})
}
