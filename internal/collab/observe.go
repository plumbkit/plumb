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

// ConversationSummaries counts the live conversations WHO takes part in, busiest
// first. limit caps the result (non-positive means no cap).
//
// It used to count every thread in the store, which meant the listing printed
// the id of exchanges between two other agents to a session with no part in
// them. The id is not decoration: a thread is addressed by it, so publishing it
// to an uninvolved session handed out the routing token for someone else's
// conversation. PutNote's participantGuard is what makes that harmless now, and
// this is the second half of the same rule — a session should not be able to
// enumerate exchanges it is not in, whether or not it could act on them.
func (s *Store) ConversationSummaries(
	ctx context.Context, who Claimant, now time.Time, limit int,
) ([]ConversationSummary, error) {
	return s.conversationSummaries(ctx, now, limit, "", &who)
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
//
// Note what it does NOT do: unlike ConversationSummaries it is not scoped to the
// caller's own threads, so every cross-project note aimed at this workspace is
// counted, including exchanges between two other sessions here. That is
// deliberate — the question it answers is "how much traffic is aimed at this
// project", a property of the project rather than of one session in it — and it
// is why the caller must gate it on the workspace's own cross_project consent.
func (s *Store) ConversationSummariesForWorkspace(
	ctx context.Context, workspace string, now time.Time, limit int,
) ([]ConversationSummary, error) {
	if s == nil || workspace == "" || !s.IsGlobal() {
		return nil, nil
	}
	return s.conversationSummaries(ctx, now, limit, workspace, nil)
}

// conversationSummaries aggregates counts, optionally narrowed to a target
// workspace and/or to the threads one claimant takes part in.
func (s *Store) conversationSummaries(
	ctx context.Context, now time.Time, limit int, workspace string, who *Claimant,
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
	if who != nil {
		// Membership is a property of the THREAD, not of the row, so this selects
		// conversations containing at least one row the claimant is in — not only
		// those rows. Counting only the caller's own rows would report a two-sided
		// exchange as half its real length, which is the opposite of what a volume
		// view is for. Names are admitted alongside identities for the same reason
		// participantGuard admits them: an unbound row addressed by name is the
		// common case.
		// The same predicate the write guard uses, so what a session may SEE and
		// what it may WRITE INTO cannot drift apart — including its inherited
		// identities, and including the delivered_to arm that is a "next" recipient's
		// only evidence of membership. An anonymous claimant yields `0 = 1` and
		// therefore sees nothing, which is the direction to fail in.
		//
		// No expiry filter on the subquery, matching participantGuard: membership is
		// historical, so a live thread whose opening notes have aged out is still the
		// caller's. The OUTER query already restricts what is COUNTED to unexpired
		// rows.
		member, memberArgs := membershipPredicate(who.identities(), who.Name)
		where += ` AND conversation_id IN (
		              SELECT conversation_id FROM collab_rows
		              WHERE kind = ? AND ` + member + `)`
		args = append(args, string(KindNote))
		args = append(args, memberArgs...)
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

// ConversationWorkspaces returns the distinct origin/target workspaces that
// have appeared in a conversation's unexpired rows. Read-only; discloses no
// message body or participant identity, only which projects were involved —
// the minimum needed to evaluate daemon-wide display consent.
//
// An UNSTAMPED workspace is reported as an empty string rather than filtered
// away, and that is the whole point of the method. Every row in the global
// store is cross-project by construction — PutNote refuses one without a
// target workspace — so each row always has a sender somewhere, and an empty
// origin_workspace can only ever mean "this participant could not be placed",
// never "this row has no sender". Dropping it would silently downgrade an
// unresolvable participant to a non-participant, and a conversation would then
// be displayed on the strength of the participants that DID resolve: exactly
// the "any one consents" rule FilterDaemonWideConversations exists to refuse.
// Reporting "" instead pushes the decision through the caller's own allow
// func, which fails closed on a workspace it cannot resolve (see
// config.TargetAllowsCrossProject).
//
// Only meaningful on the global store, and it returns nothing on any other —
// the workspace columns are stamped only on cross-project rows, so a project's
// own collab.db would report every conversation as one unplaceable
// participant. Mirrors ConversationSummariesForWorkspace's contract.
func (s *Store) ConversationWorkspaces(ctx context.Context, convID string, now time.Time) ([]string, error) {
	if s == nil || s.db == nil || convID == "" || !s.IsGlobal() {
		return nil, nil
	}
	// UNION (not UNION ALL) dedupes origin and target across both halves of the
	// query, so a conversation with several notes between the same two
	// workspaces still reports exactly two — and several rows that all failed to
	// stamp an origin collapse to a single "".
	rows, err := s.db.QueryContext(ctx,
		`SELECT origin_workspace FROM collab_rows
		  WHERE kind = ? AND conversation_id = ? AND expires_at > ?
		 UNION
		 SELECT target_workspace FROM collab_rows
		  WHERE kind = ? AND conversation_id = ? AND expires_at > ?`,
		string(KindNote), convID, now.UnixNano(),
		string(KindNote), convID, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("collab: conversation workspaces: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var ws string
		if err := rows.Scan(&ws); err != nil {
			return nil, fmt.Errorf("collab: scan conversation workspace: %w", err)
		}
		out = append(out, ws)
	}
	return out, rows.Err()
}

// daemonWideOverfetch is how much larger a FilterDaemonWideConversations query
// asks for than the caller's display limit. Filtering removes rows, so asking
// for exactly `limit` and then discarding some would under-fill the daemon-wide
// view even when enough consented conversations exist to fill it.
const daemonWideOverfetch = 4

// FilterDaemonWideConversations returns live conversations from this
// (necessarily global) store, trimmed to those where EVERY participating
// workspace satisfies allow — the unanimous-consent rule for a daemon-wide
// display (the TUI and web dashboards) that has no single recipient to ask
// consent of, unlike ConversationSummariesForWorkspace's one-recipient
// question. "Any one workspace consents" would let project A's opt-in expose
// project B's traffic without B's consent; "none at all" would make the
// feature pointless. Only unanimous consent satisfies both projects at once.
//
// Fail closed on every shape of "cannot determine consent", which is three
// distinct cases and not just the obvious one: ConversationWorkspaces errors,
// or it returns no participant at all, or it returns a participant that could
// not be placed in a workspace (an empty origin — reported as "" precisely so
// it reaches allow, which refuses it). The last is the one that matters: a
// partially-resolvable conversation must be refused like an unresolvable one,
// because "show it if the participants we could place all consent" IS the
// any-one-consents rule, arrived at sideways.
//
// limit caps the RETURNED count; the underlying query over-fetches by
// daemonWideOverfetch so filtering does not silently under-fill the view.
// Non-positive limit means no cap on either the fetch or the result.
func (s *Store) FilterDaemonWideConversations(
	ctx context.Context, now time.Time, limit int, allow func(workspace string) bool,
) ([]ConversationSummary, error) {
	if s == nil || s.db == nil || !s.IsGlobal() || allow == nil {
		return nil, nil
	}
	fetch := limit
	if fetch > 0 {
		fetch *= daemonWideOverfetch
	}
	// Unscoped on purpose — the exported ConversationSummaries is narrowed to one
	// session's own threads, which is the wrong question here. This view is the
	// daemon operator's, and its access rule is the unanimous-consent filter below
	// rather than session membership.
	all, err := s.conversationSummaries(ctx, now, fetch, "", nil)
	if err != nil {
		return nil, err
	}
	out := make([]ConversationSummary, 0, len(all))
	for _, sum := range all {
		workspaces, wErr := s.ConversationWorkspaces(ctx, sum.ID, now)
		if wErr != nil || len(workspaces) == 0 {
			continue
		}
		consented := true
		for _, ws := range workspaces {
			if !allow(ws) {
				consented = false
				break
			}
		}
		if !consented {
			continue
		}
		out = append(out, sum)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
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
