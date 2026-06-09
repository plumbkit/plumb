package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/session"
	"github.com/plumbkit/plumb/internal/stats"
)

// writeToolNames is the set of mutating MCP tool names used by the
// recent-writes feed. Any tool in this list that modifies workspace files is
// captured, so an agent can spot co-worker edits before touching the same file.
//
// The list deliberately excludes read-only tools (read_file, find_symbol, …)
// and meta-tools (rename_session, session_start, daemon_info) — only ops that
// change on-disk content are interesting for conflict-awareness.
var writeToolNames = []string{
	"write_file",
	"edit_file",
	"delete_file",
	"rename_file",
	"copy_file",
	"transaction_apply",
	"find_replace",
	"rename_symbol",
	"replace_symbol_body",
	"insert_before_symbol",
	"insert_after_symbol",
	"safe_delete_symbol",
	"git",
}

// WorkspaceSessions returns peer-session awareness for the caller's workspace.
// Concurrency: Execute is pure-read and takes no in-process mutexes of its own;
// it calls session.List (filesystem flock) and stats.SharedReadOnly (a process-
// cached read-only connection). Both are bounded by the wsSessionsTimeout so a stuck
// NFS mount or an unusually large session directory never blocks the MCP
// response. No deadlock is possible because the tool never holds more than one
// resource at a time, and the resource it does hold (an OS flock) is not
// involved in any Go mutex ordering.
type WorkspaceSessions struct {
	workspace     func() string
	selfSessID    string
	boundaryCheck func(string) error // read boundary guard
}

// NewWorkspaceSessions creates the workspace_sessions tool.
// workspace returns the session's pinned workspace root.
// selfSessID is the current session's ID (excluded from the peer list).
// boundary is the per-connection read boundary guard.
func NewWorkspaceSessions(workspace func() string, selfSessID string) *WorkspaceSessions {
	return &WorkspaceSessions{workspace: workspace, selfSessID: selfSessID}
}

// WithBoundary wires the read boundary guard. Returns the receiver for chaining.
func (t *WorkspaceSessions) WithBoundary(fn func(string) error) *WorkspaceSessions {
	t.boundaryCheck = fn
	return t
}

func (*WorkspaceSessions) Name() string { return "workspace_sessions" }

func (*WorkspaceSessions) Description() string {
	return "Returns same-workspace session awareness: who else is actively connected to " +
		"this project and what files they recently edited.\n\n" +
		"**you** — this session's name.\n\n" +
		"**active_sessions** — sessions on this workspace right now (name, client, " +
		"how long since their last tool call). A single entry whose is_self field is " +
		"true means you are the only active session — your view of the workspace is " +
		"authoritative. Multiple entries mean concurrent agents are working here; " +
		"treat any file a peer recently touched as potentially changed.\n\n" +
		"**recent_writes** — the last N mutating operations (write_file, edit_file, " +
		"rename_file, git commit, …) by all sessions on this workspace. " +
		"The file path (when available), session name, operation, and age are shown.\n\n" +
		"Use this before editing a file that another session may have recently " +
		"modified: if it appears in recent_writes, re-read it first.\n\n" +
		"Parameters:\n" +
		"  recent_limit  — max recent-write entries to return (default 10, max 50).\n\n" +
		"Workspace boundary: workspace_sessions is scoped to the caller's pinned " +
		"workspace; it never reveals sessions from a different project."
}

