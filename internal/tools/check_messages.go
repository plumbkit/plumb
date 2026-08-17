package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// CheckMessages is the check_messages MCP tool: the receive half of plumb's
// mailbox, and the reason two agents can hold a conversation rather than trade
// one-way notes.
//
// Messages already ride along on ordinary tool results, which is enough for an
// agent that is busy working. It is not enough for turn-taking: after sending a
// question you have nothing to do until the answer comes, and polling for it
// burns a tool call per attempt. This tool lets a session hand its turn over —
// parking server-side on the daemon's in-process notifier until a message
// arrives or the wait expires — so a back-and-forth costs one call per turn.
//
// Concurrency: Execute is safe for concurrent use; the wait holds no lock.
type CheckMessages struct{ deps CollabDeps }

// NewCheckMessages constructs the check_messages tool.
func NewCheckMessages(deps CollabDeps) *CheckMessages { return &CheckMessages{deps: deps} }

func (*CheckMessages) Name() string { return "check_messages" }

func (*CheckMessages) Description() string {
	return "Read messages other agents have sent you, optionally waiting for one to " +
		"arrive. The receive half of plumb's mailbox; leave_note is the send half.\n\n" +
		"Omit wait_seconds (or 0) to return immediately with whatever is waiting. A " +
		"positive wait_seconds BLOCKS until a message arrives or the wait expires — " +
		"this is how you hand your turn to a peer after asking something, instead of " +
		"polling. It is capped by [collab] max_wait_seconds, kept below the client's " +
		"own call timeout.\n\n" +
		"Each message is delivered exactly ONCE, to whichever path sees it first — " +
		"this tool, the block appended to any tool result, or session_start. " +
		"Re-calling will not redeliver it, so act on a message when you read it.\n\n" +
		"Every message carries a conversation_id; quote it in leave_note to reply in " +
		"thread. A THREAD is capped at [collab] max_exchanges — it bounds one " +
		"conversation, and a new one starts a fresh budget, so it is a speed bump, " +
		"not an enforced limit on how long two agents may talk. When a thread is " +
		"spent, surface it to your human rather than routing around the cap.\n\n" +
		"Messages are addressed to a SESSION, not a name: one written while you were " +
		"connected is readable only by this session, so a later session taking your " +
		"name cannot read your mail (nor you your predecessor's).\n\n" +
		"IT ALSO REPORTS YOUR OWN UNREAD MAIL. Any message you sent that nobody has " +
		"read yet is listed back, with its age — the one thing you cannot otherwise " +
		"observe: plumb does not push, so \"no reply\" means either the peer read it " +
		"and has not answered or never read it, and only this tells them apart. " +
		"Listing is a read; it never consumes the message on the recipient's " +
		"behalf.\n\n" +
		"Requires [collab] mailbox = true. Messages from a session in another " +
		"workspace are shown only when this project sets [collab] cross_project = " +
		"true, and are labelled with the sending project. Etiquette: the plumb-chat " +
		"skill.\n\n" +
		"Parameters:\n" +
		"  wait_seconds — block up to this long for a message (default 0, no wait)."
}

