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
	// live sessions are exempt: their pin is written once at initialize, so a
	// conversation older than the TTL would otherwise have it reclaimed while it
	// is still connected. The identity record needs no help from this list — it
	// is exempt from the sweep entirely, which is the only thing that works at
	// daemon start, when no connection exists to be exempted. See Store.Prune.
	if err := sessState.Prune(time.Now().Add(-time.Duration(ttlMinutes)*time.Minute), live...); err != nil {
		slog.Debug("daemon: session-state prune failed", "err", err)
	}
}

// reportLegacyNameConflicts logs identity records whose claim on a name cannot be
// shown to belong to one conversation.
//
// It reports rather than repairs, and that is a decision rather than an omission:
// the database holds no evidence of which claim should win, and every candidate
// repair is worse than the ambiguity. Renaming a record leaves the notes with no
// bound identity following the name, deleting one forks the identity it proves,
// and choosing by updated_at silently hands one session's name and mailbox to
// another.
//
// What it no longer reports is a conversation's own superseded generations — the
// rows a `plumb serve` RESTART leaves behind under the name its predecessor held.
// Those share a non-blank linkage and a name, so Store.LegacyNameConflicts keeps
// only the newest of them (see independentClaims). They were never a
// pre-retention artefact: retention did not stop them being written, and naming
// them at every daemon start buried the genuinely ambiguous names this warning
// exists to surface. The rows themselves are kept, because each is the durable
// proof of which session a reconnecting proxy is.
//
// Unaffected identities migrate and recover normally either way, so the cost of
// leaving this alone is bounded to the conflicting names themselves. Logged at
// Warn because an operator can resolve it deliberately and nothing else will.
func reportLegacyNameConflicts(sessState *sessionstate.Store) {
	if sessState == nil {
		return
	}
	conflicts, err := sessState.LegacyNameConflicts()
	if err != nil {
		slog.Debug("daemon: could not check retained identities for name conflicts", "err", err)
		return
	}
	for _, c := range conflicts {
		slog.Warn("daemon: more than one retained session identity holds the same name and they cannot be shown to be one conversation — plumb will not choose between claims it cannot prove apart, since deleting one forks the identity it proves and choosing by recency hands one agent's name to another on no evidence; the affected sessions keep their records and reconnect normally, but that name is ambiguous as an address",
			"name", c.Name, "claims", len(c.ProxySessionIDs))
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
