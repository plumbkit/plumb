package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLockRepo_SerialisesSameRepo(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	release, err := lockRepo(ctx, dir)
	if err != nil {
		t.Fatalf("first lockRepo: %v", err)
	}

	// A second acquire of the same key must block until the first releases.
	got := make(chan struct{})
	go func() {
		rel2, err := lockRepo(ctx, dir)
		if err != nil {
			t.Errorf("second lockRepo: %v", err)
			close(got)
			return
		}
		rel2()
		close(got)
	}()

	select {
	case <-got:
		t.Fatal("second lockRepo acquired while the first still held the lock")
	case <-time.After(50 * time.Millisecond):
		// expected: still blocked
	}

	release()
	select {
	case <-got:
		// expected: the waiter proceeds once released
	case <-time.After(2 * time.Second):
		t.Fatal("second lockRepo did not acquire after release")
	}
}

func TestLockRepo_DistinctReposDoNotSerialise(t *testing.T) {
	ctx := context.Background()
	a, b := t.TempDir(), t.TempDir()

	relA, err := lockRepo(ctx, a)
	if err != nil {
		t.Fatalf("lock a: %v", err)
	}
	defer relA()

	// A different repo key must acquire immediately despite a being held.
	done := make(chan struct{})
	go func() {
		relB, err := lockRepo(ctx, b)
		if err == nil {
			relB()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lock on a distinct repo blocked behind an unrelated repo's lock")
	}
}

func TestLockRepo_ContextCancelledWhileWaiting(t *testing.T) {
	dir := t.TempDir()
	release, err := lockRepo(context.Background(), dir)
	if err != nil {
		t.Fatalf("first lockRepo: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = lockRepo(ctx, dir) // held by the first acquire; this must give up on cancel
	if err == nil {
		t.Fatal("expected an error when the context is cancelled while waiting")
	}
	if !strings.Contains(err.Error(), "another git operation is in progress") {
		t.Errorf("error should explain the in-progress git op, got: %v", err)
	}
}

func TestSweepRepoLocks_EvictsIdleKeepsHeld(t *testing.T) {
	idle := fmt.Sprintf("/tmp/plumb-test-idle-%d", time.Now().UnixNano())
	held := fmt.Sprintf("/tmp/plumb-test-held-%d", time.Now().UnixNano())
	t.Cleanup(func() { repoLocks.Delete(idle); repoLocks.Delete(held) })

	// An idle, free entry older than the expiry is swept.
	idleEntry := &repoLockEntry{free: make(chan struct{}, 1)}
	idleEntry.free <- struct{}{}
	idleEntry.lastUsedNs.Store(time.Now().Add(-2 * repoLockIdleExpiry).UnixNano())
	repoLocks.Store(idle, idleEntry)

	// A held entry (no free token) is never swept, even if its timestamp is old.
	heldEntry := &repoLockEntry{free: make(chan struct{}, 1)} // empty == held
	heldEntry.lastUsedNs.Store(time.Now().Add(-2 * repoLockIdleExpiry).UnixNano())
	repoLocks.Store(held, heldEntry)

	sweepRepoLocks(time.Now())

	if _, ok := repoLocks.Load(idle); ok {
		t.Error("idle free entry should have been swept")
	}
	if _, ok := repoLocks.Load(held); !ok {
		t.Error("held entry must not be swept")
	}
}

// TestGit_ConcurrentCommitsSerialise is the end-to-end guarantee: two sessions
// committing to one shared worktree at once both succeed, serialised, and never
// fail with "index.lock: File exists".
func TestGit_ConcurrentCommitsSerialise(t *testing.T) {
	requireGit(t)
	dir := initTestRepo(t)

	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			fname := fmt.Sprintf("f%d.txt", i)
			if err := os.WriteFile(filepath.Join(dir, fname), []byte(fmt.Sprintf("content %d\n", i)), 0o644); err != nil {
				errs <- err
				return
			}
			tool := NewGit(WriteDeps{WorkspaceFn: func() string { return dir }}, nil)
			if _, err := callGit(t, tool, map[string]any{"subcommand": "add", "files": []string{fname}}); err != nil {
				errs <- fmt.Errorf("add %s: %w", fname, err)
				return
			}
			if _, err := callGit(t, tool, map[string]any{"subcommand": "commit", "message": fmt.Sprintf("commit %d", i), "files": []string{fname}}); err != nil {
				errs <- fmt.Errorf("commit %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent git op failed (collision not serialised?): %v", err)
		}
	}
}
