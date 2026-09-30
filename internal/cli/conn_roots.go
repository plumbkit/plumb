package cli

// conn_roots.go — the client roots/list_changed handling.
//
// Split from conn_attach.go by responsibility: this owns how a client's reported
// workspace roots map to (and, on change, re-pin) the connection's workspace.
// The load-bearing rule lives here — a mere reorder of a multi-root list must not
// drift the pin (issue #182) — so it sits with its own tests (conn_rootsrotation_test.go).

import (
	"context"
	"errors"
	"sync"

	"github.com/plumbkit/plumb/internal/mcp"
	"github.com/plumbkit/plumb/internal/paths"
	"github.com/plumbkit/plumb/internal/sessionstate"
)

// handleRootsListChanged services notifications/roots/list_changed: refresh the
// client-request callback, log the roots actually received (the pin-drift
// evidence issue #182 needed and did not have), then apply them.
//
// The "roots changed" line is emitted BEFORE the roots/list round-trip: that
// request is bounded (rootsListProbeTimeout) but still stalls the handler for
// up to the bound, so logging after it could make the protected grep line hard
// to find for exactly the hung-client case an operator is diagnosing. The
// received list follows on its own "roots received" line.
//
// It waits for OnInit's attach ladder first. The two run in separate
// goroutines, and a first-attach here that beat the ladder would skip its
// restore of a persisted session_start pin (rung 1b) and overwrite the stored
// row with the client's root — a silent cross-repo move on reconnect.
//
// Notifications are coalesced per connection (issue #514). The mcp server runs
// each in its own goroutine, and clients send bursts of them. Each used to do
// its own roots/list round trip, and whichever took the mutation lane last won
// — so a slow answer to an early notification could replace the answer to a
// later one. Now at most one run is in flight; a notification that arrives
// during it only marks the run stale, and the running goroutine fetches roots
// once more when it finishes. The last fetch starts after the last
// notification, so the pin settles on the newest answer. However many
// notifications land while one fetch is in flight, they cost one more fetch,
// not one each; a notification arriving during that re-fetch earns another,
// so a steady stream costs one fetch per fetch interval. The re-run uses the
// loop owner's ctx; every notification carries the same serve context, so
// nothing is lost.
//
// After the connection closes nothing is attached: the check below skips the
// work, and the attach itself re-checks inside the mutation lane (mutateLive),
// which is what closes the window between this check and the attach.
func (s *connSession) handleRootsListChanged(ctx context.Context, request mcp.RequestFn) {
	if !s.awaitInitSettled(ctx) {
		return
	}
	if !s.roots.enter(request) {
		s.log().Debug("daemon: roots changed — a refresh is in flight; it will re-fetch once it finishes")
		return
	}
	// A panicking run (the mcp server's safeRun recovers it) must not leave the
	// loop owned by nobody, or every later notification would be parked
	// forever. Released on panic only: a normal exit released it in next(), and
	// a later owner may hold it by now.
	defer func() {
		if r := recover(); r != nil {
			s.roots.abandon()
			panic(r)
		}
	}()
	for {
		request, ok := s.roots.next(func() bool { return ctx.Err() == nil && s.ctx.Err() == nil })
		if !ok {
			return
		}
		s.applyRootsNotification(ctx, request)
	}
}

// applyRootsNotification is one coalesced run: fetch the client's roots and
// apply them.
func (s *connSession) applyRootsNotification(ctx context.Context, request mcp.RequestFn) {
	s.setClientRequest(request)
	s.log().Info("daemon: roots changed — re-fetching workspace root")
	roots := rootsFromClient(ctx, request, s.log())
	s.log().Info("daemon: roots received", "count", len(roots), "roots", boundedForLog(roots, 8))
	if ctx.Err() != nil || s.ctx.Err() != nil {
		return
	}
	s.onRootsChanged(ctx, roots)
	s.startConfigWatcher()
}

// rootsCoalescer serialises a connection's roots/list_changed handling: one
// goroutine runs at a time, and notifications that arrive while it runs fold
// into a single re-run. Safe for concurrent use; the zero value is ready.
type rootsCoalescer struct {
	mu      sync.Mutex
	running bool          // a goroutine owns the run loop
	stale   bool          // a notification arrived that no fetch has started after
	request mcp.RequestFn // the newest notification's request function
}

// enter records a notification and reports whether the caller now owns the
// run loop. A false return means a run is in flight and will pick this
// notification up.
func (c *rootsCoalescer) enter(request mcp.RequestFn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.request = request
	c.stale = true
	if c.running {
		return false
	}
	c.running = true
	return true
}

