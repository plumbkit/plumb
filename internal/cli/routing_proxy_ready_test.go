package cli

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// TestWaitForReady_BlocksThenSucceeds proves the seam RC4 needs: when readiness
// flips from warming to ready after a few polls, waitForReady blocks and retries
// rather than failing on the first still-nil check, and ultimately returns the
// warmed client.
func TestWaitForReady_BlocksThenSucceeds(t *testing.T) {
	stub := &stubClient{id: "warm"}
	var checks atomic.Int32
	ready := func() lsp.Client {
		// nil for the first two checks (still warming), then ready.
		if checks.Add(1) >= 3 {
			return stub
		}
		return nil
	}
	start := time.Now()
	got := waitForReady(context.Background(), ready, warmReadyCap)
	if got != lsp.Client(stub) {
		t.Fatalf("expected the warmed client once ready, got %v", got)
	}
	if checks.Load() < 3 {
		t.Fatalf("expected the wait to retry (>=3 checks), got %d — it did not block", checks.Load())
	}
	if time.Since(start) > warmReadyCap {
		t.Fatalf("waited beyond the cap: %s", time.Since(start))
	}
}

// TestWaitForReady_TimesOutWhenNeverReady asserts the honest bounded timeout: a
// server that never becomes ready returns nil at (not before, not far beyond)
// the bound.
func TestWaitForReady_TimesOutWhenNeverReady(t *testing.T) {
	ready := func() lsp.Client { return nil }
	start := time.Now()
	got := waitForReady(context.Background(), ready, 100*time.Millisecond)
	if got != nil {
		t.Fatalf("expected nil when the server never becomes ready, got %v", got)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond {
		t.Fatalf("returned before the bound elapsed: %s", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("waited far beyond the bound: %s", elapsed)
	}
}

// TestWaitForReady_ReturnsPromptlyOnCancel proves the wait honours a cancelled
// context immediately rather than sleeping out the cap.
func TestWaitForReady_ReturnsPromptlyOnCancel(t *testing.T) {
	ready := func() lsp.Client { return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	got := waitForReady(ctx, ready, warmReadyCap)
	if got != nil {
		t.Fatalf("expected nil on a cancelled context, got %v", got)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("did not return promptly on cancellation: %s", time.Since(start))
	}
}

// TestWaitForReady_HonoursShorterCtxDeadline proves the bound is min(cap,
// remaining ctx): a 10s cap must not extend a query past an 80ms ctx deadline.
func TestWaitForReady_HonoursShorterCtxDeadline(t *testing.T) {
	ready := func() lsp.Client { return nil }
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := waitForReady(ctx, ready, warmReadyCap); got != nil {
		t.Fatalf("expected nil once the ctx deadline elapses, got %v", got)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("ctx deadline not honoured (cap should not extend it): %s", time.Since(start))
	}
}

// TestRoutingProxy_QueryBlocksUntilWarm proves the end-to-end fix: a query
// routed to a still-warming server blocks and retries, and once the handshake
// completes the query reaches the warmed client instead of returning an
// immediate warmingErr.
func TestRoutingProxy_QueryBlocksUntilWarm(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	// A warming entry: the pool slot exists but the proxy handle is still nil.
	cp := &clientProxy{}
	pool.entries[poolKey{rootA, "go"}] = &poolEntry{root: rootA, language: "go", proxy: cp, startedAt: time.Now()}

	rp := newRoutingProxy(pool)
	rp.setPrimary(rootA, "go", cp)

	client := &stubClient{id: "A"}
	go func() {
		time.Sleep(120 * time.Millisecond)
		cp.set(client)
	}()

	if _, err := rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(rootA, "main.go")},
	}); err != nil {
		t.Fatalf("query should block until warm then succeed, got %v", err)
	}
	if len(client.definitions) != 1 {
		t.Fatalf("expected the query to reach the warmed client, got %d calls", len(client.definitions))
	}
}

// TestRoutingProxy_QueryTimesOutWhileWarming proves that when the server never
// becomes ready within the bound, the query surfaces an honest warming error
// (not a hang), and returns close to warmCap.
func TestRoutingProxy_QueryTimesOutWhileWarming(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	cp := &clientProxy{} // stays warming forever
	pool.entries[poolKey{rootA, "go"}] = &poolEntry{root: rootA, language: "go", proxy: cp, startedAt: time.Now()}

	rp := newRoutingProxy(pool)
	rp.warmCap = 100 * time.Millisecond
	rp.setPrimary(rootA, "go", cp)

	start := time.Now()
	_, err := rp.Definition(context.Background(), protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(rootA, "main.go")},
	})
	if err == nil {
		t.Fatal("expected a warming error when the server never becomes ready")
	}
	if !strings.Contains(err.Error(), "warming") && !strings.Contains(err.Error(), "not yet ready") {
		t.Fatalf("expected an honest warm-up error, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("bounded wait exceeded: %s", time.Since(start))
	}
}

// TestRoutingProxy_QueryReturnsPromptlyOnCancel proves the block-and-retry wait
// honours context cancellation promptly even under a long cap.
func TestRoutingProxy_QueryReturnsPromptlyOnCancel(t *testing.T) {
	rootA, _ := setupTwoProjects(t)
	pool := newTestPool()
	cp := &clientProxy{} // warming
	pool.entries[poolKey{rootA, "go"}] = &poolEntry{root: rootA, language: "go", proxy: cp, startedAt: time.Now()}

	rp := newRoutingProxy(pool)
	rp.warmCap = 10 * time.Second // long cap: the ctx cancel must win
	rp.setPrimary(rootA, "go", cp)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := rp.Definition(ctx, protocol.DefinitionParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: "file://" + filepath.Join(rootA, "main.go")},
	}); err == nil {
		t.Fatal("expected an error when ctx is cancelled mid-wait")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("did not honour ctx cancellation promptly: %s", time.Since(start))
	}
}
