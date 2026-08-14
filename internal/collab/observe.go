package collab

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ConversationParticipant is one stable author in a mailbox thread.
type ConversationParticipant struct {
	ID        string
	Name      string
	Workspace string
}

// ConversationPeerParticipant resolves the one other stable participant in a
// conversation and proves the caller authored or claimed at least one row. It
// fails closed for legacy name-only rows, missing participation, or group
// threads: display names are presentation, never identity.
func (s *Store) ConversationPeerParticipant(
	ctx context.Context,
	conversationID, selfID string,
	now time.Time,
) (ConversationParticipant, bool, error) {
	if selfID == "" {
		return ConversationParticipant{}, false, nil
	}
	rows, err := s.Conversation(ctx, conversationID, now)
	if err != nil || len(rows) == 0 {
		return ConversationParticipant{}, false, err
	}
	peers, participated := collectConversationParticipants(rows, selfID, s.ws)
	if !participated || len(peers) != 1 {
		return ConversationParticipant{}, false, nil
	}
	for _, peer := range peers {
		return peer, true, nil
	}
	return ConversationParticipant{}, false, nil
}

func collectConversationParticipants(
	rows []Row,
	selfID, defaultWorkspace string,
) (map[string]ConversationParticipant, bool) {
	peers := make(map[string]ConversationParticipant)
	participated := false
	for _, r := range rows {
		if r.AuthorID == selfID || r.DeliveredToID == selfID {
			participated = true
		}
		addConversationParticipant(
			peers, selfID, r.AuthorID, r.AuthorSession, r.OriginWorkspace, defaultWorkspace)
		addConversationParticipant(
			peers, selfID, r.DeliveredToID, r.DeliveredTo, r.TargetWorkspace, defaultWorkspace)
	}
	return peers, participated
}

func addConversationParticipant(
	peers map[string]ConversationParticipant,
	selfID, id, name, workspace, defaultWorkspace string,
) {
	id, name = strings.TrimSpace(id), strings.TrimSpace(name)
	if id == "" || id == selfID || name == "" {
		return
	}
	if workspace == "" {
		workspace = defaultWorkspace
	}
	peer := ConversationParticipant{ID: id, Name: name, Workspace: workspace}
	if prior, ok := peers[id]; ok && peer.Workspace == "" {
		peer.Workspace = prior.Workspace
	}
	peers[id] = peer
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
	return s.conversationSummaries(ctx, now, limit, "")
}

// ConversationSummariesForWorkspace returns cross-project conversations in
// which workspace is either endpoint. It is valid only for the daemon-level
// store; the workspace predicate prevents one project's status surface from
// observing unrelated conversations.
func (s *Store) ConversationSummariesForWorkspace(
	ctx context.Context,
	workspace string,
	now time.Time,
	limit int,
) ([]ConversationSummary, error) {
	if workspace == "" || s == nil || !s.IsGlobal() {
		return nil, nil
	}
	return s.conversationSummaries(ctx, now, limit, workspace)
}

func (s *Store) conversationSummaries(
	ctx context.Context,
	now time.Time,
	limit int,
	workspace string,
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
	if workspace != "" {
		query = `SELECT conversation_id, COUNT(*),
			        SUM(CASE WHEN delivered_at = 0 THEN 1 ELSE 0 END), MAX(created_at)
			 FROM collab_rows
			 WHERE kind = ? AND expires_at > ?
			   AND (origin_workspace = ? OR target_workspace = ?)
			 GROUP BY conversation_id
			 ORDER BY COUNT(*) DESC, MAX(created_at) DESC`
		args = append(args, workspace, workspace)
	}
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

// MergeConversationSummaries combines local and cross-project views without
// turning volume into enforcement. A conversation ID seen in more than one view
// is counted once per note source and sorted with the same busiest-first rule.
func MergeConversationSummaries(limit int, groups ...[]ConversationSummary) []ConversationSummary {
	byID := make(map[string]ConversationSummary)
	for _, group := range groups {
		for _, summary := range group {
			merged := byID[summary.ID]
			merged.ID = summary.ID
			merged.Notes += summary.Notes
			merged.Pending += summary.Pending
			if summary.LastAt.After(merged.LastAt) {
				merged.LastAt = summary.LastAt
			}
			byID[summary.ID] = merged
		}
	}
	out := make([]ConversationSummary, 0, len(byID))
	for _, summary := range byID {
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Notes != out[j].Notes {
			return out[i].Notes > out[j].Notes
		}
		return out[i].LastAt.After(out[j].LastAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}
