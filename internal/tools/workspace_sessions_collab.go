package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// collabNoteBodyCap bounds a rendered intent or note body. This is a display
// guard for the dedicated read surface, independent of the delivery byte budget.
const collabNoteBodyCap = 240

// collabBlock renders live intents, pending inbound notes, the caller's recent
// delivery states, and conversation volume. A listing never creates either
// store. Queries are best-effort and bounded by the workspace-sessions deadline.
func (t *WorkspaceSessions) collabBlock(now time.Time) string {
	if t.collabStore == nil || t.collabPolicy == nil {
		return ""
	}
	intentsOn, mailboxOn := t.collabPolicy()
	if !intentsOn && !mailboxOn {
		return ""
	}
	store := t.collabStore()
	var global *collab.Store
	if t.collabGlobalStore != nil {
		global = t.collabGlobalStore()
	}
	if store == nil && global == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsSessionsTimeout)
	defer cancel()

	var sb strings.Builder
	if intentsOn && store != nil {
		if intents, err := store.LiveIntents(ctx, now); err == nil {
			writeCollabIntents(&sb, t.selfSessID, intents, now)
		}
	}
	if mailboxOn {
		t.writeMailboxStatus(ctx, &sb, store, global, now)
	}
	return sb.String()
}

func (t *WorkspaceSessions) writeMailboxStatus(
	ctx context.Context,
	sb *strings.Builder,
	store *collab.Store,
	global *collab.Store,
	now time.Time,
) {
	if store != nil && t.selfName != nil {
		if name := t.selfName(); name != "" {
			if notes, err := store.PendingNotesForSession(
				ctx, name, t.selfSessID, t.workspace(), now,
			); err == nil {
				writeCollabNotes(sb, notes, now)
			}
		}
	}
	writeCollabSent(sb, t.recentSentNotes(ctx, store, global, now), now)
	var localSummaries, globalSummaries []collab.ConversationSummary
	if store != nil {
		localSummaries, _ = store.ConversationSummaries(ctx, now, 5)
	}
	if global != nil && t.collabGlobalVolume != nil && t.collabGlobalVolume() {
		globalSummaries, _ = global.ConversationSummariesForWorkspace(ctx, t.workspace(), now, 5)
	}
	writeConversationVolumes(sb, collab.MergeConversationSummaries(5, localSummaries, globalSummaries), now)
}

func (t *WorkspaceSessions) recentSentNotes(
	ctx context.Context,
	store *collab.Store,
	global *collab.Store,
	now time.Time,
) []collab.Row {
	var sent []collab.Row
	if store != nil {
		sent, _ = store.RecentSentNotes(ctx, t.selfSessID, now, 5)
	}
	if global != nil {
		if rows, err := global.RecentSentNotes(ctx, t.selfSessID, now, 5); err == nil {
			sent = append(sent, rows...)
		}
	}
	sort.Slice(sent, func(i, j int) bool { return sent[i].CreatedAt.After(sent[j].CreatedAt) })
	if len(sent) > 5 {
		sent = sent[:5]
	}
	return sent
}

// writeCollabIntents renders each active session's live intent as an unverified
// claim, distinct from observed recent writes.
func writeCollabIntents(sb *strings.Builder, selfSessID string, intents []collab.Row, now time.Time) {
	if len(intents) == 0 {
		return
	}
	sb.WriteString("\npeer intents (claims, unverified — what agents SAY they are doing, not observed writes):\n")
	for _, r := range intents {
		who := r.AuthorSession
		if r.AuthorID == selfSessID {
			who += " (you)"
		}
		fmt.Fprintf(sb, "  %s — %q", who, textfmt.ClampBytes(r.Body, collabNoteBodyCap))
		if len(r.PathGlobs) > 0 {
			fmt.Fprintf(sb, " [%s]", strings.Join(r.PathGlobs, ", "))
		}
		fmt.Fprintf(sb, "  (declared %s ago)\n", humaniseAge(now.Sub(r.CreatedAt)))
	}
}

// writeCollabNotes renders only inbound note metadata without consuming or
// duplicating body content; check_messages is the canonical body-delivery surface.
func writeCollabNotes(sb *strings.Builder, notes []collab.Row, now time.Time) {
	if len(notes) == 0 {
		return
	}
	sb.WriteString("\nnotes for you (pending metadata; use check_messages to receive bodies):\n")
	for _, r := range notes {
		bytes := r.OriginalBytes
		if bytes <= 0 {
			bytes = len(r.Body)
		}
		fmt.Fprintf(sb, "  from %s — pending, %d bytes, conversation %s  (%s ago)\n",
			r.AuthorSession, bytes, r.ConversationID, humaniseAge(now.Sub(r.CreatedAt)))
	}
}

func writeCollabSent(sb *strings.Builder, notes []collab.Row, now time.Time) {
	if len(notes) == 0 {
		return
	}
	sb.WriteString("\nyour recent notes (delivery state):\n")
	for _, r := range notes {
		state := "pending"
		if !r.DeliveredAt.IsZero() {
			state = "delivered to " + r.DeliveredTo
		}
		bytes := r.OriginalBytes
		if bytes <= 0 {
			bytes = len(r.Body)
		}
		fmt.Fprintf(sb, "  %s → %s — %s, %d bytes  (%s ago)\n",
			r.ConversationID, r.Addressee, state, bytes, humaniseAge(now.Sub(r.CreatedAt)))
	}
}

func writeConversationVolumes(sb *strings.Builder, summaries []collab.ConversationSummary, now time.Time) {
	if len(summaries) == 0 {
		return
	}
	sb.WriteString("\nconversation volume (live notes; observational only):\n")
	for _, summary := range summaries {
		label := "notes"
		if summary.Notes == 1 {
			label = "note"
		}
		fmt.Fprintf(sb, "  %s — %d %s, %d pending  (last %s ago)\n",
			summary.ID, summary.Notes, label, summary.Pending, humaniseAge(now.Sub(summary.LastAt)))
	}
}
