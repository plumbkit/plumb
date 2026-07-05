package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/memory"
)

// ShareFindings is the share_findings MCP tool: a session flushes what it has
// learned into a durable, generated memory RIGHT NOW, instead of waiting for the
// idle episodic pipeline to fire. It rides that same pipeline end-to-end (redact
// → provenance stamp → markdown under .plumb/memories/ → FTS index →
// generated_memory_keep retention), so the finding is instantly discoverable by
// peers through every existing channel — search_memories, workspace_search,
// relevant_memories, hint injection, and the next session_start. Gated on
// [collab] knowledge_handoff; refused with a clear error when the flag is off.
//
// The finding is agent-authored GENERATED content — lower confidence than a
// user-written memory, provenance-stamped as such, and it never displaces a
// user memory in a capped hint slot.
//
// Concurrency: Execute is safe for concurrent use — it holds no state of its own
// and defers persistence to the memory store (per-file locked) and its FTS index.
type ShareFindings struct{ deps ShareFindingsDeps }

// ShareFindingsDeps bundles the share_findings dependencies. Unlike the phase-2
// collab tools it does not touch collab.db — it reuses the memory pipeline, so it
// takes the connection's live memory index and resolved generated-memory
// retention count rather than the collab store.
type ShareFindingsDeps struct {
	// Workspace returns the connection's pinned workspace root ("" pre-attach).
	Workspace func() string
	// SessionName returns this session's display name (unused for storage but kept
	// for symmetry with the other collab tools and future surfacing).
	SessionName func() string
	// SessionID is this session's stable ID — the provenance author and the seed
	// for the memory's distinguishing name suffix.
	SessionID string
	// Policy returns the resolved [collab] snapshot; KnowledgeHandoff gates the tool.
	Policy func() CollabPolicy
	// Index returns the connection's live memory FTS index, or nil when memory
	// indexing is disabled (the write then degrades to a plain markdown memory,
	// still discoverable by session_start and grep — the index is rebuildable).
	Index func() *memory.Index
	// GeneratedMemoryKeep returns the resolved [memory] generated_memory_keep, the
	// shared retention cap this finding counts against alongside episodic summaries.
	GeneratedMemoryKeep func() int
}

// NewShareFindings constructs the share_findings tool.
func NewShareFindings(deps ShareFindingsDeps) *ShareFindings { return &ShareFindings{deps: deps} }

func (*ShareFindings) Name() string { return "share_findings" }

func (*ShareFindings) Description() string {
	return "Hand off what you have just learned to other agents on this workspace " +
		"as a durable, searchable memory — RIGHT NOW, instead of waiting for the " +
		"idle summary to fire when your session ends.\n\n" +
		"Use it after you have mapped a subsystem, pinned down a gotcha, or worked " +
		"out how something fits together, so a peer working in parallel can pick it " +
		"up immediately. The finding is written through plumb's generated-memory " +
		"pipeline: it is secret-scrubbed before storage, stamped with your session " +
		"and the date as its provenance, and indexed for search. Peers discover it " +
		"through the ordinary channels — search_memories, workspace_search, " +
		"relevant_memories, memory hint injection, and the next session_start.\n\n" +
		"This is AGENT-GENERATED content: it is labelled lower-confidence than a " +
		"user-written memory and never displaces one in a capped hint slot. It " +
		"counts against the same [memory] generated_memory_keep retention as an idle " +
		"episodic summary. Nothing here is an LLM summary — you supply the text.\n\n" +
		"Requires [collab] knowledge_handoff = true; otherwise the call is refused. " +
		"Strictly per-workspace.\n\n" +
		"Parameters:\n" +
		"  summary     — a one- or two-line headline of the finding (required).\n" +
		"  description — optional longer detail appended below the summary.\n" +
		"  paths       — optional workspace-relative globs the finding is about " +
		"(e.g. [\"internal/tools/ratelimit*\"]); stored as frontmatter so " +
		"relevant_memories and hint injection route it to those files."
}

