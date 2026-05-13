package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// withTempRuntime redirects plumbRuntimeDir() at the os.UserCacheDir level so
// the lock files land in a t.TempDir() and don't collide with the user's real
// runtime dir or with other tests running in parallel.
func withTempRuntime(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	// plumbRuntimeDir uses os.UserCacheDir which honours XDG_CACHE_HOME on Linux
	// and HOME on macOS. Setting HOME is the portable way to redirect it.
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(dir, "cache"))
}

func TestAcquireDaemonLock_SecondAttemptFails(t *testing.T) {
	withTempRuntime(t)

	first, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("first acquireDaemonLock: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	_, err = acquireDaemonLock()
	if !errors.Is(err, errDaemonAlreadyRunning) {
		t.Fatalf("second acquireDaemonLock: want errDaemonAlreadyRunning, got %v", err)
	}
}

// TestAcquireDaemonLock_ReleasesOnClose simulates the crash-recovery story:
// closing the fd (which is what the kernel does when a process dies) must
// make the lock immediately reacquirable by another caller.
func TestAcquireDaemonLock_ReleasesOnClose(t *testing.T) {
	withTempRuntime(t)

	first, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("first acquireDaemonLock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing first lock: %v", err)
	}

	second, err := acquireDaemonLock()
	if err != nil {
		t.Fatalf("second acquireDaemonLock after release: %v", err)
	}
	_ = second.Close()
}

// TestAcquireSpawnLock_SerialisesWaiters verifies the serve-side spawn lock
// blocks concurrent callers and serves them one at a time. This is the
// property that prevents the "two daemons spawned simultaneously" bug.
func TestAcquireSpawnLock_SerialisesWaiters(t *testing.T) {
	withTempRuntime(t)

	first, err := acquireSpawnLock(context.Background())
	if err != nil {
		t.Fatalf("first acquireSpawnLock: %v", err)
	}

	got := make(chan time.Time, 1)
	go func() {
		f, err := acquireSpawnLock(context.Background())
		if err != nil {
			t.Errorf("second acquireSpawnLock: %v", err)
			close(got)
			return
		}
		got <- time.Now()
		_ = f.Close()
	}()

	// Give the goroutine time to enter the flock retry loop.
	time.Sleep(150 * time.Millisecond)
	release := time.Now()
	_ = first.Close()

	select {
	case acquiredAt, ok := <-got:
		if !ok {
			t.Fatal("second acquireSpawnLock failed")
		}
		if acquiredAt.Before(release) {
			t.Fatalf("second lock acquired %v before first released — not serialised",
				release.Sub(acquiredAt))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second acquireSpawnLock did not return within 2s after release")
	}
}

func TestAcquireSpawnLock_CancellationHonoured(t *testing.T) {
	withTempRuntime(t)

	holder, err := acquireSpawnLock(context.Background())
	if err != nil {
		t.Fatalf("holding lock: %v", err)
	}
	defer holder.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = acquireSpawnLock(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context.DeadlineExceeded, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("cancellation took %v — should be ~100ms", elapsed)
	}
}

// TestAcquireDaemonLock_ParallelStress runs 20 goroutines all trying to acquire
// the daemon lock. Only one should ever hold it at a time. Regression test
// for the original "two daemons race" bug.
func TestAcquireDaemonLock_ParallelStress(t *testing.T) {
	withTempRuntime(t)

	const N = 20
	var (
		wg          sync.WaitGroup
		mu          sync.Mutex
		holderCount int
		maxHolders  int
		successes   int
	)

	for range N {
		wg.Go(func() {
			f, err := acquireDaemonLock()
			if err != nil {
				if !errors.Is(err, errDaemonAlreadyRunning) {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			mu.Lock()
			holderCount++
			if holderCount > maxHolders {
				maxHolders = holderCount
			}
			successes++
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			mu.Lock()
			holderCount--
			mu.Unlock()
			_ = f.Close()
		})
	}
	wg.Wait()

	if maxHolders > 1 {
		t.Fatalf("up to %d goroutines held the lock simultaneously — should be 1", maxHolders)
	}
	if successes == 0 {
		t.Fatal("no goroutine acquired the lock")
	}
	// We don't assert successes == 1: closing-then-reopening between goroutines
	// can let later attempts succeed. The invariant is that no two hold it at once.
}

// confirm plumbRuntimeDir uses the temp HOME we set.
func TestLockPaths_RespectUserCacheDir(t *testing.T) {
	withTempRuntime(t)
	got := plumbRuntimeDir()
	cache, _ := os.UserCacheDir()
	want := filepath.Join(cache, "plumb")
	if got != want {
		t.Fatalf("plumbRuntimeDir = %q, want %q", got, want)
	}
	if filepath.Dir(spawnLockPath()) != got {
		t.Fatalf("spawnLockPath not under runtime dir: %s", spawnLockPath())
	}
	if filepath.Dir(daemonLockPath()) != got {
		t.Fatalf("daemonLockPath not under runtime dir: %s", daemonLockPath())
	}
}
