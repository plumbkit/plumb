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
// Notes already ride along on ordinary tool results, which is enough for an
// agent that is busy working. It is not enough for turn-taking: after sending a
// question you have nothing to do until the answer comes, and polling for it
// burns a tool call per attempt. This tool lets a session hand its turn over —
// parking server-side on the daemon's in-process notifier until a note
// arrives or the wait expires — so a back-and-forth costs one call per turn.
//
// Concurrency: Execute is safe for concurrent use; the wait holds no lock.
type CheckMessages struct{ deps CollabDeps }

// NewCheckMessages constructs the check_messages tool.
func NewCheckMessages(deps CollabDeps) *CheckMessages { return &CheckMessages{deps: deps} }

func (*CheckMessages) Name() string { return "check_messages" }

func (*CheckMessages) Description() string {
	return "Read notes other agents have sent you, optionally waiting for one to " +
		"arrive. The receive half of plumb's request/reply mailbox; leave_note is the " +
		"send half.\n\n" +
		"With wait_seconds omitted or 0 it returns immediately with whatever is " +
		"waiting. With a positive value it BLOCKS until a note arrives or the wait " +
		"expires, handing your turn to a peer instead of polling. The wait is capped " +
		"by [collab] max_wait_seconds; the result reports elapsed seconds and any clamp.\n\n" +
		"Each note is atomically claimed at most once across this tool, ordinary tool " +
		"results, and session_start. A transport failure after claim can still lose it; " +
		"end-to-end exactly-once is not promised, so act on a note when you read it.\n\n" +
		"Every note carries a conversation_id; quote it in leave_note to reply. An " +
		"in-thread reply may omit to because plumb binds the other active participant by stable ID. " +
		"Conversations have no message-count ceiling. If an exchange grows long, put " +
		"the substance in a file and send its path in a short note.\n\n" +
		"Requires [collab] mailbox = true. Cross-project notes are shown only when this " +
		"project sets [collab] cross_project = true, and name their source project.\n\n" +
		"Parameters:\n" +
		"  wait_seconds — block up to this many seconds for a note (default 0)."
}

func (*CheckMessages) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "wait_seconds": {
      "type": "integer",
      "minimum": 0,
      "description": "Block up to this many seconds waiting for a note. 0 (default) returns immediately. Capped by [collab] max_wait_seconds; the result reports elapsed seconds and any clamp."
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
			"this workspace's .plumb/config.toml) to exchange notes with peers.", nil
	}
	self := t.deps.SessionName()
	if self == "" || t.deps.SessionID == "" {
		return "session is not registered and has no safe mailbox identity — reconnect before reading notes", nil
	}
	inbox := Inbox{
		Self:      self,
		SelfID:    t.deps.SessionID,
		Root:      t.deps.Workspace(),
		Policy:    policy,
		Workspace: t.deps.StoreIfExists,
		Global:    t.deps.GlobalStoreIfExists,
	}

	// Snapshot before the first claim. If a note commits while that claim is
	// checking SQLite, its bump remains newer than this baseline and the wait
	// below re-checks the store instead of sleeping through it.
	keys := inbox.Keys()
	since := t.deps.Notifier.Gens(keys)
	if rows := inbox.Claim(ctx); len(rows) > 0 {
		return t.render(rows, policy), nil
	}
	maxWait := policy.maxWaitSeconds()
	wait, clamped := clampWait(args.WaitSeconds, maxWait)
	if wait <= 0 {
		return t.empty(policy), nil
	}

	started := time.Now()
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if woke := t.deps.Notifier.Wait(waitCtx, keys, since); !woke {
			return formatWaitTimeout(time.Since(started), args.WaitSeconds, maxWait, clamped), nil
		}
		// Advance the baseline BEFORE claiming. A bump during or after this claim
		// therefore stays newer and the next Wait observes it. A wake with no
		// readable row can come from a same-named session outside this recipient's
		// consent boundary, or from another delivery path winning the atomic claim;
		// neither is a reason to disclose the wake or end the requested wait early.
		since = t.deps.Notifier.Gens(keys)
		if rows := inbox.Claim(ctx); len(rows) > 0 {
			return t.render(rows, policy) + waitClampNotice(args.WaitSeconds, maxWait, clamped), nil
		}
	}
}

func (t *CheckMessages) render(rows []collab.Row, policy CollabPolicy) string {
	body := RenderMessages(rows, policy.ChatBudget(), time.Now())
	return strings.TrimLeft(body, "\n")
}

// empty explains the one thing an agent will otherwise get wrong: silence is not
// a reply. A peer idling on its human makes no tool calls and has not seen the
// note yet.
func (t *CheckMessages) empty(policy CollabPolicy) string {
	var sb strings.Builder
	sb.WriteString("No notes.\n")
	if !policy.CrossProject {
		sb.WriteString("  (Only this workspace's sessions can reach you — [collab] cross_project is off.)\n")
	}
	sb.WriteString("  A peer that is idle waiting on its human makes no tool calls, so it may not " +
		"have read yours yet. Silence is not a refusal.\n")
	return sb.String()
}

// clampWait bounds a requested wait to [0, max] seconds and reports whether
// the caller's value was reduced.
func clampWait(requested, maxSeconds int) (time.Duration, bool) {
	if requested <= 0 {
		return 0, false
	}
	clamped := requested > maxSeconds
	if clamped {
		requested = maxSeconds
	}
	return time.Duration(requested) * time.Second, clamped
}

func formatWaitTimeout(elapsed time.Duration, requested, maxSeconds int, clamped bool) string {
	seconds := int((elapsed + 500*time.Millisecond) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("No notes (waited %ds).%s\n  A peer idle waiting on its human makes no tool calls; silence is not a refusal.\n",
		seconds, waitClampNotice(requested, maxSeconds, clamped))
}

func waitClampNotice(requested, maxSeconds int, clamped bool) string {
	if !clamped {
		return ""
	}
	return fmt.Sprintf(" Requested wait %ds was clamped to [collab] max_wait_seconds=%ds.", requested, maxSeconds)
}
