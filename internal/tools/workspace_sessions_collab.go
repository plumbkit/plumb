package tools

// workspace_sessions_collab.go holds the two OBSERVATIONAL mailbox sections of
// the workspace_sessions listing: what this session has sent and whether it
// landed, and how much traffic each live conversation is carrying.
//
// Split from workspace_sessions.go by subject rather than by size: everything
// there answers "who else is here and what did they touch", while these two
// answer "what is the mailbox doing" — a different source, a different failure
// mode (a missing collab.db is normal), and a different disclosure rule.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// collabSentCap and collabVolumeCap bound each section. A listing is read by a
// human scanning for something wrong; an unbounded list of every note ever sent
// buries the one row that matters.
const (
	collabSentCap   = 8
	collabVolumeCap = 8
)

// writeCollabSent renders the caller's own recent notes and whether each has
// been read yet.
//
// This is the only place a sender can see delivery state alongside the rest of
// the workspace picture. check_messages already reports unread sent mail, but
// only when called, and only the failures — so a session that never calls it
// learns nothing, and a session that does cannot tell "no unread" from "sent
// nothing". Showing delivered rows too is what separates those.
//
// Bodies are deliberately NOT rendered. The notes-for-you section above prints
// bodies because those messages were addressed to this session; these were
// addressed to someone else, and a sender's own outbox is not a licence to
// re-display content in a surface a human may screenshot or paste. The metadata
// answers the question the section exists for.
func writeCollabSent(sb *strings.Builder, sent []collab.Row, now time.Time) {
	if len(sent) == 0 {
		return
	}
	sb.WriteString("\nyour recent notes (delivery state; bodies omitted):\n")
	for _, r := range sent {
		state := "pending"
		if !r.DeliveredAt.IsZero() {
			to := r.DeliveredTo
			if to == "" {
				to = "a peer"
			}
			state = "delivered to " + to
		}
		fmt.Fprintf(sb, "  to %s — %s, %d bytes", addresseeLabel(r), state, len(r.Body))
		if r.ConversationID != "" {
			fmt.Fprintf(sb, ", conversation %s", r.ConversationID)
		}
		fmt.Fprintf(sb, "  (%s ago)\n", humaniseAge(now.Sub(r.CreatedAt)))
	}
	sb.WriteString("  \"pending\" means nobody has claimed it yet — delivery is polling-only, " +
		"so a peer idle on its human has not seen it. It is not a refusal.\n")
}

// addresseeLabel names where a note went, without implying more precision than
// the row carries. A bound note names the session it belongs to; an unbound one
// names only what the sender typed, because that is genuinely all that is known.
func addresseeLabel(r collab.Row) string {
	if r.Addressee == collab.AddresseeNext {
		return `"next" (whoever attaches next)`
	}
	if r.AddresseeID == "" {
		return r.Addressee + " (by name; not bound to a live session)"
	}
	return r.Addressee
}

// writeConversationVolumes renders note volume per live conversation.
//
// This is the replacement for enforcement, and the reason it is worth building:
// the exchange cap answers "are these agents talking a lot?" by SEVERING the
// conversation after the fact, destroying whatever was being composed. Counting
// answers the same question without destroying anything — a human who can see a
// thread growing can intervene, and a legitimate exchange pays nothing.
//
// Labelled observational so nobody reads it as a control. Nothing here throttles
// or refuses; it is a number on a screen.
func writeConversationVolumes(sb *strings.Builder, vols []collab.ConversationSummary, now time.Time) {
	if len(vols) == 0 {
		return
	}
	sb.WriteString("\nconversation volume (live notes; observational only — nothing here throttles anything):\n")
	for _, v := range vols {
		fmt.Fprintf(sb, "  %s — %d note(s)", v.ID, v.Notes)
		if v.Pending > 0 {
			fmt.Fprintf(sb, ", %d unread", v.Pending)
		}
		fmt.Fprintf(sb, ", last %s ago\n", humaniseAge(now.Sub(v.LastAt)))
	}
}

// collabObservations gathers both sections' data.
//
// CROSS-PROJECT METADATA IS SHOWN ONLY TO THE RECIPIENT. The daemon-level store
// holds traffic between every project on the machine, and this listing runs
// inside one of them. It is queried with ConversationSummariesForWorkspace,
// scoped to THIS workspace as the target, and only when this workspace has
// opted in with [collab] cross_project.
//
// The consent that matters is the RECIPIENT's, not the sender's. A sender
// choosing to write across projects cannot consent on the recipient's behalf to
// that traffic being visible inside the recipient's project — the recipient is
// the one whose screen it appears on, and whose human is being told that
// another project is talking to theirs. The same rule already governs whether
// the message itself is delivered; this keeps the metadata from becoming a way
// around it.
//
// The sent list needs no such gate: those rows are the caller's own, scoped by
// author_id, so reading them across stores discloses nothing the caller did not
// write.
func (t *WorkspaceSessions) collabObservations(
	ctx context.Context, store *collab.Store, now time.Time,
) (sent []collab.Row, vols []collab.ConversationSummary) {
	if t.selfSessID != "" {
		if rows, err := store.SentBy(ctx, t.selfSessID, now, collabSentCap); err == nil {
			sent = rows
		}
		if g := t.globalStoreIfExists(); g != nil {
			if rows, err := g.SentBy(ctx, t.selfSessID, now, collabSentCap); err == nil {
				sent = append(sent, rows...)
			}
		}
	}

	local, err := store.ConversationSummaries(ctx, now, collabVolumeCap)
	if err != nil {
		local = nil
	}
	var global []collab.ConversationSummary
	if t.crossProjectOn() {
		if g := t.globalStoreIfExists(); g != nil {
			if rows, gErr := g.ConversationSummariesForWorkspace(
				ctx, t.workspace(), now, collabVolumeCap,
			); gErr == nil {
				global = rows
			}
		}
	}
	// Merged rather than concatenated: one thread can hold notes in both stores
	// under the same id, and listing it twice would overstate the number of
	// conversations while understating each one's length.
	vols = collab.MergeConversationSummaries(collabVolumeCap, local, global)
	return sent, vols
}

// globalStoreIfExists returns the daemon-level store only when it already
// exists, so a listing never brings one into being.
func (t *WorkspaceSessions) globalStoreIfExists() *collab.Store {
	if t.collabGlobalStore == nil {
		return nil
	}
	return t.collabGlobalStore()
}

// crossProjectOn reports this workspace's [collab] cross_project consent.
// Absent wiring is treated as OFF, so a caller that forgets to wire it discloses
// nothing rather than everything.
func (t *WorkspaceSessions) crossProjectOn() bool {
	return t.collabCrossProject != nil && t.collabCrossProject()
}
