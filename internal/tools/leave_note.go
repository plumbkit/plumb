package tools

import (
	"context"
	"encoding/json"
	"fmt"
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
	return "Leave a short message for another agent on this workspace — either a " +
		"named peer session, or \"next\" (whoever attaches to this workspace next).\n\n" +
		"This is a MINIMAL mailbox: notes only, no tasks, no threads, no replies. " +
		"Delivery is by polling only (plumb cannot push): a note addressed to a " +
		"session name is delivered at that peer's session_start and listed in its " +
		"workspace_sessions until it expires; a note to \"next\" is delivered once, " +
		"to the first session that attaches after you leave it, then consumed.\n\n" +
		"Notes expire after [collab] intent_ttl_minutes. Requires [collab] mailbox = " +
		"true; otherwise the call is refused. Strictly per-workspace; the body is " +
		"secret-scrubbed before storage.\n\n" +
		"Parameters:\n" +
		"  body — the message (required, free text).\n" +
		"  to   — a peer session name, or \"next\" (default) for whoever attaches next."
}

func (*LeaveNote) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "body": {
      "type": "string",
      "description": "The message to leave (free text)."
    },
    "to": {
      "type": "string",
      "description": "A peer session name, or \"next\" (default) for whoever attaches to this workspace next."
    }
  },
  "required": ["body"],
  "additionalProperties": false
}`)
}

type leaveNoteArgs struct {
	Body string `json:"body"`
	To   string `json:"to"`
}

func parseLeaveNoteArgs(raw json.RawMessage) (leaveNoteArgs, error) {
	var a leaveNoteArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("leave_note: %w", err)
	}
	if strings.TrimSpace(a.Body) == "" {
		return a, fmt.Errorf("leave_note: body is required")
	}
	a.To = strings.TrimSpace(a.To)
	if a.To == "" {
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
	store := t.deps.Store()
	if store == nil {
		return "", fmt.Errorf("leave_note: cross-agent store unavailable for this workspace")
	}
	return t.run(ctx, store, policy, args)
}

func (t *LeaveNote) run(ctx context.Context, store *collab.Store, policy CollabPolicy, args leaveNoteArgs) (string, error) {
	body, redacted := redactBody(args.Body)
	ttl := resolveTTL(policy.IntentTTLMinutes, 0)
	now := time.Now()
	in := collab.NoteInput{
		AuthorSession: t.deps.SessionName(),
		AuthorID:      t.deps.SessionID,
		Body:          body,
		Addressee:     args.To,
		TTL:           ttl,
	}
	if err := store.PutNote(ctx, in, now); err != nil {
		return "", fmt.Errorf("leave_note: %w", err)
	}
	return formatNoteResult(body, args.To, ttl, redacted), nil
}

func formatNoteResult(body, to string, ttl time.Duration, redacted bool) string {
	var sb strings.Builder
	dest := "session " + to
	if to == collab.AddresseeNext {
		dest = "the next session to attach (delivered once)"
	}
	fmt.Fprintf(&sb, "Note left for %s (advisory; delivered by polling only).\n", dest)
	fmt.Fprintf(&sb, "  note:    %s\n", body)
	fmt.Fprintf(&sb, "  expires: in %s\n", humaniseTTL(ttl))
	if redacted {
		sb.WriteString("  note:    a likely secret in the body was redacted before storage.\n")
	}
	return sb.String()
}
