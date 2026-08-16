package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// LeaveNote is the leave_note MCP tool: a session leaves a short message for a
// named peer session, or for "whoever attaches to this workspace next". It is a
// minimal mailbox — notes only, no tasks, no threads, no arbitration. Gated on
// [collab] mailbox; refused with a clear error when the flag is off.
//
// Concurrency: Execute is safe for concurrent use — persistence is deferred to
// the per-workspace collab.Store (WAL-serialised).
type LeaveNote struct{ deps CollabDeps }

// NewLeaveNote constructs the leave_note tool.
func NewLeaveNote(deps CollabDeps) *LeaveNote { return &LeaveNote{deps: deps} }

func (*LeaveNote) Name() string { return "leave_note" }

func (*LeaveNote) Description() string {
	return "Send a message to another agent — a named peer session, or \"next\" " +
		"(whoever attaches to this workspace next). This is the send half of plumb's " +
		"mailbox; check_messages is the receive half.\n\n" +
		"CONVERSATIONS. Every message belongs to a thread. Omit conversation_id to " +
		"start one (the reply tells you the new id); pass the conversation_id you " +
		"were given to answer in the same thread — in which case you may omit `to` " +
		"and the reply goes to that thread's other participant, or is refused if the " +
		"thread has none or has several. A thread is capped at [collab] " +
		"max_exchanges messages — once spent, further replies are refused and you " +
		"should summarise the exchange for your human rather than starting a fresh " +
		"thread to keep talking.\n\n" +
		"DELIVERY is by polling only — plumb cannot push. A message reaches the peer " +
		"when it next makes any tool call (pending messages are appended to the " +
		"result), when it calls check_messages, or at its next session_start. Each " +
		"message is delivered exactly once. A peer that is idle waiting on its human " +
		"makes no tool calls and will not see it until it does something — so do not " +
		"assume silence means refusal.\n\n" +
		"ADDRESSED TO A SESSION, NOT TO A NAME. When the peer you name is connected, " +
		"the message is bound to that exact session and only it can ever read it. " +
		"Session names are reusable — an ended session does not reserve its name, and " +
		"any session may rename to a free one — so without that binding a message its " +
		"recipient never read would be handed to whoever next answers to the name. The " +
		"trade is deliberate: a bound message expires unread if its recipient never " +
		"comes back. Addressing a peer that is NOT CONNECTED stores no binding and " +
		"is delivered by name, as is \"next\".\n\n" +
		"CROSS-PROJECT. Addressing a session pinned to a different workspace is " +
		"allowed, but it is delivered only if THAT project sets [collab] " +
		"cross_project = true; otherwise it expires unread. Such a message is " +
		"labelled with your workspace when the peer reads it.\n\n" +
		"Messages expire after [collab] intent_ttl_minutes. Requires [collab] " +
		"mailbox = true; otherwise the call is refused. The body is secret-scrubbed " +
		"before storage.\n\n" +
		"Parameters:\n" +
		"  body            — the message (required, free text).\n" +
		"  to              — a peer session name, or \"next\". Omitting it means \"next\" " +
		"when starting a thread, and \"the other participant\" when replying into one.\n" +
		"  conversation_id — reply into an existing thread; omit to start one."
}

