package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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
		"were given to answer in the same thread. A thread is capped at [collab] " +
		"max_exchanges messages — once spent, further replies are refused and you " +
		"should summarise the exchange for your human rather than starting a fresh " +
		"thread to keep talking.\n\n" +
		"DELIVERY is by polling only — plumb cannot push. A message reaches the peer " +
		"when it next makes any tool call (pending messages are appended to the " +
		"result), when it calls check_messages, or at its next session_start. Each " +
		"message is delivered exactly once. A peer that is idle waiting on its human " +
		"makes no tool calls and will not see it until it does something — so do not " +
		"assume silence means refusal.\n\n" +
		"CROSS-PROJECT. Addressing a session pinned to a different workspace is " +
		"allowed, but it is delivered only if THAT project sets [collab] " +
		"cross_project = true; otherwise it expires unread. Such a message is " +
		"labelled with your workspace when the peer reads it.\n\n" +
		"Messages expire after [collab] intent_ttl_minutes. Requires [collab] " +
		"mailbox = true; otherwise the call is refused. The body is secret-scrubbed " +
		"before storage.\n\n" +
		"Parameters:\n" +
		"  body            — the message (required, free text).\n" +
		"  to              — a peer session name, or \"next\" (default).\n" +
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
      "description": "A peer session name, or \"next\" (default) for whoever attaches to this workspace next. A name belonging to a session in another workspace is delivered only if that project allows cross-project messages."
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
	if a.To == "" {
		a.To = collab.AddresseeNext
	}
	a.ConversationID = strings.TrimSpace(a.ConversationID)
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
func (t *LeaveNote) resolveTarget(ctx context.Context, to, ws, convID string) (noteTarget, error) {
	if to == collab.AddresseeNext {
		return t.localTarget()
	}
	peerWS, found := "", false
	if t.deps.PeerWorkspace != nil {
		peerWS, found = t.deps.PeerWorkspace(to)
	}
	if !found && convID != "" {
		// The peer is not connected, but this thread may remember where it lives.
		if g := t.globalIfExists(); g != nil {
			if w, ok := g.ConversationPeerWorkspace(ctx, convID, to); ok {
				peerWS, found = w, true
			}
		}
	}
	if found && peerWS != "" && !sameWorkspace(peerWS, ws) {
		if t.deps.GlobalStore == nil {
			return noteTarget{}, errors.New("leave_note: cross-project store unavailable")
		}
		store := t.deps.GlobalStore()
		if store == nil {
			return noteTarget{}, errors.New("leave_note: cross-project store unavailable")
		}
		return noteTarget{store: store, crossProject: true, peerWorkspace: peerWS, origin: ws}, nil
	}
	local, err := t.localTarget()
	if err != nil {
		return local, err
	}
	local.peerUnknown = !found
	return local, nil
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
	return formatNoteResult(body, args.To, conv, ttl, redacted, target), nil
}

func formatNoteResult(body, to, conv string, ttl time.Duration, redacted bool, target noteTarget) string {
	var sb strings.Builder
	dest := "session " + to
	if to == collab.AddresseeNext {
		dest = "the next session to attach (delivered once)"
	}
	fmt.Fprintf(&sb, "Message sent to %s (advisory; delivered by polling only).\n", dest)
	fmt.Fprintf(&sb, "  message:      %s\n", body)
	fmt.Fprintf(&sb, "  conversation: %s  — quote this as conversation_id to stay in the thread\n", conv)
	fmt.Fprintf(&sb, "  expires:      in %s\n", humaniseTTL(ttl))
	if redacted {
		sb.WriteString("  note:         a likely secret in the body was redacted before storage.\n")
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
	sb.WriteString("  To wait for a reply, call check_messages with a wait_seconds value.\n")
	return sb.String()
}
