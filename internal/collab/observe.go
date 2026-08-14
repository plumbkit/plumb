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

// ConversationParticipants returns every other stable participant visible in
// this store, whether the caller authored or claimed any row here, and whether
// every named participant is backed by stable identity. The caller unions these
// snapshots across stores before deciding whether a thread has exactly one peer;
// resolving per store would let an ambiguous global half be hidden by a singleton
// local half.
func (s *Store) ConversationParticipants(
	ctx context.Context,
	conversationID, selfID string,
	now time.Time,
) ([]ConversationParticipant, bool, bool, error) {
	if selfID == "" {
		return nil, false, false, nil
	}
	rows, err := s.Conversation(ctx, conversationID, now)
	if err != nil {
		return nil, false, false, err
	}
	if len(rows) == 0 {
		// Absence in one store is neutral when a thread may span local/global.
		return nil, false, true, nil
	}
	peers, participated, complete := collectConversationParticipants(rows, selfID, s.ws)
	out := make([]ConversationParticipant, 0, len(peers))
	for _, peer := range peers {
		out = append(out, peer)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, participated, complete, nil
}

// ConversationPeerParticipant is the single-store convenience wrapper retained
// for callers that know a conversation cannot span stores.
func (s *Store) ConversationPeerParticipant(
	ctx context.Context,
	conversationID, selfID string,
	now time.Time,
) (ConversationParticipant, bool, error) {
	peers, participated, complete, err := s.ConversationParticipants(ctx, conversationID, selfID, now)
	if err != nil || !participated || !complete || len(peers) != 1 {
		return ConversationParticipant{}, false, err
	}
	return peers[0], true, nil
}

func collectConversationParticipants(
	rows []Row,
	selfID, defaultWorkspace string,
) (map[string]ConversationParticipant, bool, bool) {
	peers := make(map[string]ConversationParticipant)
	participated, complete := false, true
	for _, r := range rows {
		if r.AuthorID == selfID || r.DeliveredToID == selfID {
			participated = true
		}
		if r.AuthorSession != "" && r.AuthorID == "" {
			complete = false
		}
		if r.DeliveredTo != "" && r.DeliveredToID == "" {
			complete = false
		}
		if r.Addressee != "" && r.TargetID == "" && r.DeliveredToID == "" {
			complete = false
		}
		addConversationParticipant(
			peers, selfID, r.AuthorID, r.AuthorSession, r.OriginWorkspace, defaultWorkspace)
		addConversationParticipant(
			peers, selfID, r.TargetID, r.Addressee, r.TargetWorkspace, defaultWorkspace)
		addConversationParticipant(
			peers, selfID, r.DeliveredToID, r.DeliveredTo, r.TargetWorkspace, defaultWorkspace)
	}
	return peers, participated, complete
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

// ConversationSummariesForWorkspace returns the live cross-project notes whose
// target is workspace. It is valid only for the daemon-level store. Target-only
// scoping makes the recipient workspace's consent authoritative: a sender that
// opted in cannot expose metadata about a recipient that did not.
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
			   AND target_workspace = ?
			 GROUP BY conversation_id
			 ORDER BY COUNT(*) DESC, MAX(created_at) DESC`
		args = append(args, workspace)
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