func (*LeaveNote) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "body": {
      "type": "string",
      "description": "The message to send (free text)."
    },
    "to": {
      "type": "string",
      "description": "A peer session name, or \"next\" for whoever attaches to this workspace next. Omitting it defaults to \"next\" when you are starting a thread; when you pass a conversation_id it instead resolves to that thread's other participant, and the send is refused if the thread has no other participant or more than one. A name belonging to a session in another workspace is delivered only if that project allows cross-project messages."
    },
    "conversation_id": {
      "type": "string",
      "description": "Reply into an existing thread by quoting the conversation id you were given. Omit to start a new thread."
    }
  },
  "required": ["body"],
  "additionalProperties": false
}`)
}

type leaveNoteArgs struct {
	Body           string `json:"body"`
	To             string `json:"to"`
	ConversationID string `json:"conversation_id"`
}

func parseLeaveNoteArgs(raw json.RawMessage) (leaveNoteArgs, error) {
	var a leaveNoteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("leave_note: %w", err)
	}
	if strings.TrimSpace(a.Body) == "" {
		return a, errors.New("leave_note: body is required")
	}
	a.To = strings.TrimSpace(a.To)
	a.ConversationID = strings.TrimSpace(a.ConversationID)
	// An omitted addressee means "next" ONLY when the caller is opening a new
	// thread. Quoting a conversation_id is an unambiguous statement of intent to
	// reply in that thread, and defaulting it to "next" contradicts that — so the
	// in-thread case is left empty here and resolved against the thread itself in
	// Execute, where the store is reachable.
	if a.To == "" && a.ConversationID == "" {
		a.To = collab.AddresseeNext
	}
	return a, nil
}

func (t *LeaveNote) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := parseLeaveNoteArgs(raw)
	if err != nil {
		return "", err
	}
	policy := t.deps.Policy()
	if !policy.Mailbox {
		return "leave_note is disabled — set [collab] mailbox = true (globally or in this " +
			"workspace's .plumb/config.toml) to leave notes for peers.", nil
	}
	ws := t.deps.Workspace()
	if ws == "" {
		return "workspace not yet attached — call session_start first", nil
	}
	if args.To == "" {
		to, refusal := t.resolveThreadAddressee(ctx, args.ConversationID)
		if refusal != "" {
			return refusal, nil
		}
		args.To = to
	}
	target, err := t.resolveTarget(ctx, args.To, ws, args.ConversationID)
	if err != nil {
		return "", err
	}
	return t.run(ctx, target, policy, args)
}

// noteTarget is where a message will be stored and why. A same-project message
// goes to the workspace's own collab.db; one addressed to a session pinned
// elsewhere goes to the daemon-level store, stamped with the sender's workspace
// so the recipient can see where it came from. The sender never writes into the
// recipient's directory.
type noteTarget struct {
	store         *collab.Store
	crossProject  bool
	peerWorkspace string
	origin        string // the sender's workspace; stamped only when crossProject
	// addresseeID binds the message to the one live session that answered to the
	// addressee's name. Empty when the peer is not live (or the name resolves
	// ambiguously), which leaves the message addressed by name alone — the
	// historical behaviour, and the only one available for a peer that has not
	// attached yet.
	addresseeID string
	// peerUnknown records that no live session answers to this name and no
	// conversation history placed it, so the message was filed in this
	// workspace's own mailbox on the assumption the peer will attach here.
	peerUnknown bool
}

// resolveTarget decides which store a message belongs in.
//
// Routing cannot rest on "is a session with that name live right now": a peer
// that exits between turns of a cross-project conversation would make the next
// reply fall back to the sender's OWN mailbox, where the peer will never see it
// while the sender is told it was sent. So an existing conversation is consulted
// first — it records the workspace the peer was writing from — and only a
// genuinely unplaceable name falls back to the local store, which the caller is
// then told about explicitly.
//
// "next" is always same-project: "whoever attaches next" has no meaning across
// projects, so it is never routed to the daemon-level store.
//
// Resolving the peer also decides whether the message is BOUND to it. A live
// peer contributes its session ID, and only that session can then read the
// message; a peer placed from conversation history contributes none, because
// history proves where a name used to be, not who holds it now. That is the
// deliberate trade: a bound message dies unread if its recipient never comes
// back, which is better than being read by whoever inherits the name.
func (t *LeaveNote) resolveTarget(ctx context.Context, to, ws, convID string) (noteTarget, error) {
	if to == collab.AddresseeNext {
		return t.localTarget()
	}
	peer, found := PeerSession{}, false
	if t.deps.ResolvePeer != nil {
		peer, found = t.deps.ResolvePeer(to)
	}
	if !found && convID != "" {
		// The peer is not connected, but this thread may remember where it lives.
		if g := t.globalIfExists(); g != nil {
			if w, ok := g.ConversationPeerWorkspace(ctx, convID, to); ok {
				peer.Workspace, found = w, true
			}
		}
	}
	if found && peer.Workspace != "" && !sameWorkspace(peer.Workspace, ws) {
		if t.deps.GlobalStore == nil {
			return noteTarget{}, errors.New("leave_note: cross-project store unavailable")
		}
		store := t.deps.GlobalStore()
		if store == nil {
			return noteTarget{}, errors.New("leave_note: cross-project store unavailable")
		}
		return noteTarget{
			store: store, crossProject: true, peerWorkspace: peer.Workspace,
			origin: ws, addresseeID: peer.ID,
		}, nil
	}
	local, err := t.localTarget()
	if err != nil {
		return local, err
	}
	local.peerUnknown = !found
	local.addresseeID = peer.ID
	return local, nil
}

// resolveThreadAddressee answers "who am I replying to?" for a note that quotes
// a conversation_id but names no addressee. It returns either the addressee to
// use, or a refusal to show the caller — never a silent fallback.
//
// Falling back to "next" here is what this replaces, and it failed in both
// directions at once: the reply went to whoever attached next instead of the
// peer being answered, and since "next" matches any claimant, the author's own
// session was frequently the one that took it. The sender saw a success line
// naming a delivery that never happened.
//
// Refusing is the right failure. A reply the caller believes is addressed is
// worse than a reply the caller is told it must address, because only the second
// one is visible.
func (t *LeaveNote) resolveThreadAddressee(ctx context.Context, convID string) (to, refusal string) {
	self := t.deps.SessionName()
	seen := map[string]bool{}
	var others []string

	note := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || name == collab.AddresseeNext || name == self || seen[name] {
			return
		}
		seen[name] = true
		others = append(others, name)
	}

	// A cross-project thread lives in the daemon-level store, a same-project one
	// in the workspace's own, and the caller does not say which. Read both rather
	// than guess; the global store is consulted only if it already exists, so this
	// never brings it into being.
	now := time.Now()
	for _, store := range []*collab.Store{t.deps.Store(), t.globalIfExists()} {
		if store == nil {
			continue
		}
		rows, err := store.Conversation(ctx, convID, now)
		if err != nil {
			continue
		}
		for _, r := range rows {
			note(r.AuthorSession)
			note(r.Addressee)
		}
	}

	switch len(others) {
	case 1:
		return others[0], ""
	case 0:
		return "", fmt.Sprintf(
			"Not sent: conversation %s has no other participant on record, so there is "+
				"nobody to reply to.\n\nThe thread may have expired ([collab] intent_ttl_minutes "+
				"prunes it), or the id may be wrong. Name the recipient explicitly with `to`, "+
				"or start a new thread by omitting conversation_id.", convID)
	default:
		slices.Sort(others)
		return "", fmt.Sprintf(
			"Not sent: conversation %s has %d other participants (%s), so \"reply to the "+
				"other one\" is ambiguous.\n\nName the recipient explicitly with `to`.",
			convID, len(others), strings.Join(others, ", "))
	}
}

func (t *LeaveNote) localTarget() (noteTarget, error) {
	store := t.deps.Store()
	if store == nil {
		return noteTarget{}, errors.New("leave_note: cross-agent store unavailable for this workspace")
	}
	return noteTarget{store: store}, nil
}

// globalIfExists returns the daemon-level store only when it already exists, so
// a routing lookup never brings it into being.
func (t *LeaveNote) globalIfExists() *collab.Store {
	if t.deps.GlobalStoreIfExists == nil {
		return nil
	}
	return t.deps.GlobalStoreIfExists()
}

// sameWorkspace compares two workspace roots the way the session registry
// records them. An empty root on either side is not treated as a match — an
// unknown workspace must not silently pass for "the same project".
//
// A lexical clean is sufficient here ONLY because both roots arrive
// canonicalised: internal/cli resolves symlinks once when it acquires the
// workspace, so the pin and every session.Folder derived from it share one
// spelling (issue #263). Do not add EvalSymlinks here to "harden" it — that puts
// a syscall on the send path to re-derive something already guaranteed upstream.
// If two spellings of one project ever reach this function again, the acquisition
// site regressed; fix it there, because a false "different project" here routes
// the note to the cross-project store, where the default config drops it unread.
func sameWorkspace(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func (t *LeaveNote) run(ctx context.Context, target noteTarget, policy CollabPolicy, args leaveNoteArgs) (string, error) {
	body, redacted := redactBody(args.Body)
	ttl := resolveTTL(policy.IntentTTLMinutes, 0)
	now := time.Now()
	limit := policy.maxExchanges()
	in := collab.NoteInput{
		AuthorSession:   t.deps.SessionName(),
		AuthorID:        t.deps.SessionID,
		Body:            body,
		Addressee:       args.To,
		AddresseeID:     target.addresseeID,
		TTL:             ttl,
		ConversationID:  args.ConversationID,
		OriginWorkspace: target.origin,
		TargetWorkspace: target.peerWorkspace,
		MaxExchanges:    limit,
	}
	// The exchange budget is the backstop against two agents answering each other
	// forever, and the store applies it as part of the insert. Counting here first
	// and then sending would let two simultaneous replies each pass a check the
	// other then invalidates, so the runaway exchange the cap exists to stop is
	// exactly the case that slips through it. Opening a new conversation is still
	// always allowed — a fresh thread holds nothing.
	conv, err := target.store.PutNote(ctx, in, now)
	if errors.Is(err, collab.ErrConversationFull) {
		return fmt.Sprintf(
			"This conversation has reached its %d-message limit ([collab] max_exchanges), so the "+
				"reply was NOT sent.\n\nThat limit exists to stop two agents talking to each other "+
				"indefinitely without a human in the loop. Summarise the exchange and what you "+
				"still need for your human rather than opening a fresh thread to continue it.",
			limit), nil
	}
	if err != nil {
		return "", fmt.Errorf("leave_note: %w", err)
	}
	// Wake the recipient's fast path. Best-effort and in-process: a missed bump
	// costs delivery latency (the next periodic check still finds the row), never
	// the message.
	t.deps.Notifier.Bump(collab.NotifyKey(t.deps.Workspace(), args.To))
	return formatNoteResult(body, args.To, conv, ttl, redacted, target, policy.ChatBudget()), nil
}

// replyDeliveryLine names BOTH delivery paths, because they are not
// alternatives an agent can infer from one another. Waiting server-side is the
// only way to hand your turn to a peer rather than poll, and it needs a tool
// call the agent must be told about; the passive path needs no action at all and
// is what actually fires for an agent that just carries on working. Naming only
// the active one left an agent believing a reply required a call it might never
// make.
func replyDeliveryLine() string {
	return "  To wait for a reply, call check_messages with a wait_seconds value; " +
		"otherwise it is appended to the result of your next tool call.\n"
}

func formatNoteResult(body, to, conv string, ttl time.Duration, redacted bool, target noteTarget, budget int) string {
	var sb strings.Builder
	dest := "session " + to
	if to == collab.AddresseeNext {
		dest = "the next session to attach (delivered once)"
	}
	fmt.Fprintf(&sb, "Message sent to %s (advisory; delivered by polling only).\n", dest)
	writeDeliveredBody(&sb, body, conv, budget)
	fmt.Fprintf(&sb, "  conversation: %s  — quote this as conversation_id to stay in the thread\n", conv)
	fmt.Fprintf(&sb, "  expires:      in %s\n", humaniseTTL(ttl))
	if redacted {
		sb.WriteString("  note:         a likely secret in the body was redacted before storage.\n")
	}
	if target.addresseeID != "" {
		fmt.Fprintf(&sb, "  bound to:     the session called %q that is live right now — only it can read "+
			"this. If it ends first the message expires unread rather than being handed to a "+
			"later session that takes the name.\n", to)
	}
	if target.crossProject {
		fmt.Fprintf(&sb, "  cross-project:  the recipient is pinned to %s. It is delivered only if THAT "+
			"project sets [collab] cross_project = true; otherwise it expires unread.\n", target.peerWorkspace)
	}
	if target.peerUnknown {
		fmt.Fprintf(&sb, "  unplaced:       no session named %q is connected, and no conversation places it. "+
			"The message was filed in THIS workspace's mailbox, so only a session called %q "+
			"attaching HERE will read it. If you meant an agent in another project, wait for "+
			"it to attach and send again.\n", to, to)
	}
	sb.WriteString(replyDeliveryLine())
	return sb.String()
}

// writeDeliveredBody reports the send-time truth about what the recipient
// will actually see, using the same clamp RenderMessages applies on delivery
// so the two never disagree about what fits.
//
// Under budget: the body is echoed (there is nothing yet to hide) alongside
// the byte count, so a sender learns the shape of the limit before losing
// content to it (PLAN-301 D3) rather than discovering it only once a message
// gets cut.
//
// Over budget: the full body is deliberately WITHHELD from this reply.
// Echoing it under a success line is what made the eventual delivery-time
// truncation invisible — the sender saw their complete message and reasonably
// concluded it arrived whole. Instead this names the word TRUNCATED, the
// exact byte counts, and the remedy: resend the remainder quoting
// conversation_id, or write the full text to a file and share its path
// (PLAN-301 D1, and the card's standing "notes are pointers" position).
func writeDeliveredBody(sb *strings.Builder, body, conv string, budget int) {
	clamped, marker := clampWithTruncationMarker(body, budget)
	if marker == "" {
		fmt.Fprintf(sb, "  message:      %s\n", body)
		if budget > 0 {
			fmt.Fprintf(sb, "  bytes:        %d of %d\n", len(body), budget)
		}
		return
	}
	fmt.Fprintf(sb, "  message:      TRUNCATED for delivery — only %d of %d bytes will reach the recipient "+
		"(the %d-byte [collab] chat_budget_bytes limit). The full text is withheld from this reply so it "+
		"cannot be mistaken for confirmed delivery.\n", len(clamped), len(body), budget)
	fmt.Fprintf(sb, "  remedy:       resend the remaining %d bytes in a follow-up leave_note quoting "+
		"conversation_id %q, or write the full text to a file and share its path instead.\n",
		len(body)-len(clamped), conv)
}