func (*WorkspaceSessions) InputSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "recent_limit": {
      "type": "integer",
      "description": "Maximum recent-write entries to return (1–50; default 10).",
      "minimum": 1,
      "maximum": 50
    }
  },
  "additionalProperties": false
}`)
}

type workspaceSessionsArgs struct {
	RecentLimit int `json:"recent_limit"`
}

func parseWorkspaceSessionsArgs(raw json.RawMessage) (workspaceSessionsArgs, error) {
	var a workspaceSessionsArgs
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, fmt.Errorf("workspace_sessions: %w", err)
		}
	}
	return a, nil
}

// wsSessionsTimeout caps the total time spent on the session-file scan and the
// stats DB query together. Both are read-only and quick on any well-behaved
// filesystem; the timeout is a hard backstop for NFS/SMB mounts and large
// daemon-log directories with thousands of stale session files.
const wsSessionsTimeout = 500 * time.Millisecond

func (t *WorkspaceSessions) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	args, err := parseWorkspaceSessionsArgs(raw)
	if err != nil {
		return "", err
	}
	limit := args.RecentLimit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	workspace := t.workspace()
	if workspace == "" {
		return "workspace not yet attached — call session_start first", nil
	}
	// Boundary check: the workspace value is already pinned to this session, but
	// we apply the guard anyway so the tool honours the same contract as every
	// other path-bearing tool (boundary violations are logged and marked).
	if t.boundaryCheck != nil {
		if err := t.boundaryCheck(workspace); err != nil {
			return "", err
		}
	}

	result := runWithTimeout(
		func() string { return t.runSync(workspace, limit) },
		wsSessionsTimeout,
		"workspace_sessions: timed out reading session or stats data",
	)
	return result, nil
}

// runSync performs the actual work: reads session files and queries the stats
// DB. Called inside runWithTimeout so it never blocks the caller past
// wsSessionsTimeout. It holds no Go mutexes and acquires only one OS-level
// resource at a time (the session-dir flock, then a fresh read-only DB
// connection), so no deadlock is possible.
func (t *WorkspaceSessions) runSync(workspace string, recentLimit int) string {
	now := time.Now()

	// ── 1. Active sessions for this workspace ──────────────────────────────
	all, err := session.List()
	var peers []session.Info
	if err == nil {
		for _, s := range all {
			// Normalise both with filepath.Clean to tolerate trailing slashes
			// or redundant separators in either side.
			if filepath.Clean(s.Folder) == filepath.Clean(workspace) {
				peers = append(peers, s)
			}
		}
	}

	// ── 2. Recent writes from the stats DB (read-only connection) ──────────
	var writes []stats.RecentCall
	if db, dbErr := stats.SharedReadOnly(); dbErr == nil && db != nil {
		writes, _ = db.RecentWritesByWorkspace(workspace, writeToolNames, recentLimit)
	}

	return formatWorkspaceSessions(workspace, t.selfSessID, peers, writes, now)
}

// formatWorkspaceSessions renders the result string. Pure function with no I/O.
func formatWorkspaceSessions(workspace, selfSessID string, peers []session.Info, writes []stats.RecentCall, now time.Time) string {
	var sb strings.Builder

	// ── my session ─────────────────────────────────────────────────────────
	myName := "(unknown)"
	for _, p := range peers {
		if p.ID == selfSessID {
			myName = p.Name
			break
		}
	}
	fmt.Fprintf(&sb, "you:  %s\n", myName)

	// ── active sessions ────────────────────────────────────────────────────
	alone := len(peers) <= 1
	if alone {
		sb.WriteString("\nactive sessions: you are the only active session on this workspace.\n")
		sb.WriteString("  Your view of the project is authoritative — no concurrent agents.\n")
	} else {
		fmt.Fprintf(&sb, "\nactive sessions: %d (including you)\n", len(peers))
		for _, p := range peers {
			isSelf := p.ID == selfSessID
			selfMark := ""
			if isSelf {
				selfMark = " (you)"
			}
			idle := ""
			if !p.LastSeenAt.IsZero() {
				age := now.Sub(p.LastSeenAt)
				if age > session.IdleSessionThreshold {
					idle = fmt.Sprintf(" — idle %s", humaniseAge(age))
				} else {
					idle = fmt.Sprintf(" — last seen %s ago", humaniseAge(age))
				}
			}
			client := ""
			if p.ClientName != "" {
				client = fmt.Sprintf(" [%s]", p.ClientName)
			}
			fmt.Fprintf(&sb, "  %s%s%s%s%s\n", p.Name, selfMark, client, sessionLSP(p), idle)
		}
	}

	// ── recent writes ──────────────────────────────────────────────────────
	if len(writes) == 0 {
		sb.WriteString("\nrecent writes: none recorded on this workspace yet.\n")
		return sb.String()
	}
	sb.WriteString("\nrecent writes (newest first):\n")
	for _, w := range writes {
		age := humaniseAge(now.Sub(w.CalledAt))
		file := fileFromInputJSON(w.InputJSON)
		if file != "" {
			// Show only the path relative to the workspace so long absolute paths
			// don't clutter the output. Fall back to the full path on error.
			if rel, err := filepath.Rel(workspace, file); err == nil && !strings.HasPrefix(rel, "..") {
				file = rel
			}
			fmt.Fprintf(&sb, "  %-20s  %-18s  %s  (%s ago)\n", w.SessionName, w.Tool, file, age)
		} else {
			fmt.Fprintf(&sb, "  %-20s  %-18s  (%s ago)\n", w.SessionName, w.Tool, age)
		}
	}

	return sb.String()
}

// sessionLSP renders a session's active language servers as a compact " · LSP
// gopls, vscode-html-language-server" suffix, so a peer entry shows every server
// the session is driving (a multi-language root, e.g. Go + HTML, runs several).
// Falls back to the single primary Adapter for older session records; empty when
// the session has no LSP.
func sessionLSP(p session.Info) string {
	adapters := p.Adapters
	if len(adapters) == 0 && p.Adapter != "" {
		adapters = []string{p.Adapter}
	}
	if len(adapters) == 0 {
		return ""
	}
	return " · LSP " + strings.Join(adapters, ", ")
}

// fileFromInputJSON extracts the first file path from a tool call's input JSON.
// It handles the common shapes used by write/edit/rename/delete tools:
//   - {"file_path": "…"}    — write_file, edit_file, delete_file
//   - {"from": "…"}         — rename_file, copy_file
//   - {"operations": [{"file_path": "…"}]}  — transaction_apply
//
// Returns "" when no path can be extracted (JSON malformed, wrong shape, or
// a git/find_replace call whose primary target is not a single path).
func fileFromInputJSON(raw string) string {
	if raw == "" {
		return ""
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "from", "path"} {
		if v, ok := m[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	// transaction_apply: first op's file_path
	if ops, ok := m["operations"]; ok {
		var list []map[string]json.RawMessage
		if json.Unmarshal(ops, &list) == nil && len(list) > 0 {
			if v, ok := list[0]["file_path"]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil && s != "" {
					return s
				}
			}
		}
	}
	return ""
}
