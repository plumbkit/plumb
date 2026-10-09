package tools

import (
	"context"
	"encoding/json"
	"fmt"
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

// mailMode is what session_start does with the mail waiting for this caller,
// resolved from the optional `mail` argument. The zero value is not a mode: an
// absent argument resolves to mailClaim, so a client that passes nothing keeps
// the delivery behaviour it has always had.
type mailMode string

const (
	// mailClaim delivers waiting notes in the packet and spends the watermark.
	// It is the default, and the compatible answer: session_start returns a
	// result the caller asked for, so what it carries has provably arrived.
	mailClaim mailMode = "claim"
	// mailPreview shows what is waiting and claims none of it, so check_messages
	// still delivers every note afterwards. It exists for the clients that call
	// session_start automatically — a hook, or a plugin that does it on nearly
	// every connection — where the packet may be summarised, truncated or
	// discarded by the client before the model reads a word of it, and a claim
	// taken there spends notes nobody ever saw. Same Peek as the block appended
	// to ordinary tool results; this is a second PLACEMENT of it, not a second
	// read path.
	mailPreview mailMode = "preview"
)

// resolveMailMode validates the optional `mail` argument. An unrecognised value
// is refused rather than folded into the default: silently claiming, or silently
// previewing, would answer a caller that asked for something else — and in the
// claiming direction it would consume mail the caller had deliberately asked to
// leave alone. A malformed arguments object is refused with the same message
// resolveDetail uses.
func resolveMailMode(raw json.RawMessage) (mailMode, error) {
	var a struct {
		Mail string `json:"mail"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return "", fmt.Errorf("session_start: invalid arguments: %w", err)
	}
	switch mode := mailMode(a.Mail); mode {
	case "":
		return mailClaim, nil
	case mailClaim, mailPreview:
		return mode, nil
	default:
		return "", fmt.Errorf("session_start: mail must be %q or %q, got %q", mailClaim, mailPreview, a.Mail)
	}
}

// writeSessionMessages appends a "## Messages" block when [collab] mailbox is on
// and messages await this session. Bailing before the read when the feature is
// off or no store exists guarantees a collab.db is never created by a read path.
//
// mail selects the two shapes the block comes in: a claim that delivers, or a
// preview that leaves every note exactly where it was. The gate below is shared
// by both, and deliberately so — a caller check_messages would refuse must not
// be shown notes it cannot be shown to own, whether or not the read marks them
// read. See SessionStart.mailClaimable.
func (t *SessionStart) writeSessionMessages(sb *strings.Builder, _ string, claimable bool, mail mailMode) {
	if t.mailboxFn == nil {
		return
	}
	// An unattributable caller on a shared connection must not consume mail it
	// cannot be shown to own — the same rule check_messages enforces.
	if !claimable {
		return
	}
	on, inbox := t.mailboxFn()
	if !on || inbox.Self == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wsSessionsTimeout)
	defer cancel()
	var body string
	if mail == mailPreview {
		body = previewMessages(ctx, inbox)
	} else {
		body = claimedMessages(ctx, inbox)
	}
	if body == "" {
		return
	}
	sb.WriteString("\n## Messages\n")
	sb.WriteString(body)
	sb.WriteString("\n")
}

// claimedMessages delivers what is waiting, exactly once. The claim is the whole
// difference from previewMessages: after this returns, check_messages has nothing
// left to hand over for these notes.
func claimedMessages(ctx context.Context, inbox Inbox) string {
	rows := inbox.Claim(ctx)
	if len(rows) == 0 {
		return ""
	}
	body := RenderMessages(rows, inbox.Policy.ChatBudget(), time.Now())
	if AtCap(rows) {
		body += RenderBacklog(inbox.PendingCount(ctx))
	}
	return body
}

// previewMessages shows what is waiting without claiming any of it, so every
// note it displays is still delivered by check_messages. Peek applies the far
// looser maxPeeked bound rather than the per-call delivery cap, so the cap is
// applied here — trimming a previewed row destroys nothing — and the remainder
// is stated as a count instead of a second query.
func previewMessages(ctx context.Context, inbox Inbox) string {
	rows := PreviewRows(inbox.Peek(ctx))
	if len(rows) == 0 {
		return ""
	}
	more := 0
	if len(rows) > MaxPreviewedPerCall {
		more, rows = len(rows)-MaxPreviewedPerCall, rows[:MaxPreviewedPerCall]
	}
	return RenderMessagePreview(rows, inbox.Policy.ChatBudget(), time.Now()) + RenderBacklog(more)
}
