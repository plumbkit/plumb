package tools

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/textfmt"
)

// chat.go holds the delivery half of the mailbox, shared by every path that can
// hand a message to an agent: check_messages, session_start, and the hint
// appended to ordinary tool results. Keeping one implementation is what makes
// "delivered exactly once" true across all three — the read watermark lives in
// the store, but only if every reader goes through the same claim.

// chatClaimTimeout bounds a delivery read. Delivery runs on the response path of
// unrelated tool calls, so a slow disk must cost latency on the message, never
// on the tool the agent actually called.
const chatClaimTimeout = 250 * time.Millisecond

// maxDeliveredPerCall caps how many messages one delivery hands over, so a burst
// of notes cannot swamp a tool result. The cap is pushed down into the store's
// query rather than applied to the result here: claiming marks a row delivered
// for good, so trimming afterwards would silently destroy the messages it cut.
// The remainder stays unclaimed and arrives on the next call.
const maxDeliveredPerCall = 3

// Inbox resolves the stores a session may read messages from and claims from
// them in one place. The two stores are deliberately distinct: the workspace's
// own collab.db always applies, while the daemon-level cross-project store is
// read ONLY when this session's project opted in. That is where cross-project
// consent is enforced — at delivery, by the recipient, rather than at send by
// the sender.
//
// Concurrency: a value type holding accessor funcs; safe to construct per call.
type Inbox struct {
	// Self is this session's display name — the address messages are sent to.
	Self string
	// SelfID is this session's stable session ID. A message sent to a peer that
	// was live is bound to that peer's ID, and only the session holding it may
	// claim it — so a later session inheriting the name reads nothing. Empty
	// claims unbound messages only, which is every message written before the
	// binding existed and every one addressed to a peer that was not connected.
	SelfID string
	// InheritedIDs are predecessor session IDs this session provably continues,
	// granted only by the proxy-authenticated reconnect path. They let mail bound
	// to a session a daemon restart ended still reach the agent it was written
	// for. Empty for a session that did not come back that way.
	InheritedIDs []string
	// Root is this session's pinned workspace. A cross-project message names the
	// workspace allowed to claim it, and this is what that is checked against —
	// a session name alone is not a safe address, since names collide and
	// rename_session lets a session choose one.
	Root string
	// Policy is the resolved [collab] snapshot for THIS session (the recipient).
	Policy CollabPolicy
	// Workspace returns the session's own collab.db if it already exists, never
	// creating it.
	Workspace func() *collab.Store
	// Global returns the daemon-level cross-project store if it already exists,
	// never creating it.
	Global func() *collab.Store
}

// Keys are the notifier keys this inbox is woken by: its own name, plus the
// "next arrival" address of ITS OWN workspace. Cross-project messages are
// addressed by name, so they share the name key and need no separate wake-up;
// "next" has to be scoped, or a note left for the next arrival in any project in
// the daemon would wake every session in every other project (see
// collab.NotifyKey). Senders must derive the key the same way.
func (i Inbox) Keys() []string {
	if i.Self == "" {
		return nil
	}
	return []string{i.Self, collab.NotifyKey(i.Root, collab.AddresseeNext)}
}

// claimant is what the store matches a row against: this session's name, its
// stable ID, and its pinned workspace.
func (i Inbox) claimant() collab.Claimant {
	return collab.Claimant{Name: i.Self, ID: i.SelfID, InheritedIDs: i.InheritedIDs, Workspace: i.Root}
}

