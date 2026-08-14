package collab

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ConversationPeer resolves the one other participant in a conversation. It
// fails closed when the thread is missing or has more than two participants;
// callers can then require an explicit addressee instead of guessing.
func (s *Store) ConversationPeer(
	ctx context.Context,
	conversationID, selfID, selfName string,
	now time.Time,
) (string, bool, error) {
	rows, err := s.Conversation(ctx, conversationID, now)
	if err != nil || len(rows) == 0 {
		return "", false, err
	}
	peers := make(map[string]struct{})
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && name != AddresseeNext && name != selfName {
			peers[name] = struct{}{}
		}
	}
	for _, r := range rows {
		own := (selfID != "" && r.AuthorID == selfID) || r.AuthorSession == selfName
		if own {
			add(r.Addressee)
			add(r.DeliveredTo)
			continue
		}
		add(r.AuthorSession)
	}
	if len(peers) != 1 {
		return "", false, nil
	}
	for peer := range peers {
		return peer, true, nil
	}
	return "", false, nil
}

// RecentSentNotes returns an author's live notes newest first. Delivered rows
// remain visible until expiry, which lets status surfaces distinguish pending
// from delivered without introducing read receipts.
func (s *Store) RecentSentNotes(
	ctx context.Context,
	authorID string,
	now time.Time,
	limit int,
) ([]Row, error) {
	if s == nil || s.db == nil || authorID == "" {
		return nil, nil
	}
	query := `SELECT ` + rowColumns + `
		 FROM collab_rows
		 WHERE kind = ? AND author_id = ? AND expires_at > ?
		 ORDER BY created_at DESC`
	args := []any{string(KindNote), authorID, now.UnixNano()}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("collab: query sent notes: %w", err)
	}
	defer rows.Close()
	return scanRows(rows)
}

// ConversationSummary is the human-observability view of one live conversation.
type ConversationSummary struct {
	ID      string
	Notes   int
	Pending int
	LastAt  time.Time
}

// ConversationSummaries returns the busiest live conversations first. Counts
// include delivered notes; pending is the subset not yet claimed.
func (s *Store) ConversationSummaries(
	ctx context.Context,
	now time.Time,
	limit int,
) ([]ConversationSummary, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	query := `SELECT conversation_id, COUNT(*),
		        SUM(CASE WHEN delivered_at = 0 THEN 1 ELSE 0 END), MAX(created_at)
		 FROM collab_rows
		 WHERE kind = ? AND expires_at > ?
		 GROUP BY conversation_id
		 ORDER BY COUNT(*) DESC, MAX(created_at) DESC`
	args := []any{string(KindNote), now.UnixNano()}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("collab: summarise conversations: %w", err)
	}
	defer rows.Close()
	var out []ConversationSummary
	for rows.Next() {
		var summary ConversationSummary
		var lastNS int64
		if err := rows.Scan(&summary.ID, &summary.Notes, &summary.Pending, &lastNS); err != nil {
			return nil, fmt.Errorf("collab: scan conversation summary: %w", err)
		}
		summary.LastAt = time.Unix(0, lastNS)
		out = append(out, summary)
	}
	return out, rows.Err()
}
