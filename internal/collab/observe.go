package collab

// observe.go is the human-observability read side of the mailbox: aggregate
// counts a person can scan, not message content.
//
// It exists because the exchange cap ([collab] max_exchanges) is a blunt answer
// to a real question — "are these agents talking a lot?" — and the cap can only
// answer it by SEVERING the conversation, after the fact, destroying whatever
// was being composed. Counting is the non-destructive answer to the same
// question: a human watching note volume can intervene when an exchange looks
// like a loop, and an exchange that is legitimate pays nothing.
//
// Everything here is strictly derived and read-only. No function in this file
// claims a row, moves a watermark, or returns a body — a volume view that
// disclosed message content would be a second delivery path around the
// addressee binding, which is exactly what internal/collab exists to prevent.

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// ConversationSummary is one live conversation, counted.
//
// Deliberately carries no participants and no bodies. Naming who is in a thread
// is a disclosure decision with a different answer per surface (see the
// cross-project consent rule in internal/tools), while a count is safe
// everywhere — so the shape that is safe everywhere is the one that lives in
// the store.
type ConversationSummary struct {
	// ID is the conversation id notes quote to stay in thread.
	ID string
	// Notes is every unexpired note in the thread, delivered or not.
	Notes int
	// Pending is the subset nobody has claimed yet. Pending == Notes over a long
	// thread is the shape worth looking at: it means one side has stopped
	// reading, which no message count alone would reveal.
	Pending int
	// LastAt is the most recent note's creation time — how a stalled thread is
	// told from a live one.
	LastAt time.Time
}

// ConversationSummaries counts every live conversation in this store, busiest
// first. limit caps the result (non-positive means no cap).
func (s *Store) ConversationSummaries(ctx context.Context, now time.Time, limit int) ([]ConversationSummary, error) {
	return s.conversationSummaries(ctx, now, limit, "")
}

// ConversationSummariesForWorkspace counts the live conversations in the
// daemon-level store that are TARGETED at one workspace.
//
// Only meaningful on the global store, and it returns nothing on any other —
// target_workspace is stamped only on cross-project rows, so asking a project's
// own collab.db this question would silently count nothing and read as "no
// cross-project traffic" rather than "wrong store".
//
// The workspace filter is what makes recipient consent enforceable by the
// caller: a project can be shown the cross-project volume aimed AT it, having
// opted in, without being shown any other project's traffic.
func (s *Store) ConversationSummariesForWorkspace(
	ctx context.Context, workspace string, now time.Time, limit int,
) ([]ConversationSummary, error) {
	if s == nil || workspace == "" || !s.IsGlobal() {
		return nil, nil
	}
	return s.conversationSummaries(ctx, now, limit, workspace)
}

func (s *Store) conversationSummaries(
	ctx context.Context, now time.Time, limit int, workspace string,
) ([]ConversationSummary, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	// Aggregated in SQL rather than by scanning rows: this runs on a listing path
	// that a human may refresh repeatedly, and the alternative reads every body
	// into memory to count them — paying the disclosure risk and the allocation
	// for a number.
	where := `WHERE kind = ? AND expires_at > ?`
	args := []any{string(KindNote), now.UnixNano()}
	if workspace != "" {
		where += ` AND target_workspace = ?`
		args = append(args, workspace)
	}
	query := `SELECT conversation_id, COUNT(*),
	                 SUM(CASE WHEN delivered_at = 0 THEN 1 ELSE 0 END),
	                 MAX(created_at)
	          FROM collab_rows ` + where + `
	          GROUP BY conversation_id
	          ORDER BY COUNT(*) DESC, MAX(created_at) DESC`
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

// MergeConversationSummaries folds several stores' counts into one ranking.
//
// A conversation can legitimately have notes in two stores at once: a
// same-project reply lands in the workspace's collab.db while a cross-project
// one lands in the daemon-level store, and both carry the same conversation_id.
// Reporting them as two threads would overstate the number of conversations and
// understate each one's length — the opposite of what a volume view is for.
//
// limit is applied AFTER merging, so a thread split across two stores cannot be
// pushed out of the ranking by its own halves competing for a slot.
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
