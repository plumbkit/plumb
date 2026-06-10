package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// session_start is split across files by concern: the orientation-packet
// section builders live in session_start_sections.go; the client-specific tool
// guidance in session_start_guidance.go; filesystem / git / language-detection
// helpers in session_start_detect.go. This file holds the Tool surface, the
// optional-dependency wiring (With* methods), and workspace resolution.

var sessionStartSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "workspace": {
      "type": "string",
      "description": "Absolute workspace path. Use this to pin the project for clients that do not report a folder (e.g. Claude Desktop). If this connection is already pinned to a different project, passing a workspace here re-pins it to the new project — this is how you switch projects on a connection reused across conversations. Defaults to the daemon's already-resolved workspace."
    },
    "session_id": {
      "type": "string",
      "description": "Optional opaque identifier linking this plumb session to the caller's own session (e.g. a Claude Code conversation ID). When provided, plumb persists the ID and, if a recent session with the same ID ended within the last 24 h, inherits its name — so a resumed conversation keeps its session name in the TUI."
    }
  },
  "additionalProperties": false
}`)

// contextMDLines bounds how much of .plumb/context.md is inlined into the
// session_start response. 200 lines is generous enough for a project bible
// but keeps the output well under the MCP message size limit.
const contextMDLines = 200

// RootsResolver asks the MCP client for its workspace roots (via roots/list)
// and returns the first one as an absolute path, or "" if unavailable. It is
// the last fallback in session_start's workspace resolution chain:
// daemon-resolved workspace → explicit argument → roots/list.
type RootsResolver func(ctx context.Context) string

// SessionStart is a bootstrap tool — call it first in every session to get
// oriented. Accepts an optional session_id to link the plumb session to the
// caller's own session across reconnects (see WithExternalID). It returns in
// one round-trip:
//   - workspace path, detected language, current git branch
//   - first 200 lines of .plumb/context.md (if it exists)
//   - names and descriptions of all memories
//   - top-5 most-used tools from session history
//   - 5 most recently-modified files (workspace-relative)
//   - 3 most recent git commits (subject only)
//   - the live, resolved git tool policy (writes/destructive/push)
//   - active LSP diagnostics (errors and warnings only)
//
// Workspace resolution chain (each falls back to the next on empty):
//  1. the daemon's already-attached root (authoritative — onBeforeTool attaches
//     it before Execute, including from this call's own `workspace` arg)
//  2. explicit `workspace` argument
//  3. roots/list query to the MCP client
//
// There is deliberately no os.Getwd() fallback: in the shared daemon the
// working directory is not a per-session signal, and guessing it reported the
// wrong project.
type SessionStart struct {
	ws           WorkspaceFn
	diag         diagnosticsSource                                           // may be nil; diagnostics section skipped when nil
	roots        RootsResolver                                               // may be nil; roots/list fallback skipped when nil
	refuseFn     func() bool                                                 // may be nil; treated as false (no refusal)
	clientNameFn func() string                                               // may be nil; returns current MCP client name
	topo         topologyStoreFn                                             // may be nil; returns the live topology store, or nil when disabled
	gitPolicyFn  func() GitPolicy                                            // may be nil; git policy section skipped when nil
	lspLangFn    func() string                                               // may be nil; the LSP language attached to this session ("" when none)
	externalIDFn func(id string) string                                      // may be nil; links session to external ID, returns inherited name
	pinConflict  func(requested string)                                      // may be nil; records a same-connection workspace switch attempt
	repin        func(ctx context.Context, workspace string) (string, error) // may be nil; re-pins the connection to an explicit workspace
	episodicFn   func(ws string) (string, bool)                              // may be nil; returns the last episodic summary for the workspace
}

// WithEpisodic wires an accessor for the most recent episodic summary, surfaced
// as a "Last session" block. Nil-safe: unset or returning ok=false omits it.
func (t *SessionStart) WithEpisodic(fn func(ws string) (string, bool)) *SessionStart {
	t.episodicFn = fn
	return t
}

// WithTopology wires the topology store accessor so session_start can lead its
// tool guidance with topology (the Map) when the index is active for the
// workspace. Nil-safe: when unset or returning nil, the guidance falls back to
// the LSP-led form. Returns the receiver for chaining.
func (t *SessionStart) WithTopology(fn topologyStoreFn) *SessionStart {
	t.topo = fn
	return t
}

// topologyActive reports whether a topology store is wired and live.
func (t *SessionStart) topologyActive() bool {
	return t.topo != nil && t.topo() != nil
}

// writeSessionEpisodic appends a compact "Last session" block summarising the
// previous idle session's activity, when one is available.
func (t *SessionStart) writeSessionEpisodic(sb *strings.Builder, ws string) {
	if t.episodicFn == nil {
		return
	}
	text, ok := t.episodicFn(ws)
	if !ok || text == "" {
		return
	}
	sb.WriteString("\n## Last session\n\n")
	sb.WriteString(text)
	sb.WriteString("\n")
}

// WithLSPLanguage wires an accessor for the LSP language actually attached to
// this session ("" when no language server is attached). It lets session_start
// tell "LSP is available" apart from a marker-detected project whose server is
// off or absent, instead of assuming a server exists whenever a diagnostics
// source is wired (which it always is). Nil-safe. Returns the receiver.
func (t *SessionStart) WithLSPLanguage(fn func() string) *SessionStart {
	t.lspLangFn = fn
	return t
}

// WithExternalID wires the external-ID linker: fn receives the session_id
// argument, persists it on the session file, and may return an inherited
// session name (non-empty when a matching ended session was found). Nil-safe.
// Returns the receiver for chaining.
func (t *SessionStart) WithExternalID(fn func(id string) string) *SessionStart {
	t.externalIDFn = fn
	return t
}

// WithPinConflict wires a callback invoked when the caller asks session_start
// to switch an already-pinned connection to a different workspace. The tool
// still returns an error; the callback is for session health/observability.
func (t *SessionStart) WithPinConflict(fn func(requested string)) *SessionStart {
	t.pinConflict = fn
	return t
}

// WithRepin wires the deliberate workspace-switch callback. When the connection
// is already pinned and the caller passes an explicit `workspace` that differs,
// session_start re-pins the connection to it (via fn) instead of refusing. fn
// returns the resolved root. Nil-safe: with no callback wired, session_start
// falls back to the historical "start a new connection" refusal. Returns the
// receiver for chaining.
func (t *SessionStart) WithRepin(fn func(ctx context.Context, workspace string) (string, error)) *SessionStart {
	t.repin = fn
	return t
}

// lspAttached reports whether a language server is attached for this session.
func (t *SessionStart) lspAttached() bool {
	return t.lspLangFn != nil && t.lspLangFn() != ""
}

// NewSessionStart wires the bootstrap tool. refuseHomeRoots is consulted
// before any directory walks under the resolved workspace — it should return
// the current value of walk.refuse_home_roots so live config changes are
// honoured. Pass nil to disable the guard. clientName returns the MCP client
// name negotiated during connection initialisation; pass nil to omit
// client-specific guidance. gitPolicy returns the live, resolved git tool
// policy so session_start can report up front whether commits run through the
// git tool; pass nil to omit the git policy section.
func NewSessionStart(ws WorkspaceFn, diag diagnosticsSource, roots RootsResolver, refuseHomeRoots func() bool, clientName func() string, gitPolicy func() GitPolicy) *SessionStart {
	return &SessionStart{ws: ws, diag: diag, roots: roots, refuseFn: refuseHomeRoots, clientNameFn: clientName, gitPolicyFn: gitPolicy}
}

func (*SessionStart) Name() string { return "session_start" }

func (*SessionStart) Description() string {
	return "Bootstrap tool — call this first at the start of every session. " +
		"Returns one-shot orientation: workspace path, language, current git branch, " +
		"first 200 lines of .plumb/context.md, all saved memory names/descriptions, " +
		"top-5 most-used tools, 5 most recently-modified files, 3 most recent commits, " +
		"the live git tool policy (whether commits/destructive/push are enabled), " +
		"and any active LSP errors/warnings. If no workspace is resolved yet, pass an " +
		"absolute `workspace` to pin it — clients like Claude Desktop do not report the " +
		"folder automatically. Idempotent — safe to call multiple times."
}

func (*SessionStart) InputSchema() json.RawMessage { return sessionStartSchema }

func (t *SessionStart) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	ws, repinnedFrom, err := t.resolveSessionWorkspace(ctx, raw)
	if err != nil {
		return "", err
	}
	var inheritedName string
	if t.externalIDFn != nil {
		var a struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(raw, &a); err == nil && a.SessionID != "" {
			inheritedName = t.externalIDFn(a.SessionID)
		}
	}
	lang, lspKey := detectLanguageInfo(ws)
	hasErrors := t.hasActiveDiagnosticErrors()
	var sb strings.Builder
	t.writeSessionIdentity(&sb, ws, lang, inheritedName, repinnedFrom)
	t.writeSessionRecommendedStart(&sb, hasErrors, lang, lspKey)
	writeSessionContext(&sb, ws)
	writeSessionCommits(&sb, ws)
	writeSessionWorkingTree(&sb, ws)
	t.writeSessionGitPolicy(&sb, ws)
	recent := t.writeSessionRecentFiles(&sb, ws)
	writeSessionMemories(&sb, ws, recent)
	t.writeSessionEpisodic(&sb, ws)
	clientName := ""
	if t.clientNameFn != nil {
		clientName = t.clientNameFn()
	}
	writeSessionStats(&sb, ws, clientName)
	t.writeSessionGuidance(&sb)
	t.writeSessionDiagnostics(&sb)
	return sb.String(), nil
}

// resolveSessionWorkspace resolves the workspace for this call. repinnedFrom is
// the previous root when an explicit `workspace` argument switched an
// already-pinned connection to a different project; it is empty otherwise.
func (t *SessionStart) resolveSessionWorkspace(ctx context.Context, raw json.RawMessage) (ws string, repinnedFrom string, err error) {
	var a struct {
		Workspace string `json:"workspace"`
	}
	_ = json.Unmarshal(raw, &a)
	// The daemon's attached root is authoritative. onBeforeTool resolves and
	// attaches the workspace — including from this call's own `workspace` arg
	// (seedPathFromArgs reads it) — before Execute runs, so preferring it keeps
	// the displayed workspace consistent with the TUI, memory, and topology.
	if t.ws != nil {
		if current := t.ws(); current != "" {
			if a.Workspace != "" && !sameDir(a.Workspace, current) {
				return t.repinExplicit(ctx, current, a.Workspace)
			}
			return current, "", nil
		}
	}
	// Not attached yet: honour an explicit arg, then ask the client for roots.
	// There is no daemon-cwd fallback — the daemon's working directory is never
	// a reliable per-session signal (it is shared across all connections), and
	// guessing it produced confidently-wrong "workspaces".
	if a.Workspace != "" {
		return a.Workspace, "", nil
	}
	if t.roots != nil {
		if ws := t.roots(ctx); ws != "" {
			return ws, "", nil
		}
	}
	return "", "", noWorkspaceError()
}

// repinExplicit switches an already-pinned connection to a different workspace
// when the caller passes an explicit `workspace` argument. A deliberate
// session_start argument is an unambiguous intent to work elsewhere, so plumb
// honours it (tearing down and re-attaching the new root) instead of refusing —
// otherwise a connection reused across conversations stays welded to the first
// project it touched, with no in-session escape. When no re-pin callback is
// wired (older wiring / tests), it falls back to the historical refusal.
func (t *SessionStart) repinExplicit(ctx context.Context, current, requested string) (string, string, error) {
	if t.repin == nil {
		if t.pinConflict != nil {
			t.pinConflict(requested)
		}
		return "", "", fmt.Errorf(
			"session_start: workspace is already pinned to %s — cannot re-pin to %s in the same connection. To switch projects, start a new MCP connection",
			current, requested,
		)
	}
	newRoot, err := t.repin(ctx, requested)
	if err != nil {
		return "", "", fmt.Errorf("session_start: re-pinning to %s: %w", requested, err)
	}
	return newRoot, current, nil
}
