package tools

import (
	"context"
	"strings"
	"time"
)

// session_start_mailbox.go delivers waiting messages in the orientation packet
// ([collab] mailbox): notes addressed to this session by name, and notes left
// for "next" (whoever attaches to this workspace next). Delivery is polling
// only; plumb does not push — a fact about plumb, which wires no server→client
// wake path, not about MCP, where some clients offer one.
//
// It shares the Inbox claim with check_messages — the two tools that DELIVER,
// because each returns a result the model asked for. The block appended to other
// tool results previews without claiming, so the watermark is spent here or in
// check_messages and nowhere else, which is what makes "delivered exactly once"
// hold rather than merely being asserted.
//
// Of the two this is the weaker channel, and the difference is worth knowing.
// check_messages returns the mailbox and nothing else; this section sits near
// the tail of a long orientation packet, so a client that summarises or truncates
// that packet can drop it after the claim. If that is ever observed, the answer
// is the same one taken for the tool-result block: preview here and let
// check_messages deliver. It has not been observed, and a session_start whose
// packet is being dropped has larger problems than its mail.
//
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
	body := RenderMessages(rows, inbox.Policy.ChatBudget(), time.Now())
	if AtCap(rows) {
		body += RenderBacklog(inbox.PendingCount(ctx))
	}
	sb.WriteString(body)
	sb.WriteString("\n")
}
