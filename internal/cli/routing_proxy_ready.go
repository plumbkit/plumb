package cli

import (
	"context"
	"time"

	"github.com/plumbkit/plumb/internal/lsp"
)

// Bounded readiness wait for a still-warming language server. Rather than fail a
// query the instant the routed server's handshake is incomplete — the leak
// behind the "LSP server not yet ready" cold-start errors — a query blocks for a
// bounded window so a first call that lands mid-warm-up succeeds instead of
// surfacing warmingErr. Split from routing_proxy.go to keep that file under the
// size cap.

// warmReadyCap bounds the extra time a routed query blocks waiting for a
// still-warming server before it gives up and surfaces warmingErr. Kept well
// under the [lsp_query] withLSPDeadline (30s default) so a query that must wait
// still leaves the LSP call itself ample budget; the effective bound is
// min(this, the caller's remaining context deadline).
const warmReadyCap = 10 * time.Second

// warmPollInterval is how often the readiness wait re-checks the proxy handle.
// Coarse enough to cost nothing against a cold start measured in seconds, fine
// enough that the server flipping ready is picked up near-instantly.
const warmPollInterval = 50 * time.Millisecond

// awaitEntryReady returns e's live client, blocking up to r.warmCap (further
// capped by ctx) for a still-warming server instead of failing the query
// outright. On success it touches the proxy so the idle clock reflects the call.
// Returns nil if the bound elapses, or ctx is done, before the server is ready.
//
// It holds NO pool or path lock while waiting (see waitForReady), so a warming
// wait never serialises other sessions and cannot deadlock.
func (r *routingProxy) awaitEntryReady(ctx context.Context, e *poolEntry) lsp.Client {
	c := waitForReady(ctx, e.proxy.get, r.warmCap)
	if c != nil {
		e.proxy.touch()
	}
	return c
}

// waitForReady polls ready until it returns a non-nil client or the bound
// elapses, whichever comes first. The bound is min(capDur, the caller's
// remaining context deadline); it selects on ctx.Done so a cancelled or expired
// caller returns promptly, and it takes no pool or write lock — ready is the
// lock-free clientProxy.get — so a warming wait cannot serialise sessions or
// deadlock. ready is the injection seam a test drives with a fake handshake.
func waitForReady(ctx context.Context, ready func() lsp.Client, capDur time.Duration) lsp.Client {
	if c := ready(); c != nil {
		return c
	}
	remaining := capDur
	if d, ok := ctx.Deadline(); ok {
		if until := time.Until(d); until < remaining {
			remaining = until
		}
	}
	if remaining <= 0 {
		return nil
	}
	bound := time.NewTimer(remaining)
	defer bound.Stop()
	ticker := time.NewTicker(warmPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-bound.C:
			return ready()
		case <-ticker.C:
			if c := ready(); c != nil {
				return c
			}
		}
	}
}
