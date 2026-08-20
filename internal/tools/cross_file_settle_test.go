package tools

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// settleSourceNoWait is a diagnostics source that cannot signal, so the settle
// grace must fall back to the original fixed sleep.
type settleSourceNoWait struct{}

func (settleSourceNoWait) Diagnostics(string) []protocol.Diagnostic { return nil }

func (settleSourceNoWait) WaitNextDiagnostics(context.Context, string) ([]protocol.Diagnostic, error) {
	return nil, nil
}

// TestWaitForCrossFileSettle_ReturnsEarlyOnPublish is the F6 latency fix: the
// settle grace used to be a flat `<-time.After(200ms)` taken on EVERY edit whose
// file re-published fresh. It is now a ceiling — once a dependent file actually
// re-publishes, the sweep proceeds immediately.
func TestWaitForCrossFileSettle_ReturnsEarlyOnPublish(t *testing.T) {
	inv := cache.NewInvalidator(cache.New(0))

	// Publish REPEATEDLY until the wait returns. A single timed publish would race
	// the subscription inside waitForCrossFileSettle: if the publisher got there
	// first the wake would be missed and the wait would run out its whole ceiling,
	// failing for a reason that has nothing to do with the behaviour under test.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				inv.Handle("textDocument/publishDiagnostics",
					[]byte(`{"uri":"file:///dependent.go","diagnostics":[]}`))
			}
		}
	}()

	const ceiling = 3 * time.Second
	start := time.Now()
	waitForCrossFileSettle(inv, ceiling)
	elapsed := time.Since(start)
	close(stop)
	<-done

	// The point is that it returned ON the publish, far short of the ceiling.
	if elapsed >= ceiling/2 {
		t.Errorf("settle wait took %s against a %s ceiling; it must return as soon as a file republishes", elapsed, ceiling)
	}
}

// TestWaitForCrossFileSettle_HonoursCeiling: with no publish at all, the wait
// must still end at the configured grace. The ceiling and its default are
// unchanged by this fix.
func TestWaitForCrossFileSettle_HonoursCeiling(t *testing.T) {
	inv := cache.NewInvalidator(cache.New(0))
	start := time.Now()
	waitForCrossFileSettle(inv, 80*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed < 70*time.Millisecond {
		t.Errorf("wait returned in %s, short of the 80ms ceiling", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("wait overran its ceiling: %s", elapsed)
	}
}

// TestWaitForCrossFileSettle_FallsBackWhenUnsignalled: a source that cannot
// signal keeps exactly the old behaviour, so the change cannot make any
// configuration wait less than it used to.
func TestWaitForCrossFileSettle_FallsBackWhenUnsignalled(t *testing.T) {
	start := time.Now()
	waitForCrossFileSettle(settleSourceNoWait{}, 60*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("an unsignalling source must fall back to the full sleep, got %s", elapsed)
	}
}

// TestWaitForAnyDiagnostics_WokenByAnyURI pins the invalidator primitive: a
// wildcard waiter is woken by a publish for a file it never named.
func TestWaitForAnyDiagnostics_WokenByAnyURI(t *testing.T) {
	inv := cache.NewInvalidator(cache.New(0))
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- inv.WaitForAnyDiagnostics(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	inv.Handle("textDocument/publishDiagnostics",
		[]byte(`{"uri":"file:///somewhere/else.go","diagnostics":[]}`))

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("wildcard waiter should have been woken, got: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("wildcard waiter was never woken by a publish")
	}
}

// TestWaitNextDiagnostics_StillPerURI guards the blast radius: adding the
// wildcard must not make a per-URI waiter fire for an unrelated file.
func TestWaitNextDiagnostics_StillPerURI(t *testing.T) {
	inv := cache.NewInvalidator(cache.New(0))
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := inv.WaitNextDiagnostics(ctx, "file:///wanted.go")
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	inv.Handle("textDocument/publishDiagnostics",
		[]byte(`{"uri":"file:///unrelated.go","diagnostics":[]}`))

	if err := <-done; err == nil {
		t.Error("a per-URI waiter must NOT be woken by an unrelated file's publish")
	}
}
