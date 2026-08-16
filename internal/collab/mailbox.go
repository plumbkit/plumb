package collab

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// Claimant is the session asking for its mail: everything a row is matched
// against. The fields travel together because all of them are load-bearing and
// all of them are strings — passed positionally, transposing the ID and the
// workspace would silently widen delivery instead of failing.
//
// Concurrency: a value type — safe to copy and read from any goroutine.
// InheritedIDs must not be mutated after construction.
type Claimant struct {
	// Name is the address a note is written to.
	Name string
	// ID is this session's stable session-directory ID. A note bound to a
	// session ID is readable only by the session holding it; an unbound note
	// (every pre-v3 row, and every note to a peer that was not live when it was
	// sent) is readable by the name alone.
	ID string
	// InheritedIDs are session IDs this session PROVABLY continues — the
	// predecessor whose place it took when a daemon restart brought the same
	// serve proxy back. They are accepted exactly like ID, so mail bound to a
	// session that a restart ended still reaches the agent it was written for.
	//
	// The word that matters is provably. An inherited ID is granted only by the
	// persisted-state path, keyed on the proxy's own unguessable session ID,
	// which the reconnecting proxy presents from its own memory. It is never
	// granted for answering to a name: that would hand any session its
	// predecessor's mailbox for the cost of a rename_session call, which is the
	// hole addressee_id exists to close.
	InheritedIDs []string
	// Workspace is the caller's pinned root, which scopes cross-project mail: a
	// row carrying a target_workspace is claimable only by a session pinned there.
	Workspace string
}

