package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/collab"
	"github.com/plumbkit/plumb/internal/session"
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
    },
    "language": {
      "type": "string",
      "description": "Optional override for the workspace's primary language when automatic detection cannot infer it — e.g. an Xcode app that has .swift sources but no SwiftPM Package.swift, so no root marker resolves. Pass the [lsp.<lang>] key (e.g. 'swift', 'typescript', 'rust') to force that language server as the primary, so workspace_symbols and the call/type hierarchies work. The server must be installed and enabled; an unknown, uninstalled, or disabled language is ignored and normal detection applies. Honoured on the connection's current workspace, or alongside an explicit 'workspace' arg."
    },
    "purpose": {
      "type": "string",
      "description": "Optional human-readable tag describing what this session is for (e.g. 'deploy-fix', 'feature-auth'). Surfaced in the TUI session list, daemon_info, and workspace_sessions so an operator can tell concurrent sessions apart. Allowed characters: letters, digits, and hyphens; max 32 characters. An invalid value is rejected with a clear error."
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
//  3. roots/list query to the MCP client (whose resolver falls back to the
//     serve proxy's cwd hint when the client reports no roots)
//
// There is deliberately no daemon-side os.Getwd() fallback: in the shared
// daemon the working directory is not a per-session signal, and guessing it
// reported the wrong project. The per-conversation serve proxy's cwd IS a
// per-session signal, which is why it may enter via the roots resolver above
// — always Detect-validated and never persisted as the sticky pin.
type SessionStart struct {
	ws           WorkspaceFn
	diag         diagnosticsSource                                                     // may be nil; diagnostics section skipped when nil
	roots        RootsResolver                                                         // may be nil; roots/list fallback skipped when nil
	refuseFn     func() bool                                                           // may be nil; treated as false (no refusal)
	clientNameFn func() string                                                         // may be nil; returns current MCP client name
	topo         topologyStoreFn                                                       // may be nil; returns the live topology store, or nil when disabled
	gitPolicyFn  func() GitPolicy                                                      // may be nil; git policy section skipped when nil
	lspLangFn    func() string                                                         // may be nil; the LSP language attached to this session ("" when none)
	lspLangsFn   func() []string                                                       // may be nil; the distinct child languages of a monorepo root (>1 ⇒ multi-language identity line)
	externalIDFn func(id string) string                                                // may be nil; links session to external ID, returns inherited name
	pinConflict  func(requested string)                                                // may be nil; records a same-connection workspace switch attempt
	repin        func(ctx context.Context, workspace, language string) (string, error) // may be nil; re-pins the connection to an explicit workspace, optionally forcing a primary language
	episodicFn   func(ws string) (string, bool)                                        // may be nil; returns the last episodic summary for the workspace
	toolProfile  func() (profile string, hidden int)                                   // may be nil; the resolved tool profile + count of tools hidden from tools/list
	lspWarmingFn func() (bool, time.Duration)                                          // may be nil; reports whether the primary LSP is still warming + elapsed
	purposeFn    func(purpose string)                                                  // may be nil; persists a validated session purpose tag
	selfSessID   string                                                                // this session's ID, excluded from the peer digest
	collabFn     func() (peerAwareness bool, hintBudgetBytes int)                      // may be nil; the resolved [collab] snapshot for the peer digest
	mailboxFn    func() (on bool, store *collab.Store, self string, budgetBytes int)   // may be nil; the phase-2 mailbox delivery snapshot
}

// WithSelfSession records this connection's session ID so the peer digest can
// exclude it from the active-session list. Returns the receiver for chaining.
func (t *SessionStart) WithSelfSession(id string) *SessionStart {
	t.selfSessID = id
	return t
}

// WithCollab wires the resolved [collab] snapshot accessor (peer_awareness +
// hint_budget_bytes) used to gate and bound the session_start peer digest.
// Nil-safe: unset ⇒ the digest is omitted. Returns the receiver for chaining.
func (t *SessionStart) WithCollab(fn func() (bool, int)) *SessionStart {
	t.collabFn = fn
	return t
}

// WithMailbox wires the phase-2 mailbox delivery snapshot: whether [collab]
// mailbox is on, an open-if-exists collab.db accessor, this session's name (the
// note addressee), and the [collab] hint_budget_bytes bound. When on and notes
// await, session_start delivers them (consuming "next" notes) as a "## Messages"
// block. Nil-safe: unwired ⇒ no delivery. Returns the receiver for chaining.
func (t *SessionStart) WithMailbox(fn func() (on bool, store *collab.Store, self string, budgetBytes int)) *SessionStart {
	t.mailboxFn = fn
	return t
}

// WithToolProfile wires an accessor returning the connection's resolved tool
// profile ("lean"/"full") and the number of tools hidden from tools/list under
// it. When the profile is "lean", session_start prepends a terse note that the
// hidden tools stay callable by name. Nil-safe (treated as "full", no note).
func (t *SessionStart) WithToolProfile(fn func() (profile string, hidden int)) *SessionStart {
	t.toolProfile = fn
	return t
}

// resolvedToolProfile returns the profile + hidden count, defaulting to "full"
// (no tools hidden) when no accessor is wired.
func (t *SessionStart) resolvedToolProfile() (string, int) {
	if t.toolProfile == nil {
		return "full", 0
	}
	return t.toolProfile()
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
// tell "LSP is ready" apart from a marker-detected project whose server is
// off or absent, instead of assuming a server exists whenever a diagnostics
// source is wired (which it always is). Nil-safe. Returns the receiver.
func (t *SessionStart) WithLSPLanguage(fn func() string) *SessionStart {
	t.lspLangFn = fn
	return t
}

// WithLSPLanguages wires an accessor for the distinct child languages attached
// to a monorepo workspace root (e.g. zig + swift discovered under one .plumb/
// root). When it returns more than one, session_start renders them as a combined
// "Language: Swift, Zig" identity line; the single primary (WithLSPLanguage)
// still drives the recommended-step guidance. Nil-safe. Returns the receiver.
func (t *SessionStart) WithLSPLanguages(fn func() []string) *SessionStart {
	t.lspLangsFn = fn
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
func (t *SessionStart) WithRepin(fn func(ctx context.Context, workspace, language string) (string, error)) *SessionStart {
	t.repin = fn
	return t
}

// WithPurpose wires the callback that persists a validated session purpose tag
// (set on the session record and stamped on this session's stats rows). Nil-safe:
// with no callback wired, a supplied purpose is validated but not persisted.
// Returns the receiver for chaining.
func (t *SessionStart) WithPurpose(fn func(purpose string)) *SessionStart {
	t.purposeFn = fn
	return t
}

// lspAttached reports whether a language server is attached for this session.
func (t *SessionStart) lspAttached() bool {
	return t.lspLangFn != nil && t.lspLangFn() != ""
}

// WithLSPWarmup wires an accessor reporting whether the session's primary
// language server is still warming (handshake incomplete) and for how long. When
// it reports warming, session_start softens "LSP is ready" into a warming
// advisory that steers the agent to topology/find_symbol meanwhile. Nil-safe:
// unset means never warming. Returns the receiver for chaining.
func (t *SessionStart) WithLSPWarmup(fn func() (bool, time.Duration)) *SessionStart {
	t.lspWarmingFn = fn
	return t
}

// lspWarming reports the primary LSP warm-up state, or (false, 0) when no
// accessor is wired.
func (t *SessionStart) lspWarming() (bool, time.Duration) {
	if t.lspWarmingFn == nil {
		return false, 0
	}
	return t.lspWarmingFn()
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
	if err := t.applyPurpose(raw); err != nil {
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
	// A forced/attached primary may have no root marker (e.g. swift pinned on an
	// Xcode app with no Package.swift), so marker detection returns nothing. Prefer
	// the language actually attached to this session for the display and guidance.
	if t.lspLangFn != nil {
		if attached := t.lspLangFn(); attached != "" && attached != lspKey {
			lang, lspKey = labelForLSPKey(attached), attached
		}
	}
	// Multi-language monorepo root: show every detected language in the identity
	// line (e.g. "Swift, Zig"), while lspKey stays the elected primary so the
	// recommended-step guidance and lspAttached() logic are unchanged.
	if t.lspLangsFn != nil {
		if keys := t.lspLangsFn(); len(keys) > 1 {
			lang = joinLanguageLabels(keys)
		}
	}
	hasErrors := t.hasActiveDiagnosticErrors()
	var sb strings.Builder
	t.writeSessionIdentity(&sb, ws, lang, inheritedName, repinnedFrom)
	t.writeSessionRecommendedStart(&sb, hasErrors, lang, lspKey)
	writeSessionContext(&sb, ws)
	writeSessionCommits(&sb, ws)
	writeSessionWorkingTree(&sb, ws)
	t.writeSessionGitPolicy(&sb, ws)
	writeSessionSubmodules(&sb, ws)
	recent := t.writeSessionRecentFiles(&sb, ws)
	writeSessionMemories(&sb, ws, recent)
	t.writeSessionEpisodic(&sb, ws)
	t.writeSessionPeers(&sb, ws)
	t.writeSessionMessages(&sb, ws)
	writeSessionStats(&sb, ws)
	t.writeSessionGuidance(&sb)
	t.writeSessionDiagnostics(&sb)
	return sb.String(), nil
}

// applyPurpose validates an optional `purpose` argument and, when valid and
// non-empty, persists it via the wired callback. An invalid purpose is rejected
// with a clear error rather than silently dropped, so a malformed tag is a loud
// caller-side bug. A missing or empty purpose is a no-op (the session keeps any
// previously-set tag).
func (t *SessionStart) applyPurpose(raw json.RawMessage) error {
	var a struct {
		Purpose string `json:"purpose"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.Purpose == "" {
		return nil
	}
	purpose, err := session.NormalisePurpose(a.Purpose)
	if err != nil {
		return fmt.Errorf("session_start: invalid purpose: %w", err)
	}
	if purpose != "" && t.purposeFn != nil {
		t.purposeFn(purpose)
	}
	return nil
}

// resolveSessionWorkspace resolves the workspace for this call. repinnedFrom is
// the previous root when an explicit `workspace` argument switched an
// already-pinned connection to a different project; it is empty otherwise.
func (t *SessionStart) resolveSessionWorkspace(ctx context.Context, raw json.RawMessage) (ws string, repinnedFrom string, err error) {
	var a struct {
		Workspace string `json:"workspace"`
		Language  string `json:"language"`
	}
	_ = json.Unmarshal(raw, &a)
	// The daemon's attached root is authoritative. onBeforeTool resolves and
	// attaches the workspace — including from this call's own `workspace` arg
	// (seedPathFromArgs reads it) — before Execute runs, so preferring it keeps
	// the displayed workspace consistent with the TUI, memory, and topology.
	if t.ws != nil {
		if current := t.ws(); current != "" {
			switch {
			case a.Workspace != "" && !sameDir(a.Workspace, current):
				return t.repinExplicit(ctx, current, a.Workspace, a.Language)
			case a.Language != "":
				return t.forceLanguage(ctx, current, a.Language)
			default:
				return current, "", nil
			}
		}
	}
	// Not attached yet: honour an explicit arg, then ask the client for roots.
	// There is no daemon-cwd fallback — the daemon's working directory is never
	// a reliable per-session signal (it is shared across all connections), and
	// guessing it produced confidently-wrong "workspaces".
	if a.Workspace != "" {
		if a.Language != "" && t.repin != nil {
			root, rerr := t.repin(ctx, a.Workspace, a.Language)
			if rerr != nil {
				return "", "", fmt.Errorf("session_start: pinning %s as %s: %w", a.Workspace, a.Language, rerr)
			}
			return root, "", nil
		}
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
func (t *SessionStart) repinExplicit(ctx context.Context, current, requested, language string) (string, string, error) {
	if t.repin == nil {
		if t.pinConflict != nil {
			t.pinConflict(requested)
		}
		return "", "", fmt.Errorf(
			"session_start: workspace is already pinned to %s — cannot re-pin to %s in the same connection. To switch projects, start a new MCP connection",
			current, requested,
		)
	}
	newRoot, err := t.repin(ctx, requested, language)
	if err != nil {
		return "", "", fmt.Errorf("session_start: re-pinning to %s: %w", requested, err)
	}
	// Suppress the "re-pinned" banner when the requested path resolves to the
	// same root (e.g. a subdir of the current project, or a language-only pin):
	// no project switch actually happened.
	from := current
	if sameDir(newRoot, current) {
		from = ""
	}
	return newRoot, from, nil
}

// forceLanguage re-pins the connection's CURRENT workspace to a forced primary
// language (the session_start `language` arg without a project switch). With no
// re-pin callback wired it ignores the override rather than failing — the
// orientation packet must still return.
func (t *SessionStart) forceLanguage(ctx context.Context, current, language string) (string, string, error) {
	if t.repin == nil {
		return current, "", nil
	}
	if _, err := t.repin(ctx, current, language); err != nil {
		return "", "", fmt.Errorf("session_start: pinning language %s: %w", language, err)
	}
	return current, "", nil
}
