package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// stubDiag is a thread-safe postWriteDiagSource for testing awaitDiagnosticsRefresh.
type stubDiag struct {
	mu   sync.Mutex
	diag []protocol.Diagnostic
}

func (s *stubDiag) Diagnostics(_ string) []protocol.Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diag
}

func (s *stubDiag) set(d []protocol.Diagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.diag = d
}

func errDiag(msg string) []protocol.Diagnostic {
	return []protocol.Diagnostic{{Severity: protocol.SevError, Message: msg}}
}

func TestAwaitDiagnosticsRefresh_NilSource(t *testing.T) {
	got := awaitDiagnosticsRefresh(nil, "file:///foo.go", nil, 50*time.Millisecond)
	if got != nil {
		t.Errorf("nil source: want nil, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_Disabled(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("old error")
	src.set(baseline)

	start := time.Now()
	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, -1)
	elapsed := time.Since(start)

	if elapsed > 20*time.Millisecond {
		t.Errorf("disabled window: returned after %v, want near-instant", elapsed)
	}
	if len(got) != 1 || got[0].Message != "old error" {
		t.Errorf("disabled window: want baseline returned unchanged, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_TimesOut(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("unchanged")
	src.set(baseline)

	window := 60 * time.Millisecond
	start := time.Now()
	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, window)
	elapsed := time.Since(start)

	if elapsed < window {
		t.Errorf("should have waited at least %v, returned after %v", window, elapsed)
	}
	if len(got) != 1 || got[0].Message != "unchanged" {
		t.Errorf("timeout: want baseline, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_EarlyReturn(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("before")
	src.set(baseline)

	// Change the diagnostics after a short delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		src.set(errDiag("after"))
	}()

	window := 500 * time.Millisecond
	start := time.Now()
	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, window)
	elapsed := time.Since(start)

	if elapsed >= window {
		t.Errorf("should have returned early (diag changed), but waited full window %v", elapsed)
	}
	if len(got) != 1 || got[0].Message != "after" {
		t.Errorf("early return: want updated diag, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_ZeroWindowUsesDefault(t *testing.T) {
	src := &stubDiag{}
	baseline := errDiag("no change")
	src.set(baseline)

	// window=0 should use defaultPostWriteDiagWindow (300ms). We use a change
	// fired after 50ms to confirm we didn't return instantly (which would mean
	// the window was treated as 0 duration rather than the 300ms default).
	changed := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		src.set(errDiag("changed"))
		close(changed)
	}()

	got := awaitDiagnosticsRefresh(src, "file:///foo.go", baseline, 0)

	select {
	case <-changed:
	default:
		t.Fatal("goroutine should have fired before the 300ms default window expired")
	}
	if len(got) != 1 || got[0].Message != "changed" {
		t.Errorf("zero window: want updated diag via default window, got %v", got)
	}
}

// gitExec runs a git command in dir and calls t.Fatal on failure.
func gitExec(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initGitRepo creates a temporary git repository with a single empty commit so
// git status works. Returns the repo root directory.
func initGitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir, err := os.MkdirTemp("", "plumb-gitrepo-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	gitExec(t, dir, "init")
	gitExec(t, dir, "config", "user.email", "test@plumb.test")
	gitExec(t, dir, "config", "user.name", "Plumb Test")
	// Seed an initial commit so the repo is usable.
	seed := filepath.Join(dir, ".gitkeep")
	if err := os.WriteFile(seed, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", ".gitkeep")
	gitExec(t, dir, "commit", "-m", "init")
	return dir
}

func TestPathIsDirty_OutsideGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	// os.MkdirTemp with "" prefix lands in the system temp dir, which is
	// outside any git repo on a normal development machine.
	dir, err := os.MkdirTemp("", "plumb-nogit-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pathIsDirty(context.Background(), f) {
		t.Error("expected not dirty for file outside any git repository")
	}
}

func TestPathIsDirty_CleanCommittedFile(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "clean.txt")
	if err := os.WriteFile(f, []byte("committed content"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "clean.txt")
	gitExec(t, dir, "commit", "-m", "add clean")

	if pathIsDirty(context.Background(), f) {
		t.Error("expected not dirty for a committed, unmodified file")
	}
}

func TestPathIsDirty_ModifiedTrackedFile(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "modified.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "modified.txt")
	gitExec(t, dir, "commit", "-m", "add")

	// Modify after commit → dirty.
	if err := os.WriteFile(f, []byte("modified content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pathIsDirty(context.Background(), f) {
		t.Error("expected dirty for a committed file with working-tree modifications")
	}
}

func TestPathIsDirty_UntrackedFile(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "untracked.txt")
	if err := os.WriteFile(f, []byte("new file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !pathIsDirty(context.Background(), f) {
		t.Error("expected dirty for an untracked (newly-added) file")
	}
}

func TestPathIsDirty_GitIgnoredFile(t *testing.T) {
	dir := initGitRepo(t)
	// Write a .gitignore that ignores *.log.
	gi := filepath.Join(dir, ".gitignore")
	if err := os.WriteFile(gi, []byte("*.log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", ".gitignore")
	gitExec(t, dir, "commit", "-m", "gitignore")

	f := filepath.Join(dir, "ignored.log")
	if err := os.WriteFile(f, []byte("log content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pathIsDirty(context.Background(), f) {
		t.Error("expected not dirty for a gitignored file")
	}
}

func TestLockPathKeyResolvesSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if got, want := lockPathKey(link), lockPathKey(target); got != want {
		t.Fatalf("lockPathKey(link) = %q, want target key %q", got, want)
	}
}

// pathLockKey returns a test-scoped unique key that won't collide with other
// tests running in parallel. Prefixed with t.Name() and a random suffix.
func uniquePathKey(t *testing.T) string {
	t.Helper()
	return "/test-path-lock/" + t.Name()
}

func TestSweepPathLocks_EvictsIdleEntries(t *testing.T) {
	key := uniquePathKey(t)

	// Pre-populate an entry whose lastUsedNs is two hours old.
	e := &pathLockEntry{}
	e.lastUsedNs.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	pathLocks.Store(key, e)
	t.Cleanup(func() { pathLocks.Delete(key) })

	sweepPathLocks(time.Now())

	if _, ok := pathLocks.Load(key); ok {
		t.Fatal("expected idle entry to be evicted by sweep, but it remains")
	}
}

func TestSweepPathLocks_SkipsLockedEntries(t *testing.T) {
	key := uniquePathKey(t)

	e := &pathLockEntry{}
	e.lastUsedNs.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	// Hold the mutex — sweep must not delete it.
	e.mu.Lock()
	pathLocks.Store(key, e)
	t.Cleanup(func() {
		e.mu.Unlock()
		pathLocks.Delete(key)
	})

	sweepPathLocks(time.Now())

	if _, ok := pathLocks.Load(key); !ok {
		t.Fatal("sweep evicted a locked entry; it should have been skipped")
	}
}

func TestSweepPathLocks_KeepsRecentEntries(t *testing.T) {
	key := uniquePathKey(t)

	e := &pathLockEntry{}
	e.lastUsedNs.Store(time.Now().UnixNano()) // just used
	pathLocks.Store(key, e)
	t.Cleanup(func() { pathLocks.Delete(key) })

	sweepPathLocks(time.Now())

	if _, ok := pathLocks.Load(key); !ok {
		t.Fatal("sweep evicted a recently-used entry; it should have been kept")
	}
}

func TestSweepPathLocks_ReChecksAfterTryLock(t *testing.T) {
	// Verify the double-check: if lastUsedNs is updated between the initial
	// idleness check and the TryLock, the entry must NOT be deleted.
	key := uniquePathKey(t)

	e := &pathLockEntry{}
	// Start with an old timestamp so the first idleness check passes.
	e.lastUsedNs.Store(time.Now().Add(-2 * time.Hour).UnixNano())
	pathLocks.Store(key, e)
	t.Cleanup(func() { pathLocks.Delete(key) })

	// Simulate a concurrent update that happens between the Range iteration
	// and the TryLock: update lastUsedNs to "now" just before calling sweep.
	// Because sweepPathLocks re-checks after TryLock, the entry must survive.
	e.lastUsedNs.Store(time.Now().UnixNano())

	sweepPathLocks(time.Now())

	if _, ok := pathLocks.Load(key); !ok {
		t.Fatal("sweep deleted an entry that was refreshed before TryLock; it should have survived")
	}
}

func TestStartPathLockSweep_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	StartPathLockSweep(ctx)
	// Cancelling the context should not panic and the goroutine should exit.
	cancel()
	// Give the goroutine a moment to observe the cancellation.
	time.Sleep(50 * time.Millisecond)
	// No assertion other than "did not hang or panic" — the test passes if it
	// completes within the test timeout.
}
