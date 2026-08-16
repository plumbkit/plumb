package cli

// daemon_sessionstate.go — the session-state maintenance the daemon runs at
// start, before any connection is accepted.
//
// Split from daemon.go, which owns process lifecycle: these are policy
// decisions about persisted state (what expires, what is no longer trusted),
// and they answer to the session-state schema rather than to the listener.

import (
	"log/slog"
	"time"

	"github.com/plumbkit/plumb/internal/sessionstate"
)

// pruneSessionState reclaims persisted per-connection state older than the TTL,
// dropping rows left by a serve proxy that died without reconnecting. A TTL of 0
// disables pruning (state lingers until the next daemon restart with a positive
// TTL). Best-effort and nil-safe.
func pruneSessionState(sessState *sessionstate.Store, ttlMinutes int, live ...string) {
	if sessState == nil || ttlMinutes <= 0 {
		return
	}
	// live sessions are exempt: their pin and name are written once at
	// initialize, so a conversation older than the TTL would otherwise have them
	// reclaimed while it is still connected.
	if err := sessState.Prune(time.Now().Add(-time.Duration(ttlMinutes)*time.Minute), live...); err != nil {
		slog.Debug("daemon: session-state prune failed", "err", err)
	}
}

// sweepLegacyWidePins drops persisted pins at a home directory or a container of
// one, exactly once per database (issue #318).
//
// Before this release any client could claim a workspace through the initialize
// `_meta` pinned-workspace key, and the daemon stored the result with
// session_start origin — the origin #306 lets name a wide root. A row minted
// that way outlives the fix: nothing in the row records which channel wrote it,
// and restoring one re-persists it, so the TTL prune never reaches it.
//
// A wide row a human really did declare is indistinguishable from a forged one,
// so this costs that human one re-declaration and then never fires again. It is
// logged at Warn naming each root, because a session that was working will stop
// attaching and the operator deserves to know why.
func sweepLegacyWidePins(sessState *sessionstate.Store) {
	if sessState == nil {
		return
	}
	removed, err := sessState.SweepWidePinsOnce(containsUserHome)
	if err != nil {
		slog.Warn("daemon: one-time wide-pin sweep failed; it will be retried on the next start", "err", err)
		return
	}
	if len(removed) > 0 {
		slog.Warn("daemon: dropped persisted workspace pin(s) at a home directory or a container of one — a one-time cleanup for issue #318; re-pin deliberately with session_start if you meant to work there",
			"roots", boundedForLog(removed, 5))
	}
}
