package tools

import (
	"context"
	"strings"
	"time"
)

// session_start_mailbox.go delivers waiting messages in the orientation packet
// ([collab] mailbox): notes addressed to this session by name, and notes left
// for "next" (whoever attaches to this workspace next). Delivery is polling
// only; plumb cannot push.
//
// It shares the Inbox claim with check_messages and the tool-result block, which
// creates one at-most-once atomic claim across all three: the read watermark
// lives in the store, and every reader goes through the same claim. A transport
// failure after response construction is outside that server-side guarantee.
// Messages are agent-authored, so they render as received messages, distinct
// from the daemon-observed peer digest above them.

// writeSessionMessages appends a "## Messages" block when [collab] mailbox is on
// and messages await this session. Bailing before the claim when the feature is
// off or no store exists guarantees a collab.db is never created by a read path.
func (t *SessionStart) writeSessionMessages(sb *strings.Builder, _ string) {
	if t.mailboxFn == nil {
		return
	}
	on, inbox := t.mailboxFn()
	if !on || inbox.Self == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsSessionsTimeout)
	defer cancel()
	rows := inbox.Claim(ctx)
	if len(rows) == 0 {
		return
	}
	sb.WriteString("\n## Messages\n")
	sb.WriteString(RenderMessages(rows, inbox.Policy.ChatBudget(), time.Now()))
	sb.WriteString("\n")
}
