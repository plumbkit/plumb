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
	"github.com/plumbkit/plumb/internal/session"
)

// LeaveNote is the leave_note MCP tool: a session leaves a short note for a
// named peer session, or for "whoever attaches to this workspace next". It is a
// request/reply mailbox, not a task or arbitration system. Gated on
// [collab] mailbox; refused with a clear error when the flag is off.
//
// Concurrency: Execute is safe for concurrent use — persistence is deferred to
// the per-workspace collab.Store (WAL-serialised).
type LeaveNote struct{ deps CollabDeps }

// NewLeaveNote constructs the leave_note tool.
func NewLeaveNote(deps CollabDeps) *LeaveNote { return &LeaveNote{deps: deps} }

func (*LeaveNote) Name() string { return "leave_note" }

func (*LeaveNote) Description() string {
	return "Send a note to another agent — a named active peer session, or \"next\" " +
		"(whoever attaches to this workspace next). This is the send half of plumb's " +
		"request/reply mailbox; check_messages is the receive half.\n\n" +
		"CONVERSATIONS. Omit conversation_id to start one (the reply tells you the new " +
		"id). Pass that id to reply in-thread; plumb binds the other active participant " +
		"by stable session ID and fails closed for offline, foreign, or ambiguous threads. Conversations " +
		"have no message-count ceiling. If an exchange grows long, write the substance " +
		"to a file and send its path in a short note.\n\n" +
		"DELIVERY is by polling — MCP is request/reply, so plumb cannot push. A note " +
		"reaches the peer when it next makes any tool call, when it calls check_messages, or " +
		"runs session_start. Plumb claims each note at most once across those paths; a " +
		"transport failure after claim can still lose it. A peer idle on its " +
		"human makes no calls, so silence is not a refusal.\n\n" +
		"BODIES are stored and delivered under [collab] chat_budget_bytes. The send receipt says the note " +
		"is queued (pending delivery) and gives the byte budget; workspace_sessions shows " +
		"the later delivered transition. If a note is cut, both parties are told exactly " +
		"how many bytes were truncated and how to send the remainder.\n\n" +
		"CROSS-PROJECT. A note to a session in another workspace is delivered only if " +
		"THAT project sets [collab] cross_project = true; otherwise it expires unread.\n\n" +
		"Notes expire after [collab] intent_ttl_minutes. Requires [collab] mailbox = " +
		"true. Bodies are secret-scrubbed before storage.\n\n" +
		"Parameters:\n" +
		"  body            — the note (required, free text).\n" +
		"  to              — an active peer name, or \"next\"; omit on an in-thread reply.\n" +
		"  conversation_id — reply into an existing conversation; omit to start one."
}