func (*ShareFindings) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "summary": {
      "type": "string",
      "description": "A one- or two-line headline of the finding. Stored as the memory body and indexed for search."
    },
    "description": {
      "type": "string",
      "description": "Optional longer detail, appended below the summary in the memory body."
    },
    "paths": {
      "type": "array",
      "items": { "type": "string" },
      "description": "Optional workspace-relative globs the finding is about; stored as frontmatter so relevant_memories and hint injection route it to those files."
    }
  },
  "required": ["summary"],
  "additionalProperties": false
}`)
}

type shareFindingsArgs struct {
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Paths       []string `json:"paths"`
}

func parseShareFindingsArgs(raw json.RawMessage) (shareFindingsArgs, error) {
	var a shareFindingsArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, fmt.Errorf("share_findings: %w", err)
	}
	if strings.TrimSpace(a.Summary) == "" {
		return a, fmt.Errorf("share_findings: summary is required")
	}
	return a, nil
}

func (t *ShareFindings) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	args, err := parseShareFindingsArgs(raw)
	if err != nil {
		return "", err
	}
	if !t.deps.Policy().KnowledgeHandoff {
		return "share_findings is disabled — set [collab] knowledge_handoff = true (globally or in " +
			"this workspace's .plumb/config.toml) to hand findings to peers.", nil
	}
	ws := t.deps.Workspace()
	if ws == "" {
		return "workspace not yet attached — call session_start first", nil
	}
	return t.run(ws, args)
}

func (t *ShareFindings) run(ws string, args shareFindingsArgs) (string, error) {
	body, redacted := shareFindingsBody(args.Summary, args.Description)
	now := time.Now().UTC()
	// Nanosecond component keeps the name unique when one session shares two
	// findings inside the same wall-clock second (e.g. two batched calls in a
	// single turn) — the store overwrites on name collision, so a second-
	// resolution timestamp alone would silently clobber the first finding.
	name := fmt.Sprintf("finding-%s-%09d-%s", now.Format("20060102-150405"), now.Nanosecond(), shortSessionSuffix(t.deps.SessionID))
	ix := t.deps.Index()
	// WriteGenerated redacts again before persistence (idempotent) — the belt-and-
	// braces guarantee no agent-supplied secret reaches durable storage.
	err := memory.WriteGenerated(ix, ws, name, "Shared finding (agent-generated)", body, memory.Provenance{
		Confidence:    memory.ConfidenceGenerated,
		SourceSession: t.deps.SessionID,
		SourcePaths:   args.Paths,
		CreatedAt:     now,
	})
	if err != nil {
		return "", fmt.Errorf("share_findings: %w", err)
	}
	if _, err := memory.PruneGeneratedEpisodic(ix, ws, t.deps.GeneratedMemoryKeep()); err != nil {
		// Non-fatal: the finding is already persisted; a failed prune only means the
		// retention pool is briefly over cap and will be trimmed on the next write.
		slog.Warn("share_findings: generated-memory prune failed", "err", err)
	}
	return formatFindingResult(name, args.Paths, redacted), nil
}

// shareFindingsBody assembles the memory body from the summary and optional
// description and reports whether redaction scrubbed anything (so the reply can
// note it). The persisted body is redacted again by WriteGenerated regardless.
func shareFindingsBody(summary, description string) (string, bool) {
	body := strings.TrimSpace(summary)
	if d := strings.TrimSpace(description); d != "" {
		body += "\n\n" + d
	}
	clean, redacted := redactBody(body)
	return clean + "\n", redacted
}

// shortSessionSuffix returns a short, filename-safe suffix from a session ID so a
// finding's name is distinguishable per session, mirroring the episodic naming.
func shortSessionSuffix(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "session"
	}
	return id
}

func formatFindingResult(name string, globs []string, redacted bool) string {
	var sb strings.Builder
	sb.WriteString("Finding shared as a generated memory (agent-authored; discoverable by peers now).\n")
	fmt.Fprintf(&sb, "  memory:  %s\n", name)
	if len(globs) > 0 {
		fmt.Fprintf(&sb, "  paths:   %s\n", strings.Join(globs, ", "))
	}
	sb.WriteString("  visible: search_memories, workspace_search, relevant_memories, hints, next session_start\n")
	if redacted {
		sb.WriteString("  note:    a likely secret in the body was redacted before storage.\n")
	}
	return sb.String()
}
