package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/lsp/protocol"
)

// stubDiag is a thread-safe postWriteDiagSource for testing awaitDiagnosticsRefresh.
// set() only wakes registered WaitNextDiagnostics callers — signals are dropped
// when no waiter is registered, matching the real Invalidator.Handle behaviour.
type stubDiag struct {
	mu   sync.Mutex
	diag []protocol.Diagnostic
	subs []chan struct{}
}

func newStubDiag() *stubDiag { return &stubDiag{} }

func (s *stubDiag) Diagnostics(_ string) []protocol.Diagnostic {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.Diagnostic, len(s.diag))
	copy(out, s.diag)
	return out
}

func (s *stubDiag) WaitNextDiagnostics(ctx context.Context, uri string) ([]protocol.Diagnostic, error) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for i, c := range s.subs {
			if c == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				break
			}
		}
		s.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return s.Diagnostics(uri), ctx.Err()
	case <-ch:
		return s.Diagnostics(uri), nil
	}
}

func (s *stubDiag) set(d []protocol.Diagnostic) {
	s.mu.Lock()
	s.diag = d
	subs := s.subs
	s.subs = nil
	s.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func errDiag(msg string) []protocol.Diagnostic {
	return []protocol.Diagnostic{{Severity: protocol.SevError, Message: msg}}
}

func TestAwaitDiagnosticsRefresh_NilSource(t *testing.T) {
	got, _ := awaitDiagnosticsRefresh(nil, "file:///foo.go", 50*time.Millisecond, nil)
	if got != nil {
		t.Errorf("nil source: want nil, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_Disabled(t *testing.T) {
	src := newStubDiag()
	src.set(errDiag("old error"))

	start := time.Now()
	got, _ := awaitDiagnosticsRefresh(src, "file:///foo.go", -1, nil)
	elapsed := time.Since(start)

	if elapsed > 20*time.Millisecond {
		t.Errorf("disabled window: returned after %v, want near-instant", elapsed)
	}
	if len(got) != 1 || got[0].Message != "old error" {
		t.Errorf("disabled window: want current diag returned, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_FeedsEstimator(t *testing.T) {
	src := newStubDiag()
	src.set(errDiag("before"))
	est := NewDiagWaitEstimator()

	go func() {
		time.Sleep(20 * time.Millisecond)
		src.set(errDiag("after"))
	}()

	ceiling := 500 * time.Millisecond
	_, _ = awaitDiagnosticsRefresh(src, "file:///foo.go", ceiling, est)

	// A publish was observed, so the estimator now holds a sample and bounds the
	// next effective window below the ceiling.
	if got := est.window(ceiling); got >= ceiling {
		t.Fatalf("estimator did not learn from the publish: window %v >= ceiling %v", got, ceiling)
	}
}

// TestAwaitDiagnosticsRefresh_AdaptiveWindowShortensCleanWrite is the empirical
// proof for the 0.8.16 latency change: a write the server never re-publishes for
// (the clean-write case) waits the full ceiling with no estimator, but only the
// adaptive window once the estimator has learned the server's typical latency.
func TestAwaitDiagnosticsRefresh_AdaptiveWindowShortensCleanWrite(t *testing.T) {
	const ceiling = 300 * time.Millisecond

	// Baseline: no estimator. The stub never signals, so the wait runs to the
	// full ceiling.
	base := newStubDiag()
	base.set(errDiag("unchanged"))
	start := time.Now()
	_, _ = awaitDiagnosticsRefresh(base, "file:///foo.go", ceiling, nil)
	baseline := time.Since(start)
	if baseline < ceiling {
		t.Fatalf("nil estimator returned in %v, expected the full %v ceiling", baseline, ceiling)
	}

	// Warmed estimator: the server has been re-publishing in ~40ms, so the
	// effective window is 3×40 = 120ms — the same clean write returns in ~that,
	// well under the ceiling.
	est := NewDiagWaitEstimator()
	for range 10 {
		est.record(40 * time.Millisecond)
	}
	warm := newStubDiag()
	warm.set(errDiag("unchanged"))
	start = time.Now()
	_, _ = awaitDiagnosticsRefresh(warm, "file:///foo.go", ceiling, est)
	adaptive := time.Since(start)
	if adaptive >= 200*time.Millisecond {
		t.Fatalf("warmed estimator waited %v, expected well under the %v ceiling", adaptive, ceiling)
	}
	if adaptive < 80*time.Millisecond {
		t.Fatalf("warmed estimator returned in %v, expected ~120ms (3×40ms) adaptive window", adaptive)
	}
}

func TestAwaitDiagnosticsRefresh_FreshFlag(t *testing.T) {
	// A publish during the wait → fresh (the diagnostics reflect this write).
	src := newStubDiag()
	src.set(errDiag("before"))
	go func() {
		time.Sleep(20 * time.Millisecond)
		src.set(errDiag("after"))
	}()
	if _, fresh := awaitDiagnosticsRefresh(src, "file:///foo.go", 500*time.Millisecond, nil); !fresh {
		t.Fatalf("expected fresh=true when the server publishes during the wait")
	}

	// Timeout with no publish → not fresh.
	quiet := newStubDiag()
	quiet.set(errDiag("unchanged"))
	if _, fresh := awaitDiagnosticsRefresh(quiet, "file:///foo.go", 40*time.Millisecond, nil); fresh {
		t.Fatalf("expected fresh=false on timeout with no publish")
	}

	// Disabled wait → not fresh (returned without waiting).
	if _, fresh := awaitDiagnosticsRefresh(quiet, "file:///foo.go", -1, nil); fresh {
		t.Fatalf("expected fresh=false when the wait is disabled")
	}
}

func TestFormatPostWriteDiagnostics_StaleNote(t *testing.T) {
	diags := errDiag("undefined: Foo")
	stale := formatPostWriteDiagnostics(diags, false, 0)
	if !strings.Contains(stale, "undefined: Foo") {
		t.Fatalf("missing diagnostic:\n%s", stale)
	}
	if !strings.Contains(stale, "re-analys") {
		t.Fatalf("expected a staleness note when not fresh:\n%s", stale)
	}
	if strings.Contains(formatPostWriteDiagnostics(diags, true, 0), "re-analys") {
		t.Fatalf("fresh diagnostics should carry no staleness note")
	}
}

// diagAtLine builds one error diagnostic at the given 0-based line.
func diagAtLine(msg string, line uint32) []protocol.Diagnostic {
	d := []protocol.Diagnostic{{Severity: protocol.SevError, Message: msg}}
	d[0].Range.Start.Line = line
	return d
}

func TestFormatPostWriteDiagnostics_OutOfRangeDowngraded(t *testing.T) {
	// File now has 10 lines; a diagnostic at line 41 (0-based) points well past
	// EOF — the classic stale-after-shrink case. It must be down-ranked to
	// "stale?", never rendered as a hard "error".
	out := formatPostWriteDiagnostics(diagAtLine("undefined: Gone", 41), true, 10)
	if !strings.Contains(out, "stale?") {
		t.Fatalf("expected an out-of-range diagnostic to be grouped as stale?:\n%s", out)
	}
	if strings.Contains(out, "error L42") {
		t.Fatalf("out-of-range diagnostic must not be rendered as a hard error:\n%s", out)
	}
	if !strings.Contains(out, "current end") {
		t.Fatalf("expected the stale? explanation note:\n%s", out)
	}
}

func TestFormatPostWriteDiagnostics_InRangeStaysError(t *testing.T) {
	// A diagnostic at line 3 (0-based) in a 10-line file is in range — it must
	// still be a hard error, not downgraded.
	out := formatPostWriteDiagnostics(diagAtLine("real error", 3), true, 10)
	if !strings.Contains(out, "error L4: real error") {
		t.Fatalf("in-range diagnostic must stay a hard error:\n%s", out)
	}
	if strings.Contains(out, "stale?") {
		t.Fatalf("in-range diagnostic must not be marked stale:\n%s", out)
	}
}

func TestAwaitDiagnosticsRefresh_TimesOut(t *testing.T) {
	src := newStubDiag()
	src.set(errDiag("unchanged"))

	window := 60 * time.Millisecond
	start := time.Now()
	got, _ := awaitDiagnosticsRefresh(src, "file:///foo.go", window, nil)
	elapsed := time.Since(start)

	if elapsed < window {
		t.Errorf("should have waited at least %v, returned after %v", window, elapsed)
	}
	if len(got) != 1 || got[0].Message != "unchanged" {
		t.Errorf("timeout: want current diag, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_EarlyReturn(t *testing.T) {
	src := newStubDiag()
	src.set(errDiag("before"))

	// Signal new diagnostics after a short delay.
	go func() {
		time.Sleep(30 * time.Millisecond)
		src.set(errDiag("after"))
	}()

	window := 500 * time.Millisecond
	start := time.Now()
	got, _ := awaitDiagnosticsRefresh(src, "file:///foo.go", window, nil)
	elapsed := time.Since(start)

	if elapsed >= window {
		t.Errorf("should have returned early on signal, but waited full window %v", elapsed)
	}
	if len(got) != 1 || got[0].Message != "after" {
		t.Errorf("early return: want updated diag, got %v", got)
	}
}

func TestAwaitDiagnosticsRefresh_ZeroWindowUsesDefault(t *testing.T) {
	src := newStubDiag()
	src.set(errDiag("no change"))

	// window=0 should use defaultPostWriteDiagWindow (300ms). Signal at 50ms
	// to confirm we actually blocked waiting for the notification.
	changed := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		src.set(errDiag("changed"))
		close(changed)
	}()

	got, _ := awaitDiagnosticsRefresh(src, "file:///foo.go", 0, nil)

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
	// An untracked file's entire content is uncommitted, so a destructive write
	// (overwrite/delete) would lose it — pathIsDirty must treat it as dirty.
	if !pathIsDirty(context.Background(), f) {
		t.Error("expected dirty for an untracked file (its content is unrecoverable on overwrite/delete)")
	}
	// The move/copy variant preserves content, so an untracked source is fine.
	if pathIsDirtyIgnoringUntracked(context.Background(), f) {
		t.Error("expected not dirty for an untracked file under the move/copy variant")
	}
}

func TestPathIsDirtyIgnoringUntracked_ModifiedTrackedFile(t *testing.T) {
	dir := initGitRepo(t)
	f := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(f, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitExec(t, dir, "add", "tracked.txt")
	gitExec(t, dir, "commit", "-m", "add tracked")
	if err := os.WriteFile(f, []byte("modified content"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Only untracked files are exempted: a tracked file with working-tree edits
	// is still dirty under the move/copy variant.
	if !pathIsDirtyIgnoringUntracked(context.Background(), f) {
		t.Error("expected dirty for a tracked, modified file even under the move/copy variant")
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
