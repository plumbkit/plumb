package collab

import (
	"context"
	"fmt"
	"time"
)

// outbox.go is the sender's view of the mailbox: the notes a session WROTE,
// read by author_id. It is apart from mailbox.go because it answers the opposite
// question — not "what may this claimant be handed" but "what became of what I
// sent" — and so needs none of the claimant's addressing or read scope: a
// session asks only about rows it wrote itself.

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