func (*CheckMessages) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "wait_seconds": {
      "type": "integer",
      "minimum": 0,
      "description": "Block up to this many seconds waiting for a message. 0 (default) returns immediately. Capped by [collab] max_wait_seconds."
    }
  },
  "additionalProperties": false
}`)
}

type checkMessagesArgs struct {
	WaitSeconds int `json:"wait_seconds"`
}

func (t *CheckMessages) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var args checkMessagesArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("check_messages: %w", err)
		}
	}
	policy := t.deps.Policy()
	if !policy.Mailbox {
		return "check_messages is disabled — set [collab] mailbox = true (globally or in " +
			"this workspace's .plumb/config.toml) to exchange messages with peers.", nil
	}
	self := t.deps.SessionName()
	if self == "" {
		return "workspace not yet attached — call session_start first", nil
	}
	// A session whose registration failed keeps a display name for the TUI and
	// logs, but that name was drawn without a uniqueness check and sits in no
	// file any peer's check can see, so it may shadow a live session. Claiming is
	// destructive — a message is handed over exactly once — so a shadow would
	// swallow the real recipient's mail.
	//
	// This tool built its address from the display name rather than taking it
	// from connSession.inbox, which already withholds one here. workspace_sessions
	// had the same defect on its listing path, found later by an independent
	// review; both are closed now. Do not restate either as "the only one" — the
	// pattern is that each surface deriving its own address is a fresh chance to
	// skip the gate, so the guard belongs at every one of them.
	if t.deps.sessionID() == "" {
		return "This session is not registered in the session directory, so it has no mailbox " +
			"address and no peer can write to it. Registration failed at startup — see the " +
			"daemon log.", nil
	}
	inbox := Inbox{
		Self:         self,
		SelfID:       t.deps.sessionID(),
		InheritedIDs: t.inheritedIDs(),
		Root:         t.deps.Workspace(),
		Policy:       policy,
		Workspace:    t.deps.StoreIfExists,
		Global:       t.deps.GlobalStoreIfExists,
	}
	// The receipt is appended to whichever branch below answers, and is resolved
	// after them so its ages are current even at the end of a 55-second wait.
	return t.read(ctx, args, policy, inbox) + t.outboxReceipt(ctx), nil
}

// read is Execute's body once the gates have passed: claim, or wait and claim.
// It returns no error — every branch is a message to the agent, including the
// ones that report nothing.
func (t *CheckMessages) read(ctx context.Context, args checkMessagesArgs, policy CollabPolicy, inbox Inbox) string {
	// Claim first: something may already be waiting, in which case there is
	// nothing to wait for.
	if rows := inbox.Claim(ctx); len(rows) > 0 {
		return t.render(rows, policy)
	}
	wait, clamped := clampWait(args.WaitSeconds, policy.maxWaitSeconds())
	if wait <= 0 {
		return t.empty(policy)
	}
	notice := waitClampNotice(args.WaitSeconds, policy.maxWaitSeconds(), clamped)

	// Snapshot the generations BEFORE waiting so a message written between the
	// claim above and the park below still wakes us instead of being missed.
	keys := inbox.Keys()
	since := t.deps.Notifier.Gens(keys)
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if woke := t.deps.Notifier.Wait(waitCtx, keys, since); !woke {
		return fmt.Sprintf("No messages (waited %s).%s", humaniseAge(wait), notice)
	}
	if rows := inbox.Claim(ctx); len(rows) > 0 {
		return t.render(rows, policy)
	}
	// Woken but nothing to claim. The legitimate cause is a race inside THIS
	// session: another delivery path (a tool-result block, or session_start) got
	// there first, which is the watermark doing its job. But a wake-up is not
	// evidence that a message for us exists — a session name is a daemon-wide
	// notifier key, so a send to a same-named peer in another project wakes us
	// too, and one this project has not opted in to read is invisible here.
	// Announcing an arrival would then be both false and a disclosure of
	// something the agent may not read, so the reply states only what is
	// certainly true and offers the race as a possibility.
	return t.empty(policy) +
		"  If a peer did write to you during the wait, another call in this session claimed it " +
		"first — each message is delivered exactly once.\n" + notice
}

// inheritedIDs are the predecessor identities this session may also read for,
// or nil when it did not come back through the authenticated reconnect path.
func (t *CheckMessages) inheritedIDs() []string {
	if t.deps.InheritedSessionIDs == nil {
		return nil
	}
	return t.deps.InheritedSessionIDs()
}

// receiptTimeout bounds the outbox read. It shares the delivery budget's
// reasoning: a receipt is a courtesy on someone else's call, never a reason to
// make that call slow.
const receiptTimeout = 250 * time.Millisecond

// maxReceiptRows caps how many unread sent messages are listed, so a sender that
// has written to a dozen absent peers gets the oldest few — the ones worth
// acting on — rather than a wall of its own text.
const maxReceiptRows = 5

// outboxReceipt reports this session's own messages that nobody has read yet.
//
// It is the half of the mailbox a sender could not otherwise see. Delivery is
// polling-only, so "no reply" has always had two indistinguishable causes: the
// peer read it and has not answered, or the peer never read it at all. Binding a
// message to a session sharpened that — a bound message expires unread rather
// than being inherited — which makes the difference something the sender must be
// able to observe rather than infer.
//
// It reads BOTH stores regardless of this session's cross_project flag, which
// gates delivery in the other direction: that flag decides whether this project
// will READ another project's messages, and these rows are the caller's own. It
// is also the case where the receipt matters most — a cross-project message to a
// recipient who never opted in expires unread by default. Errors are swallowed
// and the store is never created; a receipt must not fail the call it rides on.
func (t *CheckMessages) outboxReceipt(ctx context.Context) string {
	if t.deps.sessionID() == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, receiptTimeout)
	defer cancel()

	now := time.Now()
	var unread []collab.Row
	for _, get := range []func() *collab.Store{t.deps.StoreIfExists, t.deps.GlobalStoreIfExists} {
		if get == nil {
			continue
		}
		s := get()
		if s == nil {
			continue
		}
		// One past the display cap, per store, so the render can say there is more
		// without an unbounded scan. The cap cannot be applied per store and then
		// trusted as a total: two stores each capped at the display limit yield
		// twice it, and the overflow count would be measuring the query rather than
		// the mailbox.
		rows, err := s.UnreadSentBy(ctx, t.deps.sessionID(), now, maxReceiptRows+1)
		if err != nil {
			slog.Debug("collab: outbox receipt failed", "session", t.deps.sessionID(), "err", err)
			continue
		}
		unread = append(unread, rows...)
	}
	// Each store returns its own rows oldest-first; concatenating two of them does
	// not. Sort so the messages shown are genuinely the oldest — the ones whose
	// silence has lasted longest and is worth acting on.
	slices.SortStableFunc(unread, func(a, b collab.Row) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return renderReceipt(unread, now)
}

// renderReceipt formats the unread-sent block, or "" when everything the session
// sent has been read. Silence is the common case and carries its own meaning.
func renderReceipt(unread []collab.Row, now time.Time) string {
	if len(unread) == 0 {
		return ""
	}
	shown := unread
	if len(shown) > maxReceiptRows {
		shown = shown[:maxReceiptRows]
	}
	var sb strings.Builder
	sb.WriteString("\nYour own messages that NOBODY has read yet:\n")
	bound := false
	for _, r := range shown {
		to := r.Addressee
		if to == collab.AddresseeNext {
			to = `"next" (nobody has attached since)`
		}
		fmt.Fprintf(&sb, "  to %s — sent %s ago, still unread", to, humaniseAge(now.Sub(r.CreatedAt)))
		if r.TargetWorkspace != "" {
			fmt.Fprintf(&sb, " (cross-project, to %s)", r.TargetWorkspace)
		}
		sb.WriteString("\n")
		bound = bound || r.AddresseeID != ""
	}
	if n := len(unread) - len(shown); n > 0 {
		// "at least": each store was queried with its own cap, so this counts what
		// was fetched, not what exists. Reporting it as an exact total would be a
		// number the query cannot support.
		fmt.Fprintf(&sb, "  …and at least %d more.\n", n)
	}
	sb.WriteString("  Unread means the peer has made no tool call since — it is idle, not refusing. ")
	if bound {
		sb.WriteString("A message bound to a session that has since ended will expire unread rather " +
			"than pass to a later session of the same name, so re-send if you need it read. ")
	}
	sb.WriteString("Tell your human rather than waiting indefinitely.\n")
	return sb.String()
}

func (t *CheckMessages) render(rows []collab.Row, policy CollabPolicy) string {
	body := RenderMessages(rows, policy.ChatBudget(), time.Now())
	return strings.TrimLeft(body, "\n")
}

// empty explains the one thing an agent will otherwise get wrong: silence is not
// a reply. A peer idling on its human makes no tool calls and has not seen the
// message yet.
func (t *CheckMessages) empty(policy CollabPolicy) string {
	var sb strings.Builder
	sb.WriteString("No messages.\n")
	if !policy.CrossProject {
		sb.WriteString("  (Only this workspace's sessions can reach you — [collab] cross_project is off.)\n")
	}
	sb.WriteString("  A peer that is idle waiting on its human makes no tool calls, so it may not " +
		"have read yours yet. Silence is not a refusal.\n")
	return sb.String()
}

// clampWait bounds a requested wait to [0, max] seconds, and reports whether it
// had to. The caller needs the second value: a caller that asks for 300s and is
// silently given 55 builds its turn-handoff around a number that was never in
// force, and the only evidence it gets back is an elapsed time that looks like
// the call returned early.
func clampWait(requested, maxSeconds int) (wait time.Duration, clamped bool) {
	if requested <= 0 {
		return 0, false
	}
	if requested > maxSeconds {
		return time.Duration(maxSeconds) * time.Second, true
	}
	return time.Duration(requested) * time.Second, false
}

// waitClampNotice states that a requested wait was reduced, and names the knob.
// Empty when nothing was clamped, so the common case stays silent.
func waitClampNotice(requested, maxSeconds int, clamped bool) string {
	if !clamped {
		return ""
	}
	return fmt.Sprintf("  Your requested wait of %ds was clamped to %ds — the ceiling is "+
		"[collab] max_wait_seconds, kept below the client's own call timeout so the wait "+
		"cannot outlive the request carrying it.\n", requested, maxSeconds)
}
