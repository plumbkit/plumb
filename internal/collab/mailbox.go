package collab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// mailbox.go answers one question in one place: which stored note may be handed
// to which session.
//
// It is separate from store.go because that question has three readers with
// different jobs — probe (HasPendingNotes), list (PendingNotes) and claim
// (ClaimNotes) — and they must agree exactly. A probe broader than the claim
// promises mail that never arrives; a listing broader than the claim advertises
// a message the reader can never collect, and discloses its sender and body to
// someone who was never meant to see either. None of those raise an error: the
// message simply does not show up, or shows up to the wrong agent. So the rule
// lives here, written once, rather than being copied and kept in step by hand.

// Claimant is the session asking for its mail: the three things a row is matched
// against. They travel together because all three are load-bearing and all three
// are strings — passed positionally, transposing the ID and the workspace would
// silently widen delivery instead of failing.
//
// Concurrency: a value type — safe to copy and read from any goroutine.
type Claimant struct {
	// Name is the address a note is written to.
	Name string
	// ID is this session's stable session-directory ID. A note bound to a
	// session ID is readable only by the session holding it; an unbound note
	// (every pre-v3 row, and every note to a peer that was not live when it was
	// sent) is readable by the name alone.
	ID string
	// Workspace is the caller's pinned root, which scopes cross-project mail: a
	// row carrying a target_workspace is claimable only by a session pinned there.
	Workspace string
}

// addresseeMatch is the identity half of the delivery predicate, shared by the
// listing and claiming paths so a session is never shown mail it could not
// claim. A row with an empty addressee_id is unbound and keeps the historical
// name-only semantics; a bound one is readable only by the session it names.
//
// This is what stops a session name being an identity. Names come from a small
// pool, an ended session does not reserve its name, and rename_session lets a
// session pick one — so a note left for a peer that exits before reading it
// would otherwise be handed to whoever next answers to that name, with the
// sender told it was delivered to the peer it meant.
const addresseeMatch = `AND (addressee_id = '' OR addressee_id = ?)`

// claimableWhere is the FULL predicate for "a note this claimant may be handed":
// unexpired, unclaimed, addressed to it or to "next", identity-matched, and
// within its workspace scope. ClaimNotes and HasPendingNotes share it verbatim.
//
// They must, because HasPendingNotes exists to decide whether ClaimNotes is
// worth running. A probe that is broader than the claim reports mail that does
// not arrive; one that is narrower suppresses a claim that would have delivered.
// Either way the bug is invisible — the message simply does not show up — so the
// rule is written once rather than kept in agreement by hand.
const claimableWhere = `kind = ? AND delivered_at = 0 AND expires_at > ?
		   AND (addressee = ? OR addressee = ?) ` + addresseeMatch + `
		   AND (target_workspace = '' OR target_workspace = ?)`

// claimableArgs binds claimableWhere, in its parameter order.
func claimableArgs(who Claimant, now time.Time) []any {
	return []any{string(KindNote), now.UnixNano(), who.Name, AddresseeNext, who.ID, who.Workspace}
}

