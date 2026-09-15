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
// show a message to an agent: check_messages, session_start, and the block
// appended to ordinary tool results.
//
// Those three are NOT equivalent, and treating them as if they were is what
// lost messages. Only two of them are channels the model provably sees:
// check_messages and session_start return a result the model asked for, so what
// plumb writes there arrives with it. The block appended to an unrelated tool's
// result is a different thing — a bet that the client surfaces text the model
// never requested. Most clients do. A harness that runs plumb's tools inside a
// sandboxed program does not: in DSH the agent's only callable tool is
// `run_code`, plumb's tools are called from inside the program, and the runtime
// surfaces to the model ONLY what that program prints or returns. Every inner
// result, plumb's block included, is discarded.
//
// So the read watermark is set by the two channels that can prove delivery, and
// the appended block is a PREVIEW that claims nothing (Peek, not Claim). That is
// what makes "delivered exactly once" a fact rather than a hope: a message can
// now be shown early and still be waiting in check_messages, but it can never be
// marked read by a result nobody read.

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

// MaxPreviewedPerCall is the same ceiling for the connection-side preview, which
// has to apply it in Go rather than in the statement: the preview must first drop
// the notes it has already shown, and only then take the first few of what is
// left. Trimming is safe there for the reason it is not safe in Claim — a peeked
// row is not consumed, so anything cut stays claimable.
const MaxPreviewedPerCall = maxDeliveredPerCall

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
//
// Under [collab] keep_delivered_notes the claim is the permanence trigger too:
// it stamps each delivered row kept-forever. That is the RECIPIENT's policy —
// the recipient is the one reading — so a cross-project note's retention is
// decided by whichever project claims it, never by the sender's setting.
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
		claim := s.ClaimNotes
		if i.Policy.KeepDeliveredNotes {
			claim = s.ClaimNotesKeeping
		}
		rows, err := claim(ctx, i.claimant(), now, remaining)
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

// Preview is one peeked note together with a key identifying it across the two
// stores an inbox reads. Row IDs are per-store autoincrements, so an ID alone
// collides between the workspace's collab.db and the daemon-level cross-project
// store; the key carries the store's position alongside it.
type Preview struct {
	Row collab.Row
	Key string
}

// PreviewRows extracts the rows for rendering.
func PreviewRows(ps []Preview) []collab.Row {
	rows := make([]collab.Row, len(ps))
	for i, p := range ps {
		rows[i] = p.Row
	}
	return rows
}

// maxPeeked bounds one peek. The preview shows at most MaxPreviewedPerCall
// bodies and states the rest as a count, so reading further buys nothing but a
// larger sort — and this read repeats on the periodic backstop for as long as a
// note sits unclaimed, which is the one place an unbounded scan could become a
// standing cost rather than a one-off. Well above the display cap so the backlog
// number stays exact in every realistic case.
const maxPeeked = 64