// identities returns the addressee_id values this claimant may read, its own
// first. Blanks and duplicates among the inherited IDs are dropped, so a stray
// empty string cannot turn the IN list into a match on unbound rows it was not
// meant to widen — though that particular case is harmless, since the unbound
// arm already matches those.
//
// The result always holds at least one element (the caller's own ID, possibly
// empty), which keeps the generated IN list syntactically valid without a
// special case.
func (c Claimant) identities() []string {
	out := make([]string, 0, 1+len(c.InheritedIDs))
	out = append(out, c.ID)
	for _, id := range c.InheritedIDs {
		if id == "" || slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// addresseeMatch builds the identity half of the delivery predicate together
// with its arguments, so the placeholder count and the values bound to them
// cannot disagree — the failure a separate SQL constant and args function
// invites once the number of identities stops being fixed.
//
// A row with an empty addressee_id is unbound and keeps the historical
// name-only semantics; a bound one is readable only by a session presenting the
// ID it names.
//
// This is what stops a session name being an identity. Names come from a small
// pool, an ended session does not reserve its name, and rename_session lets a
// session pick one — so a note left for a peer that exits before reading it
// would otherwise be handed to whoever next answers to that name, with the
// sender told it was delivered to the peer it meant.
func addresseeMatch(who Claimant) (string, []any) {
	ids := who.identities()
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	return `AND (addressee_id = '' OR addressee_id IN (` + strings.Join(marks, ", ") + `))`, args
}

// notAuthoredBy builds the sender-exclusion half of the delivery predicate,
// together with its arguments, for the same reason addresseeMatch does: the
// placeholder count varies with the claimant's identity list, and a separate SQL
// constant would eventually disagree with it.
//
// Without this a session that writes a to:"next" note claims it back on its own
// next tool call. That is not merely useless — delivery is exactly-once, so the
// author CONSUMES the message and the peer it was written for can never receive
// it, while the sender was told the send succeeded. Nothing in the exchange
// reveals the loss.
//
// The empty arm is load-bearing. author_id is empty on rows written before senders
// were attributed, and a claimant may itself hold no ID; a bare author_id != ?
// would then read as "author_id is not empty" and suppress every legacy row for
// exactly the sessions least able to notice. A row with no recorded author
// cannot be proven self-authored, so it stays deliverable.
//
// Inherited IDs are excluded too. A restarted session that inherited its
// predecessor's mailbox is the same logical agent as the session that wrote the
// note, so handing it back would recreate the loop across a restart.
func notAuthoredBy(who Claimant) (string, []any) {
	ids := who.identities()
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	return `AND (author_id = '' OR author_id NOT IN (` + strings.Join(marks, ", ") + `))`, args
}

// claimable is the FULL predicate for "a note this claimant may be handed":
// unexpired, unclaimed, addressed to it or to "next", identity-matched, not
// written by it, and within its workspace scope. ClaimNotes and HasPendingNotes
// share it verbatim.
//
// They must, because HasPendingNotes exists to decide whether ClaimNotes is
// worth running. A probe that is broader than the claim reports mail that does
// not arrive; one that is narrower suppresses a claim that would have delivered.
// Either way the bug is invisible — the message simply does not show up — so the
// rule is written once rather than kept in agreement by hand. The author
// exclusion belongs HERE and not in ClaimNotes for that reason: put it only on
// the claim and the probe announces mail the claim then refuses to hand over,
// which is a spin loop rather than a fix.
//
// Note what the identity clause does NOT touch. "next" is matched by the
// addressee arm and always carries an empty addressee_id, so no number of
// identities widens it. target_workspace is its own AND term, so an inherited ID
// never reaches another project's mail. Both remain exactly as strict as before.
func claimable(who Claimant, now time.Time) (string, []any) {
	idSQL, idArgs := addresseeMatch(who)
	authorSQL, authorArgs := notAuthoredBy(who)
	where := `kind = ? AND delivered_at = 0 AND expires_at > ?
			   AND (addressee = ? OR addressee = ?) ` + idSQL + `
			   ` + authorSQL + `
			   AND (target_workspace = '' OR target_workspace = ?)`
	args := append([]any{string(KindNote), now.UnixNano(), who.Name, AddresseeNext}, idArgs...)
	args = append(args, authorArgs...)
	return where, append(args, who.Workspace)
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
	where, args := claimable(who, now)
	query := `SELECT 1 FROM collab_rows WHERE ` + where + ` LIMIT 1`

	var one int
	var err error
	if stmt, prepErr := s.probeStmt(ctx, query); prepErr == nil {
		err = stmt.QueryRowContext(ctx, args...).Scan(&one)
	} else {
		// Preparing is an optimisation, so failing to prepare must not fail the
		// probe. Nothing is swallowed by falling through: whatever broke the
		// prepare breaks this query too, and its error is the one returned.
		err = s.db.QueryRowContext(ctx, query, args...).Scan(&one)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("collab: probe notes: %w", err)
	}
	return true, nil
}

// probeStmt returns the prepared form of one probe statement, preparing it on
// first use and reusing it thereafter.
//
// Preparing is most of the probe's cost. Measured on the mailbox benchmark
// (BenchmarkMailProbe_Idle, variants v2-probe and v2p-probe-prepared, single
// session): ~25.8µs unprepared against ~5.8µs prepared, the same statement
// either way. Since the probe runs on every tool call that reaches the response
// path, that is ~20µs of pure parse work per call, repeated forever.
//
// THE CACHE IS KEYED BY SQL TEXT, and cannot be a single statement, because the
// probe's text is not fixed. claimable embeds addresseeMatch, which emits one
// placeholder per identity the claimant holds — its own ID plus any inherited
// predecessor IDs — so a session that inherited a predecessor's mailbox probes
// with a different statement from one that did not. Caching a single statement
// would bind the first shape seen and then feed a later claimant's argument list
// to it: at best an argument-count error, at worst the wrong identity set
// silently in force, which is the hole addressee_id exists to close. Keying by
// the text makes the two shapes distinct entries rather than a collision.
//
// The map is bounded by the number of distinct identity counts a workspace's
// sessions present — one, or two where a restart granted an inheritance — not by
// the number of sessions or claimants.
//
// Close releases every entry; see Store.Close for why leaving them to the handle
// is not good enough.
func (s *Store) probeStmt(ctx context.Context, query string) (*sql.Stmt, error) {
	s.probeMu.Lock()
	defer s.probeMu.Unlock()
	if stmt, ok := s.probeStmts[query]; ok {
		return stmt, nil
	}
	// Preparing under the lock rather than racing and discarding a loser: this
	// runs at most once per statement shape per store, so the contention is a
	// one-off, and the alternative prepares the same statement twice on the very
	// first concurrent probe.
	stmt, err := s.db.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	if s.probeStmts == nil {
		s.probeStmts = make(map[string]*sql.Stmt, 2)
	}
	s.probeStmts[query] = stmt
	return stmt, nil
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
	idSQL, idArgs := addresseeMatch(who)
	args := append([]any{string(KindNote), who.Name, now.UnixNano()}, idArgs...)
	//nolint:gosec // G202: idSQL is generated by addresseeMatch from a count, not from
	// caller data — it contains only "?" placeholders and fixed SQL, and every identity
	// is bound as a parameter. TestAddresseeMatch_InterpolatesNoData enforces that.
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+rowColumns+`
		 FROM collab_rows
		 WHERE kind = ? AND addressee = ? AND delivered_at = 0 AND expires_at > ?
		   `+idSQL+`
		   AND (target_workspace = '' OR target_workspace = ?)
		 ORDER BY created_at DESC`,
		append(args, who.Workspace)...)
	if err != nil {
		return nil, fmt.Errorf("collab: query notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// UnreadSentBy returns the unexpired notes AUTHORED by authorID that nobody has
// claimed yet, oldest first — the sender's own outbox, and the only way a sender
// can learn that a message did not land. plumb does not push and a
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
	return s.sentBy(ctx, authorID, now, limit, true)
}

// SentBy returns the caller's unexpired sent notes whether or not they have
// been read, NEWEST first — the observability view of the same outbox
// UnreadSentBy exposes.
//
// The two orderings are not a style difference. UnreadSentBy answers "what have
// I lost?", so it leads with the oldest, which is the one most likely to expire
// unread. This answers "what have I been doing?", where the newest is the one
// worth seeing first, and it keeps delivered rows because "delivered" is the
// fact being displayed — an outbox that showed only failures could not
// distinguish a quiet session from a broken one.
//
// Same author_id scoping and the same pure-read guarantee as UnreadSentBy: a
// session asks only about rows it wrote itself, and nothing here moves a
// watermark.
func (s *Store) SentBy(ctx context.Context, authorID string, now time.Time, limit int) ([]Row, error) {
	return s.sentBy(ctx, authorID, now, limit, false)
}

// sentBy is the one query behind both, so the outbox cannot come to mean two
// different things depending on which caller asked.
func (s *Store) sentBy(ctx context.Context, authorID string, now time.Time, limit int, unreadOnly bool) ([]Row, error) {
	if s == nil || s.db == nil || authorID == "" {
		return nil, nil
	}
	q := `SELECT ` + rowColumns + `
		 FROM collab_rows
		 WHERE kind = ? AND author_id = ? AND expires_at > ?`
	if unreadOnly {
		q += ` AND delivered_at = 0 ORDER BY created_at ASC`
	} else {
		q += ` ORDER BY created_at DESC`
	}
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
	where, whereArgs := claimable(who, now)
	select_ := `SELECT id FROM collab_rows
			 WHERE ` + where + `
			 ORDER BY created_at ASC`
	args := append(
		[]any{now.UnixNano(), who.Name, who.ID}, // SET delivered_at, delivered_to, delivered_to_id
		whereArgs...)
	if limit > 0 {
		select_ += ` LIMIT ?`
		args = append(args, limit)
	}
	//nolint:gosec // G202: select_ is built from claimable, whose only variable part is
	// a generated "?" list — no caller data reaches the statement text; identities and
	// the limit are bound. TestAddresseeMatch_InterpolatesNoData enforces that.
	rows, err := s.db.QueryContext(ctx,
		`UPDATE collab_rows SET delivered_at = ?, delivered_to = ?, delivered_to_id = ?
		 WHERE id IN (`+select_+`)
		 RETURNING `+rowColumns, args...)
	if err != nil {
		return nil, fmt.Errorf("collab: claim notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}
