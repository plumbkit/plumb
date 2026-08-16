package sessionstate

// widepinsweep.go — the one-time cleanup of workspace pins at a home directory
// or a container of one (issue #318).
//
// Separate from db.go, which owns the schema and the per-session CRUD: this is
// a one-shot maintenance policy with its own meta flag, and it is the only
// operation here that takes a caller-supplied predicate.

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SweepWidePinsOnce deletes every pinned_workspace row whose workspace isWide
// reports true for, and records that it ran so it never runs again on this
// database. Returns the roots it removed.
//
// This exists for issue #318. Before the fix, a client could claim any
// workspace by setting the initialize `_meta` pinned-workspace key, and the
// daemon recorded the resulting pin with session_start origin — the one origin
// permitted to name a home directory or a container of one (issue #306). Rows
// minted that way survive the fix: nothing about a stored row says which
// channel produced it, and restoring one re-persists it, refreshing updated_at,
// so the TTL prune never ages it out of a session that keeps reconnecting.
//
// ONCE is the whole point. A row a human really did declare is indistinguishable
// from a forged one, so the sweep costs that human a single re-declaration — the
// same cost the ladder already imposes when the row is absent. Running it on
// every start instead would make a deliberately declared wide workspace
// impossible to keep, which is issue #182's contract.
//
// isWide is injected rather than implemented here: containment is answered by
// probing the filesystem (internal/cli's containsUserHome), and this package
// sits below that. nil-safe.
func (s *Store) SweepWidePinsOnce(isWide func(root string) bool) ([]string, error) {
	if s == nil || isWide == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if ran, err := s.widePinSweepRan(); err != nil || ran {
		return nil, err
	}
	wide, err := s.widePins(isWide)
	if err != nil {
		return nil, err
	}
	// Deliberately NOT short-circuiting on an empty result: the flag must be
	// stamped even when there was nothing to remove. Otherwise the sweep stays
	// armed on a clean database and fires on the first wide root the caller
	// declares AFTER upgrading — which is a workspace this release never had any
	// business touching.

	// Deletes AND the flag land in one transaction, so the database is never left
	// half-swept: either every wide row is gone and the flag is set, or nothing
	// changed and the next start retries.
	//
	// Note this is belt-and-braces rather than a fix for a reachable race: the
	// daemon runs the sweep before it accepts any connection, so between a crash
	// mid-sweep and the next start there is no client able to re-declare a
	// workspace. The transaction earns its place by making the "exactly once"
	// claim true by construction instead of by that argument, which a later
	// change to the startup order could quietly invalidate.
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("sessionstate: begin wide sweep: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds; the deferred rollback is the failure path

	removed := make([]string, 0, len(wide))
	for _, p := range wide {
		if _, err := tx.Exec(`DELETE FROM pinned_workspace WHERE proxy_session_id=?`, p.id); err != nil {
			return nil, fmt.Errorf("sessionstate: delete wide pin %q: %w", p.ws, err)
		}
		// The read-tracking rows for that workspace are scoped to it and can never
		// be reached again once the pin is gone; leaving them would keep a swept
		// home directory's file list in the database until the TTL prune.
		if _, err := tx.Exec(`DELETE FROM read_tracking WHERE proxy_session_id=? AND workspace=?`, p.id, p.ws); err != nil {
			return nil, fmt.Errorf("sessionstate: delete reads for wide pin %q: %w", p.ws, err)
		}
		removed = append(removed, p.ws)
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO meta(key, value, updated_at) VALUES(?,?,?)`,
		metaWidePinSweep, "done", time.Now().UnixNano(),
	); err != nil {
		return nil, fmt.Errorf("sessionstate: stamp %s: %w", metaWidePinSweep, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sessionstate: commit wide sweep: %w", err)
	}
	return removed, nil
}

// widePinSweepRan reports whether SweepWidePinsOnce already ran on this
// database. Caller holds s.mu.
func (s *Store) widePinSweepRan() (bool, error) {
	var done string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, metaWidePinSweep).Scan(&done); {
	case err == nil:
		return true, nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("sessionstate: read %s: %w", metaWidePinSweep, err)
	}
}

// widePinRow is one pin the sweep will remove: its proxy session and its root.
type widePinRow struct{ id, ws string }

// widePins returns the pinned workspaces isWide reports true for. Caller holds
// s.mu.
func (s *Store) widePins(isWide func(root string) bool) ([]widePinRow, error) {
	rows, err := s.db.Query(`SELECT proxy_session_id, workspace FROM pinned_workspace`)
	if err != nil {
		return nil, fmt.Errorf("sessionstate: scan pins for wide sweep: %w", err)
	}
	defer rows.Close()

	var wide []widePinRow
	for rows.Next() {
		var p widePinRow
		if err := rows.Scan(&p.id, &p.ws); err != nil {
			return nil, fmt.Errorf("sessionstate: scan pin row: %w", err)
		}
		if isWide(p.ws) {
			wide = append(wide, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sessionstate: scan pins for wide sweep: %w", err)
	}
	return wide, nil
}

// metaWidePinSweep is the meta key recording that SweepWidePinsOnce has run.
//
// It is version-agnostic on purpose: it records that the cleanup happened, not
// which build did it. Downgrading to a pre-#318 binary after the sweep can mint
// fresh forged rows, and this flag then stops the sweep from ever running
// again — inherent to "once", and the reason the fix that PREVENTS new forged
// rows is the load-bearing half, not this cleanup.
const metaWidePinSweep = "wide_pin_sweep_318"
