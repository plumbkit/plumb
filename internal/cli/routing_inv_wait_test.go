package cli

import (
	"context"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/cache"
	"github.com/plumbkit/plumb/internal/config"
)

// anyDiagnosticsWaiter mirrors the OPTIONAL interface that
// internal/tools.waitForCrossFileSettle type-asserts. It is duplicated here
// because that interface is unexported, and because the assertion is exactly
// what silently failed: WriteDeps.Diag is always *routingInvProxy in a real
// session, never a bare *cache.Invalidator, so the post-write settle grace fell
// back to a fixed sleep in production while unit tests — which construct an
// Invalidator directly — passed.
type anyDiagnosticsWaiter interface {
	WaitForAnyDiagnostics(ctx context.Context) error
}

// TestRoutingInvProxy_SatisfiesAnyDiagnosticsWaiter is the compile-time guard
// against that whole class of gap: the type production actually wires must keep
// satisfying the optional interface, or the optimisation silently disappears.
func TestRoutingInvProxy_SatisfiesAnyDiagnosticsWaiter(t *testing.T) {
	var _ anyDiagnosticsWaiter = (*routingInvProxy)(nil)
	var _ anyDiagnosticsWaiter = (*cache.Invalidator)(nil)
}

// TestRoutingInvProxy_WaitForAnyDiagnostics_WokenByPrimary exercises the real
// production type end to end: a publish on the primary invalidator must wake a
// waiter held through the proxy.
func TestRoutingInvProxy_WaitForAnyDiagnostics_WokenByPrimary(t *testing.T) {
	inv := cache.NewInvalidator(cache.New(0))
	proxy := newRoutingInvProxy(newWorkspacePool(context.Background(), config.Config{}))
	proxy.setPrimary("/ws", "go", inv)

	// Publish repeatedly so the wake cannot be missed by racing the subscription.
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
					[]byte(`{"uri":"file:///ws/dep.go","diagnostics":[]}`))
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := proxy.WaitForAnyDiagnostics(ctx)
	elapsed := time.Since(start)
	close(stop)
	<-done

	if err != nil {
		t.Fatalf("expected the proxy to be woken by a publish, got: %v", err)
	}
	if elapsed >= time.Second {
		t.Errorf("proxy waited %s; it must return as soon as a server republishes", elapsed)
	}
}

// TestRoutingInvProxy_WaitForAnyDiagnostics_NoPrimary: with no server attached
// there is nothing to wait on, so the call must still honour the caller's
// ceiling rather than returning instantly (which would make the settle grace a
// no-op) or blocking forever.
func TestRoutingInvProxy_WaitForAnyDiagnostics_NoPrimary(t *testing.T) {
	proxy := newRoutingInvProxy(newWorkspacePool(context.Background(), config.Config{}))

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := proxy.WaitForAnyDiagnostics(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Error("expected the context error when there is no server to wait on")
	}
	if elapsed < 60*time.Millisecond {
		t.Errorf("returned in %s, short of the ceiling — the grace would be skipped entirely", elapsed)
	}
}
