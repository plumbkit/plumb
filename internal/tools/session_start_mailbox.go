package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// session_start_mailbox.go delivers the phase-2 minimal mailbox ([collab]
// mailbox): notes addressed to this session, and notes left for "next" (whoever
// attaches to this workspace next) — the latter consumed on delivery. Delivery
// is polling only; plumb cannot push. Agent-authored, so notes are rendered as
// received messages, distinct from the observed peer digest above.

// maxDeliveredNotes caps how many notes one session_start renders, so a flood of
// notes cannot dominate the orientation packet. Each note body is additionally
// byte-bounded by the [collab] hint budget (UTF-8 boundary), honouring the
// injected-signal budget invariant while never truncating mid-message across
// notes.
const maxDeliveredNotes = 10

// writeSessionMessages appends a "## Messages" block when [collab] mailbox is on
// and notes await this session. It consumes "next" notes (delivered once). Bailing
// before DeliverNotes when the feature is off or no store exists guarantees a
// collab.db is never created by the read path.
func (t *SessionStart) writeSessionMessages(sb *strings.Builder, _ string) {
	if t.mailboxFn == nil {
		return
	}
	on, store, self, budget := t.mailboxFn()
	if !on || store == nil || self == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsSessionsTimeout)
	defer cancel()
	notes, err := store.DeliverNotes(ctx, self, time.Now())
	if err != nil || len(notes) == 0 {
		return
	}
	sb.WriteString(formatDeliveredNotes(notes, budget, time.Now()))
}

// formatDeliveredNotes renders the delivered notes as a bounded messages block.
// Pure function. A "next" note is flagged so the reader knows it was addressed to
// whoever attached first (i.e. them), not to their session by name.
func formatDeliveredNotes(notes []collab.Row, budget int, now time.Time) string {
	var sb strings.Builder
	sb.WriteString("\n## Messages\n\n")
	sb.WriteString("Notes left for you by peers (agent-authored; advisory):\n")
	shown := notes
	if len(shown) > maxDeliveredNotes {
		shown = shown[:maxDeliveredNotes]
	}
	for _, r := range shown {
		body := r.Body
		if budget > 0 {
			body = clampToBytes(body, budget)
		}
		fmt.Fprintf(&sb, "- from %s", r.AuthorSession)
		if r.Addressee == collab.AddresseeNext {
			sb.WriteString(" (to next arrival)")
		}
		fmt.Fprintf(&sb, ", %s ago: \"%s\"\n", humaniseAge(now.Sub(r.CreatedAt)), body)
	}
	if len(notes) > maxDeliveredNotes {
		fmt.Fprintf(&sb, "- (+%d more)\n", len(notes)-maxDeliveredNotes)
	}
	sb.WriteString("\n")
	return sb.String()
}
