package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
		"With wait_seconds omitted or 0 it returns immediately with whatever is " +
		"waiting. With a positive wait_seconds it BLOCKS until a message arrives or " +
		"the wait expires — this is how you hand your turn to a peer after asking " +
		"something, instead of polling. The wait is capped by [collab] " +
		"max_wait_seconds, which is kept below the client's own call timeout.\n\n" +
		"Each message is delivered exactly ONCE, to whichever path sees it first — " +
		"this tool, the block appended to an ordinary tool result, or session_start. " +
		"Re-calling will not redeliver it, so act on a message when you read it.\n\n" +
		"Every message carries a conversation_id; quote it in leave_note to reply in " +
		"thread. A THREAD is capped at [collab] max_exchanges. Note what that does and " +
		"does not do: it bounds one conversation, and opening a new one starts a fresh " +
		"budget. It is a speed bump that forces a deliberate act, not an enforced limit " +
		"on how long two agents may talk \u2014 when a thread is spent, surface it to your " +
		"human rather than routing around the cap.\n\n" +
		"Requires [collab] mailbox = true. Messages from a session in another " +
		"workspace are shown only when this project sets [collab] cross_project = " +
		"true, and are labelled with the sending project.\n\n" +
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
	inbox := Inbox{
		Self:      self,
		Root:      t.deps.Workspace(),
		Policy:    policy,
		Workspace: t.deps.StoreIfExists,
		Global:    t.deps.GlobalStoreIfExists,
	}

	// Claim first: something may already be waiting, in which case there is
	// nothing to wait for.
	if rows := inbox.Claim(ctx); len(rows) > 0 {
		return t.render(rows, policy), nil
	}
	wait := clampWait(args.WaitSeconds, policy.maxWaitSeconds())
	if wait <= 0 {
		return t.empty(policy), nil
	}

	// Snapshot the generations BEFORE waiting so a message written between the
	// claim above and the park below still wakes us instead of being missed.
	keys := inbox.Keys()
	since := t.deps.Notifier.Gens(keys)
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	if woke := t.deps.Notifier.Wait(waitCtx, keys, since); !woke {
		return fmt.Sprintf("No messages (waited %s).", humaniseTTL(wait)), nil
	}
	if rows := inbox.Claim(ctx); len(rows) > 0 {
		return t.render(rows, policy), nil
	}
	// Woken but nothing to claim: another delivery path (a tool-result block, or
	// session_start) got there first. That is the watermark doing its job.
	return "No messages (a message arrived but was already delivered on another call).", nil
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

// clampWait bounds a requested wait to [0, max] seconds.
func clampWait(requested, maxSeconds int) time.Duration {
	if requested <= 0 {
		return 0
	}
	if requested > maxSeconds {
		requested = maxSeconds
	}
	return time.Duration(requested) * time.Second
}