func (*LeaveNote) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "body": {
      "type": "string",
      "description": "The note to send (free text)."
    },
    "to": {
      "type": "string",
      "description": "An active peer display name, or \"next\" for whoever attaches to this workspace next. Defaults to next only when conversation_id is omitted; an in-thread omission resolves the other participant or refuses. Cross-project delivery requires the recipient project to opt in."
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
	if a.To == "" && a.ConversationID == "" {
		a.To = collab.AddresseeNext
	}
	if a.To != "" && a.To != collab.AddresseeNext {
		to, err := session.NormaliseName(a.To)
		if err != nil {
			return a, fmt.Errorf("leave_note: invalid addressee: %w", err)
		}
		a.To = to
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
	self := t.deps.SessionName()
	if self == "" || t.deps.SessionID == "" {
		return "session is not registered and has no safe mailbox identity — reconnect before sending notes", nil
	}
	to, targetID, boundWorkspace, err := t.resolveAddressee(ctx, args.To, args.ConversationID)
	if err != nil {
		return "", err
	}
	if targetID == t.deps.SessionID || (targetID == "" && to == self) {
		return "Note NOT sent: the addressee resolves to this session. A note is never delivered to its own author.", nil
	}
	args.To = to
	target, err := t.resolveTarget(args.To, targetID, ws, boundWorkspace)
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
	peerID        string // stable intended recipient; empty only for next/unplaced
	origin        string // the sender's workspace; stamped only when crossProject
	// peerUnknown records that no live session answers to this name and no
	// conversation history placed it, so the message was filed in this
	// workspace's own mailbox on the assumption the peer will attach here.
	peerUnknown bool
}

// resolveTarget decides which store a message belongs in. A threaded reply
// carries the live workspace resolved from stable identity; a fresh message may
// still resolve a display name through the live session directory. "next" is
// always same-project.
func (t *LeaveNote) resolveTarget(to, targetID, ws, boundWorkspace string) (noteTarget, error) {
	if to == collab.AddresseeNext {
		return t.localTarget()
	}
	found := targetID != "" && boundWorkspace != ""
	if (targetID == "") != (boundWorkspace == "") {
		return noteTarget{}, errors.New("leave_note: incomplete stable peer route")
	}
	if found && !sameWorkspace(boundWorkspace, ws) {
		if t.deps.GlobalStore == nil {
			return noteTarget{}, errors.New("leave_note: cross-project store unavailable")
		}
		store := t.deps.GlobalStore()
		if store == nil {
			return noteTarget{}, errors.New("leave_note: cross-project store unavailable")
		}
		return noteTarget{
			store: store, crossProject: true, peerWorkspace: boundWorkspace, peerID: targetID, origin: ws,
		}, nil
	}
	local, err := t.localTarget()
	if err != nil {
		return local, err
	}
	local.peerUnknown = !found
	local.peerID = targetID
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

// resolveAddressee binds an in-thread reply to the other participant's stable
// session ID, then resolves that ID to the live session's current name and
// workspace. Offline, ambiguous, legacy, and non-participant threads fail closed
// so name reuse can never retarget a reply.
func (t *LeaveNote) resolveAddressee(
	ctx context.Context,
	to, conversationID string,
) (string, string, string, error) {
	if conversationID == "" {
		if to == collab.AddresseeNext || t.deps.PeerSessionByName == nil {
			return to, "", "", nil
		}
		id, workspace, found, ambiguous := t.deps.PeerSessionByName(to)
		if ambiguous {
			return "", "", "", fmt.Errorf(
				"leave_note: more than one active session is named %q; use a unique session name", to)
		}
		if !found {
			return to, "", "", nil
		}
		if id == "" || workspace == "" {
			return "", "", "", errors.New("leave_note: active peer has no stable route")
		}
		return to, id, workspace, nil
	}
	peer, err := t.conversationPeer(ctx, conversationID)
	if err != nil {
		return "", "", "", err
	}
	name, workspace, err := t.liveConversationPeer(peer, to)
	return name, peer.ID, workspace, err
}

func (t *LeaveNote) conversationPeer(
	ctx context.Context,
	conversationID string,
) (collab.ConversationParticipant, error) {
	peers := make(map[string]collab.ConversationParticipant)
	participated, complete := false, true
	for _, store := range t.conversationStores() {
		storePeers, storeParticipation, storeComplete, err := store.ConversationParticipants(
			ctx, conversationID, t.deps.SessionID, time.Now())
		if err != nil {
			return collab.ConversationParticipant{},
				fmt.Errorf("leave_note: resolve conversation peer: %w", err)
		}
		participated = participated || storeParticipation
		complete = complete && storeComplete
		for _, peer := range storePeers {
			if prior, ok := peers[peer.ID]; ok && peer.Workspace == "" {
				peer.Workspace = prior.Workspace
			}
			peers[peer.ID] = peer
		}
	}
	if !participated || !complete || len(peers) != 1 {
		return collab.ConversationParticipant{}, fmt.Errorf(
			"leave_note: conversation_id %q does not prove this session has exactly one other stable participant",
			conversationID)
	}
	for _, peer := range peers {
		return peer, nil
	}
	return collab.ConversationParticipant{}, errors.New("leave_note: conversation peer resolution failed")
}

func (t *LeaveNote) conversationStores() []*collab.Store {
	var stores []*collab.Store
	if t.deps.StoreIfExists != nil {
		if store := t.deps.StoreIfExists(); store != nil {
			stores = append(stores, store)
		}
	}
	if store := t.globalIfExists(); store != nil {
		stores = append(stores, store)
	}
	return stores
}

func (t *LeaveNote) liveConversationPeer(
	peer collab.ConversationParticipant,
	requestedName string,
) (string, string, error) {
	if t.deps.PeerSessionByID == nil {
		return "", "", errors.New("leave_note: stable peer lookup unavailable")
	}
	currentName, currentWorkspace, found := t.deps.PeerSessionByID(peer.ID)
	if !found || currentName == "" || currentWorkspace == "" {
		return "", "", fmt.Errorf(
			"leave_note: conversation participant %q is not active; start a new conversation to the peer's current active session name instead of replying to this thread",
			peer.Name)
	}
	if requestedName != "" && requestedName != peer.Name && requestedName != currentName {
		return "", "", fmt.Errorf(
			"leave_note: to %q is not conversation participant %q (currently %q)",
			requestedName, peer.Name, currentName)
	}
	return currentName, currentWorkspace, nil
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
	window := noteBodyWindow(body, policy.ChatBudget())
	ttl := resolveTTL(policy.IntentTTLMinutes, 0)
	now := time.Now()
	in := collab.NoteInput{
		AuthorSession:   t.deps.SessionName(),
		AuthorID:        t.deps.SessionID,
		Body:            window.body,
		OriginalBytes:   window.total,
		Addressee:       args.To,
		TargetID:        target.peerID,
		TTL:             ttl,
		ConversationID:  args.ConversationID,
		OriginWorkspace: target.origin,
		TargetWorkspace: target.peerWorkspace,
	}
	conv, err := target.store.PutNote(ctx, in, now)
	if err != nil {
		return "", fmt.Errorf("leave_note: %w", err)
	}
	// Wake the recipient's fast path. Best-effort and in-process: a missed bump
	// costs delivery latency (the next periodic check still finds the row), never
	// the note.
	t.deps.Notifier.Bump(collab.NotifyKey(t.deps.Workspace(), args.To))
	if target.peerID != "" {
		t.deps.Notifier.Bump(collab.NotifySessionKey(target.peerID))
	}
	return formatNoteResult(body, args.To, conv, ttl, redacted, target, policy.ChatBudget()), nil
}

// replyDeliveryLine names both delivery paths: server-side waiting and passive
// delivery on the next successful tool result.
func replyDeliveryLine() string {
	return "  To wait for a reply, call check_messages with a wait_seconds value; " +
		"otherwise it is appended to the result of your next tool call.\n"
}

func formatNoteResult(
	body, to, conv string,
	ttl time.Duration,
	redacted bool,
	target noteTarget,
	budget int,
) string {
	var sb strings.Builder
	dest := "session " + to
	if to == collab.AddresseeNext {
		dest = "the next session to attach (claimed at most once)"
	}
	window := noteBodyWindow(body, budget)
	if window.delivered < window.total {
		fmt.Fprintf(&sb, "Note queued for %s (pending delivery): %d of %d bytes within the configured %d-byte "+
			"budget — %d bytes were TRUNCATED and the recipient did not receive them.\n",
			dest, window.delivered, window.total, budget, window.total-window.delivered)
		if redacted {
			fmt.Fprintf(&sb, "  remedy:       redaction changed the stored representation, so there is no reliable "+
				"byte offset into the submitted body. Send the substantive remainder in a follow-up "+
				"quoting conversation_id %s. For longer content, write a safe file and send its path.\n", conv)
		} else {
			fmt.Fprintf(&sb, "  remedy:       send the remainder in a follow-up quoting conversation_id %s; "+
				"resume the original UTF-8 body at byte offset %d. For longer content, write a file "+
				"and send its path.\n", conv, window.delivered)
		}
	} else {
		fmt.Fprintf(&sb, "Note queued for %s (pending delivery): %d of %d budget bytes.\n", dest, window.delivered, budget)
	}
	fmt.Fprintf(&sb, "  conversation: %s  — quote this as conversation_id to stay in the thread\n", conv)
	fmt.Fprintf(&sb, "  expires:      in %s\n", humaniseTTL(ttl))
	if redacted {
		sb.WriteString("  note:         a likely secret in the body was redacted before storage.\n")
	}
	if target.crossProject {
		sb.WriteString("  cross-project:  the recipient is pinned to another project. It is delivered only if THAT " +
			"project sets [collab] cross_project = true; otherwise it expires unread.\n")
	}
	if target.peerUnknown {
		fmt.Fprintf(&sb, "  unplaced:       no session named %q is connected, and no conversation places it. "+
			"The note was filed in THIS workspace's mailbox, so only a session called %q "+
			"attaching HERE will read it. If you meant an agent in another project, wait for "+
			"it to attach and send again.\n", to, to)
	}
	sb.WriteString(replyDeliveryLine())
	return sb.String()
}