// Peek returns every note this session could claim right now, marking NOTHING
// delivered. It is the read behind the preview appended to an ordinary tool
// result, and the difference between it and Claim is the whole point: a preview
// rides on a result the client may never show the model, so it must not be what
// sets the read watermark. See the package comment at the top of this file.
//
// It carries no cap, unlike Claim. The caller has to drop the notes it has
// already previewed BEFORE taking its few, or a fourth message arriving behind
// three already-shown ones would sit behind a cap that keeps re-spending itself
// on the same three. Returning everything claimable is also what lets the caller
// state the backlog from what it already has, rather than paying for a second
// counting query.
//
// Advisory in the same way Claim is: errors are swallowed, since a preview must
// never turn a successful tool call into a failure.
func (i Inbox) Peek(ctx context.Context) []Preview {
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
	var out []Preview
	for idx, s := range stores {
		rows, err := s.ClaimableNotes(ctx, i.claimant(), now, maxPeeked-len(out))
		if err != nil {
			// Same reasoning as Claim's swallowed error: a preview must not fail the
			// call it rides on. Unlike Claim, nothing is lost when this one fails —
			// the note stays claimable and check_messages still delivers it.
			slog.Debug("collab: peek messages failed", "session", i.Self, "err", err)
			continue
		}
		for _, r := range rows {
			out = append(out, Preview{Row: r, Key: fmt.Sprintf("%d:%d", idx, r.ID)})
		}
		if len(out) >= maxPeeked {
			break
		}
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

// PendingCount reports how many notes remain claimable across this inbox's
// stores, claiming nothing — the source of the backlog line a capped delivery
// renders. Same gates and the same swallowed errors as Claim: a count is
// advisory too. Zero from an empty mailbox and zero from a failed count are
// indistinguishable to the caller, which is acceptable — the line only rides a
// delivery that just filled its cap, so the worst a swallowed error does is
// understate a backlog the next delivery will surface anyway.
func (i Inbox) PendingCount(ctx context.Context) int {
	if !i.Policy.Mailbox || i.Self == "" {
		return 0
	}
	stores := i.stores()
	if len(stores) == 0 {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, chatClaimTimeout)
	defer cancel()

	now := time.Now()
	total := 0
	for _, s := range stores {
		n, err := s.PendingCount(ctx, i.claimant(), now)
		if err != nil {
			slog.Debug("collab: count pending failed", "session", i.Self, "err", err)
			continue
		}
		total += n
	}
	return total
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
	return renderMessages(rows, budget, now, false)
}

// RenderMessagePreview formats the same messages as an early look that claimed
// nothing. It differs from RenderMessages in the two things that are no longer
// true of it: the messages are WAITING rather than handed over, and the reply
// handle is withheld.
//
// Withholding it is deliberate. Replying straight off a preview would leave the
// note unread — the sender would go on seeing it in their unread outbox while
// holding the answer to it — so the one action offered here is the one that
// takes delivery. check_messages prints the reply handle, which is where an
// agent that means to answer was always going to end up.
func RenderMessagePreview(rows []collab.Row, budget int, now time.Time) string {
	return renderMessages(rows, budget, now, true)
}

func renderMessages(rows []collab.Row, budget int, now time.Time, preview bool) string {
	if len(rows) == 0 {
		return ""
	}
	state := "new"
	if preview {
		state = "waiting"
	}
	var sb strings.Builder
	sb.WriteString("\n\n[Messages — ")
	fmt.Fprintf(&sb, "%d %s, addressed to you by another agent. Advisory: they are agent-authored claims.]\n", len(rows), state)
	for _, r := range rows {
		body, marker, _ := clampWithTruncationMarker(r.Body, budget)
		fmt.Fprintf(&sb, "  from %s", r.AuthorSession)
		if r.OriginWorkspace != "" {
			fmt.Fprintf(&sb, " (project %s)", r.OriginWorkspace)
		}
		if r.Addressee == collab.AddresseeNext {
			sb.WriteString(" (to next arrival)")
		}
		fmt.Fprintf(&sb, ", %s ago: %q%s\n", humaniseAge(now.Sub(r.CreatedAt)), body, marker)
	}
	if preview {
		sb.WriteString("  Preview: shown early on this result and NOT marked read. " +
			"Call check_messages to take delivery and get the reply handle.\n")
		return sb.String()
	}
	fmt.Fprintf(&sb, "  reply: leave_note({to: %q, conversation_id: %q, body: \"…\"})\n",
		rows[len(rows)-1].AuthorSession, rows[len(rows)-1].ConversationID)
	return sb.String()
}

// RenderNextWaiting reports that notes addressed to "whoever attaches next" are
// waiting, without showing them. Empty for zero, so the common case is silent.
//
// A count is all a preview may honestly give for these. Every session here is a
// candidate and the atomic claim picks one, so a body shown on this path could
// be acted on twice — see splitNextNotes in internal/cli/conn_chat.go. Saying
// one is waiting costs nothing and still points at the call that resolves it.
func RenderNextWaiting(n int) string {
	if n <= 0 {
		return ""
	}
	noun := "message"
	if n > 1 {
		noun = "messages"
	}
	return fmt.Sprintf("  %d %s left for whoever attaches next — not shown here, because "+
		"any session in this workspace could be the one that gets it. call check_messages "+
		"to try to claim it.\n", n, noun)
}

// RenderBacklog states what a capped delivery left behind: how many notes were
// still claimable the moment the batch filled the per-call cap. It points the
// agent at an immediate drain rather than at patience — the next tool call
// WOULD deliver the remainder, but an agent about to reply to message one of
// seven should read the other six first, and the newest may answer the oldest.
// The count is a snapshot, not a promise, and the wording keeps saying so.
func RenderBacklog(waiting int) string {
	if waiting <= 0 {
		return ""
	}
	return fmt.Sprintf("  %d more waiting as of this call — call check_messages (no wait_seconds) to read them now, before replying; the newest may answer the oldest.\n", waiting)
}

// clampWithTruncationMarker clamps body to budget bytes and, if that cut
// anything, returns a marker naming exactly how much arrived and how much did
// not — so a recipient never mistakes a bare ellipsis for a sender who simply
// trailed off. A non-positive budget means unbounded: no clamp, no marker.
// Shared with leave_note's send-time reply, which needs the identical
// receive-time cut to warn the sender honestly.
//
// kept counts bytes of BODY, which is strictly less than len(clamped) whenever
// the clamp appended its ellipsis: that marker spends budget without carrying
// any of the sender's text. Counting it as delivered would overstate the cut
// and send the reader — or the sender following the remedy — to an offset that
// silently skips the bytes it covers.
func clampWithTruncationMarker(body string, budget int) (clamped, marker string, kept int) {
	if budget <= 0 || len(body) <= budget {
		return body, "", len(body)
	}
	clamped, kept = textfmt.ClampBytesKept(body, budget)
	marker = fmt.Sprintf(" [truncated: received %d of %d bytes — ask the sender for the remaining %d]",
		kept, len(body), len(body)-kept)
	return clamped, marker, kept
}
