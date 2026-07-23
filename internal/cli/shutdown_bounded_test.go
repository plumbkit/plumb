package cli

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"
)

// These tests pin the bounded-teardown work for PLAN-17: the topology-indexer
// and LSP-supervisor stops on the orderly shutdown path are wrapped in
// waitWithTimeout so a wedged component is abandoned at its budget instead of
// blocking until the shutdown watchdog forces a hard exit. They must NOT run in
// parallel — they mutate slog.Default() to capture the abandonment warn.

// shutdownWarnLog records the step names of abandonment warns emitted by
// waitWithTimeout. It counts only records that carry a "step" attribute, so
// unrelated daemon/supervisor warns (file-watcher-unavailable, etc.) never
// register as a spurious abandonment.
type shutdownWarnLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *shutdownWarnLog) record(step string) {
	l.mu.Lock()
	l.steps = append(l.steps, step)
	l.mu.Unlock()
}

func (l *shutdownWarnLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.steps)
}

func (l *shutdownWarnLog) has(step string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Contains(l.steps, step)
}

func (l *shutdownWarnLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.steps...)
}

// captureShutdownWarns installs a slog handler that records every abandonment
// warn (a warn carrying a "step" attribute) for the duration of the test.
func captureShutdownWarns(t *testing.T) *shutdownWarnLog {
	t.Helper()
	log := &shutdownWarnLog{}
	h := &captureHandler{fn: func(_ string, attrs map[string]string) {
		if step, ok := attrs["step"]; ok && step != "" {
			log.record(step)
		}
	}}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return log
}

// TestWaitWithTimeout is the core of the bounded-teardown primitive: a component
// whose stop completes promptly returns true and logs no warn (identical to the
// pre-change unbounded wait), while a component whose stop wedges on a channel is
// abandoned within its budget and logs a warn naming it.
func TestWaitWithTimeout(t *testing.T) {
	t.Run("prompt stop returns true and logs no warn", func(t *testing.T) {
		warns := captureShutdownWarns(t)
		done := make(chan struct{})
		go func() {
			time.Sleep(5 * time.Millisecond) // a healthy component stops quickly
			close(done)
		}()
		if !waitWithTimeout(done, time.Second, "fast-component") {
			t.Fatal("waitWithTimeout reported a timeout for a component that stopped in time")
		}
		if got := warns.count(); got != 0 {
			t.Errorf("a prompt stop must log no abandonment warn; got %v", warns.snapshot())
		}
	})

	t.Run("wedged component is abandoned within budget and logs a warn", func(t *testing.T) {
		warns := captureShutdownWarns(t)
		// block models a component whose Stop() blocks on a channel — done only
		// closes once the wedge is released, which never happens within the budget.
		block := make(chan struct{})
		done := make(chan struct{})
		go func() {
			<-block
			close(done)
		}()

		const budget = 60 * time.Millisecond
		start := time.Now()
		if waitWithTimeout(done, budget, "wedged-component") {
			t.Fatal("waitWithTimeout reported success for a component that never stopped")
		}
		elapsed := time.Since(start)

		// Abandoned at (not before, not far past) its budget. Generous margins so
		// a loaded CI scheduler cannot flake this.
		if elapsed < budget/2 {
			t.Errorf("abandoned suspiciously early (%s) for a %s budget", elapsed, budget)
		}
		if elapsed > budget+2*time.Second {
			t.Errorf("took %s to abandon a wedged component; the wait was effectively unbounded", elapsed)
		}
		if !warns.has("wedged-component") {
			t.Errorf("a wedged stop must log a warn naming the component; steps=%v", warns.snapshot())
		}

		close(block) // release the fake so its goroutine does not leak
		<-done
	})
}

// TestTopologyPoolStopAll_PromptNoWarn verifies StopAll of a healthy store
// finishes well under its budget, abandons nothing (no warn), and clears the
// pool — the identical-behaviour half of the bound.
func TestTopologyPoolStopAll_PromptNoWarn(t *testing.T) {
	dir := t.TempDir()
	p := newTopologyPool(enabledTopologyConfig())
	if s := p.Acquire(dir, enabledTopologyConfig()); s == nil {
		t.Fatal("expected a store from an enabled pool")
	}

	warns := captureShutdownWarns(t) // install AFTER Acquire so store-open warns are ignored
	start := time.Now()
	p.StopAll()
	elapsed := time.Since(start)

	if elapsed >= topoStopAllGrace {
		t.Errorf("StopAll of a healthy store took %s, want well under topoStopAllGrace (%s)", elapsed, topoStopAllGrace)
	}
	if got := warns.count(); got != 0 {
		t.Errorf("a healthy StopAll must abandon nothing; got %v", warns.snapshot())
	}
	if got := p.storeCount(); got != 0 {
		t.Errorf("store count = %d, want 0 after StopAll", got)
	}
}

// TestWorkspacePoolClose_PromptNoWarn verifies pool.close of a healthy (warming)
// entry — a real supervisor owning a live child process — finishes well under
// poolCloseGrace+supStopGrace and abandons nothing. sup.Stop() cancels the
// supervisor context, which kills the child and unblocks the loop goroutine, so
// the bounded wait returns promptly with no warn.
func TestWorkspacePoolClose_PromptNoWarn(t *testing.T) {
	cmd, args := sleepCommand(t)
	pool := warmingPool(context.Background(), cmd, args)
	pool.startGrace = 20 * time.Millisecond // return the warming entry fast; we only need it created

	root := t.TempDir()
	if _, err := pool.acquireLang(context.Background(), root, "go", false); err != nil {
		t.Fatalf("acquireLang: %v", err)
	}
	if pool.lookup(root, "go") == nil {
		t.Fatal("expected a warming entry after acquireLang")
	}

	warns := captureShutdownWarns(t) // install AFTER acquire so watcher/spawn warns are ignored
	start := time.Now()
	pool.close()
	elapsed := time.Since(start)

	if elapsed >= poolCloseGrace+supStopGrace {
		t.Errorf("close of a healthy warming entry took %s, want well under poolCloseGrace+supStopGrace (%s)", elapsed, poolCloseGrace+supStopGrace)
	}
	if got := warns.count(); got != 0 {
		t.Errorf("a healthy pool close must abandon nothing; got %v", warns.snapshot())
	}
}
