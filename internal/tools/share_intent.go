package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
)

// ShareIntent is the share_intent MCP tool: a session broadcasts what it is
// working on ("refactoring the rate limiter — avoid internal/tools/ratelimit*")
// so peers can steer around in-progress work. One live intent per session — a
// new call replaces the previous one, keeping the model self-cleaning. Gated on
// [collab] intents; refused with a clear error when the flag is off.
//
// Concurrency: Execute is safe for concurrent use — it holds no state of its own
// and defers persistence to the per-workspace collab.Store (WAL-serialised).
type ShareIntent struct{ deps CollabDeps }

// NewShareIntent constructs the share_intent tool.
func NewShareIntent(deps CollabDeps) *ShareIntent { return &ShareIntent{deps: deps} }

func (*ShareIntent) Name() string { return "share_intent" }

func (*ShareIntent) Description() string {
	return "Broadcast what you are working on to other agents active on this " +
		"workspace RIGHT NOW, so they can steer around your in-progress work " +
		"instead of colliding with it (e.g. \"refactoring the rate limiter — " +
		"avoid internal/tools/ratelimit*\").\n\n" +
		"This is ADVISORY and a CLAIM, not a lock: it never blocks anyone's write, " +
		"and what you say you are doing is not the same as what the daemon observes " +
		"you did (that is workspace_sessions' recent_writes). Peers see your intent " +
		"in workspace_sessions, and a peer whose write touches a path matching your " +
		"path_globs gets a bounded advisory hint labelled as an unverified claim.\n\n" +
		"You have at most ONE live intent — calling this again replaces it. The " +
		"intent expires after ttl_minutes (default from [collab] intent_ttl_minutes) " +
		"and is cleared automatically when your session ends. Delivery is by polling " +
		"and hint injection only; plumb cannot push to another agent.\n\n" +
		"Requires [collab] intents = true; otherwise the call is refused. " +
		"Strictly per-workspace; the body is secret-scrubbed before storage.\n\n" +
		"Parameters:\n" +
		"  body        — what you are doing (required, free text).\n" +
		"  path_globs  — optional workspace-relative globs for the area you are " +
		"working on (e.g. [\"internal/tools/ratelimit*\"]); drives peer write hints.\n" +
		"  ttl_minutes — optional expiry override; defaults to [collab] intent_ttl_minutes."
}

func (*ShareIntent) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "body": {
      "type": "string",
      "description": "What you are working on (free text). Rendered to peers as an unverified claim."
    },
    "path_globs": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Optional workspace-relative globs for the area being worked on; a peer write matching one gets an advisory hint."
    },
    "ttl_minutes": {
      "type": "integer",
      "description": "Optional expiry in minutes; defaults to [collab] intent_ttl_minutes.",
      "minimum": 1
    }
  },
  "required": ["body"],
  "additionalProperties": false
}`)
}

type shareIntentArgs struct {
	Body       string   `json:"body"`
	PathGlobs  []string `json:"path_globs"`
	TTLMinutes int      `json:"ttl_minutes"`
}

func parseShareIntentArgs(raw json.RawMessage) (shareIntentArgs, error) {
	var a shareIntentArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("share_intent: %w", err)
	}
	if strings.TrimSpace(a.Body) == "" {
		return a, fmt.Errorf("share_intent: body is required")
	}
	return a, nil
}

func (t *ShareIntent) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	args, err := parseShareIntentArgs(raw)
	if err != nil {
		return "", err
	}
	policy := t.deps.Policy()
	if !policy.Intents {
		return "share_intent is disabled — set [collab] intents = true (globally or in this " +
			"workspace's .plumb/config.toml) to broadcast intents.", nil
	}
	ws := t.deps.Workspace()
	if ws == "" {
		return "workspace not yet attached — call session_start first", nil
	}
	store := t.deps.Store()
	if store == nil {
		return "", fmt.Errorf("share_intent: cross-agent store unavailable for this workspace")
	}
	return t.run(ctx, store, policy, args)
}

func (t *ShareIntent) run(ctx context.Context, store *collab.Store, policy CollabPolicy, args shareIntentArgs) (string, error) {
	body, redacted := redactBody(args.Body)
	ttl := resolveTTL(policy.IntentTTLMinutes, args.TTLMinutes)
	now := time.Now()
	in := collab.IntentInput{
		AuthorSession: t.deps.SessionName(),
		AuthorID:      t.deps.SessionID,
		Body:          body,
		PathGlobs:     args.PathGlobs,
		TTL:           ttl,
	}
	if err := store.PutIntent(ctx, in, now); err != nil {
		return "", fmt.Errorf("share_intent: %w", err)
	}
	return formatIntentResult(body, args.PathGlobs, ttl, redacted), nil
}

func formatIntentResult(body string, globs []string, ttl time.Duration, redacted bool) string {
	var sb strings.Builder
	sb.WriteString("Intent broadcast to peers on this workspace (advisory; replaces any prior intent).\n")
	fmt.Fprintf(&sb, "  intent:  %s\n", body)
	if len(globs) > 0 {
		fmt.Fprintf(&sb, "  area:    %s\n", strings.Join(globs, ", "))
	}
	fmt.Fprintf(&sb, "  expires: in %s\n", humaniseTTL(ttl))
	if redacted {
		sb.WriteString("  note:    a likely secret in the body was redacted before storage.\n")
	}
	return sb.String()
}

// humaniseTTL renders a TTL duration as a compact "2 h" / "45 min" / "3 days".
func humaniseTTL(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