// next hands the loop owner the request to fetch with, clearing stale, or
// reports false — releasing ownership in the same critical section — when no
// notification is outstanding or live says the work is no longer wanted.
// Releasing under the lock is what keeps a notification from being lost: one
// that arrives after this returns false finds running cleared and takes the
// loop itself.
func (c *rootsCoalescer) next(live func() bool) (mcp.RequestFn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.stale || !live() {
		c.running = false
		c.stale = false
		return nil, false
	}
	c.stale = false
	return c.request, true
}

// abandon releases the loop after its owner panicked, dropping whatever was
// outstanding: the next notification takes the loop and fetches afresh.
func (c *rootsCoalescer) abandon() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.running = false
	c.stale = false
}

// markInitSettled records that OnInit's attach ladder has finished. Idempotent.
func (s *connSession) markInitSettled() {
	s.initSettledOnce.Do(func() { close(s.initSettled) })
}

// awaitInitSettled blocks until the attach ladder has run, reporting false when
// the call or the connection ends first.
func (s *connSession) awaitInitSettled(ctx context.Context) bool {
	select {
	case <-s.initSettled:
		return s.ctx.Err() == nil
	case <-ctx.Done():
		return false
	case <-s.ctx.Done():
		return false
	}
}

// onRootsChanged applies a client's updated workspace roots (the
// notifications/roots/list_changed path). On the first attach it pins the root,
// like OnInit. When the connection is already pinned and the client reports a
// different root — an editor that genuinely switched folders — it re-pins to
// follow the switch, closing the same "welded connection" gap that the
// session_start re-pin fixed for clients that never report roots (Claude
// Desktop). An empty or unchanged root is left alone: repinWorkspace no-ops when
// the resolved root matches the current pin, so a spurious notification (or a
// roots/list the client cannot satisfy) never tears the workspace down.
func (s *connSession) onRootsChanged(ctx context.Context, roots []string) {
	if len(roots) == 0 {
		return // client reported no usable roots — keep the current pin
	}
	if s.view().acquiredRoot == "" {
		s.attachWorkspace(ctx, roots[0])
		s.applyProjectConfig(s.workspace())
		return
	}
	// A multi-root client that merely REORDERS its roots (or adds/removes OTHER
	// folders) must not drag the pin between projects. Taking Roots[0] on every
	// notification did exactly that — issue #182's roots-rotation drift, with no
	// session_start in between. Keep the pin while its root is still reported; only
	// a genuine removal of our workspace re-pins.
	if s.pinnedRootStillReported(roots) {
		return
	}
	// A pin the caller set with an explicit session_start outranks client-reported
	// roots entirely (the live counterpart of the persisted-pin promotion rule): a
	// multiplexing client managing one shared folder set across agent sessions
	// must not drag a deliberate pin away by dropping our root (issue #182).
	// Snapshot fast path — the authoritative, race-free check runs again inside
	// attachOrRepinTo's mutation lane.
	if s.pinExplicitlyHeld() {
		s.log().Info("daemon: roots changed — keeping explicit session_start pin", "pinned", s.workspace(), "roots", boundedForLog(roots, 8))
		return
	}
	folder := paths.URIToPath(roots[0])
	if folder == "" || folder == "/" {
		return
	}
	if _, err := s.repinWorkspaceFrom(ctx, folder, "", sessionstate.PinSourceRoots, pinTriggerLive, false); err != nil {
		if errors.Is(err, errConnClosed) {
			return // the connection went away mid-notification; nothing to report
		}
		s.log().Warn("daemon: roots-changed re-pin failed", "to", folder, "err", err)
	}
}

// pinnedRootStillReported reports whether the currently-pinned workspace root is
// still derivable from any of the client's reported roots, resolved the same way
// a re-pin would — so a reported subfolder that Detects up to the pinned root
// counts as "still reported".
func (s *connSession) pinnedRootStillReported(roots []string) bool {
	cur := s.workspace()
	if cur == "" {
		return false
	}
	for _, r := range roots {
		folder := paths.URIToPath(r)
		if folder == "" || folder == "/" {
			continue
		}
		if s.resolveRootFolder(folder) == cur {
			return true
		}
	}
	return false
}

// resolveRootFolder resolves an absolute folder to its workspace root the same
// way repinWorkspaceFrom does: the detected project root, or the folder itself
// synthesised as a root when no marker is found. Kept in step with that
// resolution — including explicit=false: a client-reported root is not a
// session_start declaration, so a reported $HOME resolves to "" here exactly
// as the re-pin itself would refuse it (it then simply never matches the pin).
func (s *connSession) resolveRootFolder(folder string) string {
	root, _, err := s.pool.Detect(folder)
	if err != nil {
		return s.pool.SynthesiseRoot(folder, false)
	}
	return root
}