// HasPendingNotes reports whether ClaimNotes would hand this claimant anything,
// without touching a row. It exists because the response-path delivery check
// runs on EVERY tool call and is nearly always a miss, and a claim is an
// expensive way to learn that.
//
// The cost is not I/O — a zero-match claim writes nothing. It is the claim's
// query PLAN: a LIST SUBQUERY plus a temp B-tree built per execution to order a
// result set that turns out to be empty, ~100-145µs, all of it holding the
// writer lock. This probe is a single indexed lookup with LIMIT 1, ~20-30µs, and
// takes no write lock at all, so it never queues behind a peer's leave_note.
//
// It is a fast path, never an authority: it may report true and the subsequent
// claim find nothing, because a peer claimed the row in between. That is the
// watermark doing its job, and callers must treat an empty claim as normal.
func (s *Store) HasPendingNotes(ctx context.Context, who Claimant, now time.Time) (bool, error) {
	if s == nil || s.db == nil || who.Name == "" {
		return false, nil
	}
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM collab_rows WHERE `+claimableWhere+` LIMIT 1`,
		claimableArgs(who, now)...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("collab: probe notes: %w", err)
	}
	return true, nil
}

// PendingNotes returns the unexpired, NOT YET DELIVERED notes addressed to the
// claimant, newest first, without claiming them. Used by the listing path
// (workspace_sessions), which reports what is waiting rather than handing it
// over. It never returns "next" notes — those are claimed only by ClaimNotes,
// so listing them would advertise a message the caller may lose the race for.
func (s *Store) PendingNotes(ctx context.Context, who Claimant, now time.Time) ([]Row, error) {
	if s == nil || s.db == nil || who.Name == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rowColumns+`
		 FROM collab_rows
		 WHERE kind = ? AND addressee = ? AND delivered_at = 0 AND expires_at > ?
		   `+addresseeMatch+`
		   AND (target_workspace = '' OR target_workspace = ?)
		 ORDER BY created_at DESC`,
		string(KindNote), who.Name, now.UnixNano(), who.ID, who.Workspace)
	if err != nil {
		return nil, fmt.Errorf("collab: query notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// UnreadSentBy returns the unexpired notes AUTHORED by authorID that nobody has
// claimed yet, oldest first — the sender's own outbox, and the only way a sender
// can learn that a message did not land. Delivery is polling-only and a
// recipient may never return, so "sent" and "read" are genuinely different
// facts; without this the sender can observe only the first.
//
// It is a pure read: it must never set the watermark. Marking a row delivered
// here would consume, on the SENDER's behalf, a message the recipient has not
// seen — the exactly-once guarantee turned into exactly-never.
//
// Addressing is by author_id, which has always been a session ID rather than a
// name, so this needs none of Claimant's identity machinery: a session asks only
// about rows it wrote itself. That is also what makes it safe against the
// daemon-level store without the recipient's cross_project gate — that gate
// stops a project READING another project's messages, and these are the caller's
// own. limit caps the scan (non-positive means no cap).
func (s *Store) UnreadSentBy(ctx context.Context, authorID string, now time.Time, limit int) ([]Row, error) {
	if s == nil || s.db == nil || authorID == "" {
		return nil, nil
	}
	q := `SELECT ` + rowColumns + `
		 FROM collab_rows
		 WHERE kind = ? AND author_id = ? AND delivered_at = 0 AND expires_at > ?
		 ORDER BY created_at ASC`
	args := []any{string(KindNote), authorID, now.UnixNano()}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("collab: query sent notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// ClaimNotes hands the caller every unexpired note addressed to who.Name or to
// "next" that nobody has claimed yet, OLDEST FIRST so a conversation reads in
// order, and marks each one delivered to who.Name in the same statement.
//
// Claiming replaced the original delete-on-delivery: a delivered row stays until
// its TTL, which is what gives a conversation a readable transcript and a
// countable number of exchanges. The read watermark — not the row's existence —
// is what stops a message being handed over twice. Because the claim is a single
// atomic UPDATE matching only `delivered_at = 0`, two sessions racing for the
// same "next" note cannot both win: the second one's UPDATE matches no rows.
//
// who.Workspace and who.ID are together what make a session NAME safe to address
// by. A row carrying a target_workspace is claimable only by a session pinned
// there; same-project rows carry none and are scoped by the database they live
// in. Without that a session could claim another project's cross-project mail
// just by adopting the right name. Within one project the same argument applies
// across TIME rather than across projects, and addresseeMatch answers it: a bound
// row names the session it is for, so a later session that inherits the name
// cannot read its predecessor's mail.
//
// limit caps how many are claimed (non-positive means no cap). The cap is
// applied by the STATEMENT, not by the caller trimming the result: a claimed row
// is marked delivered and will never be offered again, so anything the caller
// then dropped would be silently lost. Over-limit messages stay unclaimed.
//
// It is deliberately ONE `UPDATE … RETURNING`, not a transaction that reads and
// then writes. In WAL mode a DEFERRED transaction takes a read snapshot on its
// first SELECT and cannot upgrade it to a write if another connection has
// committed meanwhile: SQLite fails it with SQLITE_BUSY_SNAPSHOT, which
// busy_timeout deliberately does NOT retry. Delivery is exactly the path where
// several sessions wake at once (one send bumps every watcher), so that shape
// failed on essentially every concurrent burst. A single UPDATE takes the write
// lock up front, so contention is handled by busy_timeout as normal.
func (s *Store) ClaimNotes(ctx context.Context, who Claimant, now time.Time, limit int) ([]Row, error) {
	if s == nil || s.db == nil || who.Name == "" {
		return nil, nil
	}
	select_ := `SELECT id FROM collab_rows
			 WHERE ` + claimableWhere + `
			 ORDER BY created_at ASC`
	args := append(
		[]any{now.UnixNano(), who.Name}, // SET delivered_at, delivered_to
		claimableArgs(who, now)...)
	if limit > 0 {
		select_ += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx,
		`UPDATE collab_rows SET delivered_at = ?, delivered_to = ?
		 WHERE id IN (`+select_+`)
		 RETURNING `+rowColumns, args...)
	if err != nil {
		return nil, fmt.Errorf("collab: claim notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}