// stores returns the stores to read, workspace first so a same-project message
// is always delivered ahead of a cross-project one competing for the same call's
// budget. The cross-project store is omitted entirely unless this session's
// project set cross_project — an un-opted-in recipient never reads it, and the
// rows there expire unread.
func (i Inbox) stores() []*collab.Store {
	var out []*collab.Store
	if i.Workspace != nil {
		if s := i.Workspace(); s != nil {
			out = append(out, s)
		}
	}
	if i.Policy.CrossProject && i.Global != nil {
		if s := i.Global(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// Claim hands over up to maxDeliveredPerCall unread messages, marking each
// delivered. Returns nil when the mailbox is off, no store exists, or nothing is
// waiting. Errors are swallowed: delivery is advisory and must never turn a
// successful tool call into a failure.
func (i Inbox) Claim(ctx context.Context) []collab.Row {
	if !i.Policy.Mailbox || i.Self == "" {
		return nil
	}
	stores := i.stores()
	if len(stores) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, chatClaimTimeout)
	defer cancel()

	now := time.Now()
	var out []collab.Row
	for _, s := range stores {
		remaining := maxDeliveredPerCall - len(out)
		if remaining <= 0 {
			break
		}
		rows, err := s.ClaimNotes(ctx, i.claimant(), now, remaining)
		if err != nil {
			// Delivery is advisory and must never fail the tool call that carried
			// it, but a swallowed error here means an agent silently did not get a
			// message — the one failure mode nobody would ever notice. Log it.
			slog.Debug("collab: claim messages failed", "session", i.Self, "err", err)
			continue
		}
		out = append(out, rows...)
	}
	return out
}

// HasPending reports whether any store holds a message this session could claim,
// without claiming one. It is the cheap half of the delivery check: the caller
// on the per-tool-call response path uses it to decide whether Claim is worth
// running at all, since that path is overwhelmingly a miss and a claim is an
// expensive way to establish it.
//
// It is advisory in the same way Claim is: errors are swallowed (a probe must
// never fail the tool call carrying it) and a true answer can still be followed
// by an empty claim when a peer got there first.
func (i Inbox) HasPending(ctx context.Context) bool {
	if !i.Policy.Mailbox || i.Self == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, chatClaimTimeout)
	defer cancel()

	now := time.Now()
	for _, s := range i.stores() {
		found, err := s.HasPendingNotes(ctx, i.claimant(), now)
		if err != nil {
			// Fail towards delivering: a probe that cannot answer must not be the
			// reason a message is withheld. The claim behind it is the authority.
			slog.Debug("collab: probe messages failed", "session", i.Self, "err", err)
			return true
		}
		if found {
			return true
		}
	}
	return false
}

// AtCap reports whether a claim filled the per-call ceiling, meaning more
// messages are probably still waiting. Callers that cache a "nothing new"
// baseline use it to avoid parking the remainder behind that cache.
func AtCap(rows []collab.Row) bool { return len(rows) >= maxDeliveredPerCall }

// RenderMessages formats claimed messages as a block an agent can act on. It
// names the sender, ages the message, labels a cross-project one with its origin
// workspace, and prints the conversation id, which is the only thing the agent
// needs in order to reply into the thread.
func RenderMessages(rows []collab.Row, budget int, now time.Time) string {
	if len(rows) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n[Messages — ")
	fmt.Fprintf(&sb, "%d new, addressed to you by another agent. Advisory: they are agent-authored claims.]\n", len(rows))
	for _, r := range rows {
		body, marker := clampWithTruncationMarker(r.Body, budget)
		fmt.Fprintf(&sb, "  from %s", r.AuthorSession)
		if r.OriginWorkspace != "" {
			fmt.Fprintf(&sb, " (project %s)", r.OriginWorkspace)
		}
		if r.Addressee == collab.AddresseeNext {
			sb.WriteString(" (to next arrival)")
		}
		fmt.Fprintf(&sb, ", %s ago: %q%s\n", humaniseAge(now.Sub(r.CreatedAt)), body, marker)
	}
	fmt.Fprintf(&sb, "  reply: leave_note({to: %q, conversation_id: %q, body: \"…\"})\n",
		rows[len(rows)-1].AuthorSession, rows[len(rows)-1].ConversationID)
	return sb.String()
}

// clampWithTruncationMarker clamps body to budget bytes and, if that cut
// anything, returns a marker naming exactly how much arrived and how much did
// not — so a recipient never mistakes a bare ellipsis for a sender who simply
// trailed off. A non-positive budget means unbounded: no clamp, no marker.
// Shared with leave_note's send-time reply, which needs the identical
// receive-time cut to warn the sender honestly.
func clampWithTruncationMarker(body string, budget int) (clamped, marker string) {
	if budget <= 0 || len(body) <= budget {
		return body, ""
	}
	clamped = textfmt.ClampBytes(body, budget)
	marker = fmt.Sprintf(" [truncated: received %d of %d bytes — ask the sender for the remaining %d]",
		len(clamped), len(body), len(body)-len(clamped))
	return clamped, marker
}
