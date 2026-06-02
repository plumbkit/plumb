package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/golimpio/plumb/internal/fsguard"
	"github.com/golimpio/plumb/internal/lsp/protocol"
	"github.com/golimpio/plumb/internal/memory"
	"github.com/golimpio/plumb/internal/stats"
)

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
	t.writeSessionGitPolicy(&sb, ws)
	t.writeSessionRecentFiles(&sb, ws)
	writeSessionMemories(&sb, ws)
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
			if a.Workspace != "" && filepath.Clean(a.Workspace) != filepath.Clean(current) {
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

func (t *SessionStart) writeSessionIdentity(sb *strings.Builder, ws, lang, inheritedName, repinnedFrom string) {
	fmt.Fprintf(sb, "# Workspace: %s\n\n", ws)
	if repinnedFrom != "" {
		fmt.Fprintf(sb, "Re-pinned this connection: %s → %s\n\n", repinnedFrom, ws)
	}
	if lang != "" {
		fmt.Fprintf(sb, "Language: %s\n", lang)
	}
	if branch := gitBranch(ws); branch != "" {
		fmt.Fprintf(sb, "Branch:   %s\n", branch)
	}
	refuse := t.refuseFn != nil && t.refuseFn()
	if skip, _ := fsguard.RefuseWalk(ws, refuse); !skip {
		if scale := workspaceScale(ws, lang); scale != "" {
			fmt.Fprintf(sb, "Scale:    %s\n", scale)
		}
	}
	if inheritedName != "" {
		fmt.Fprintf(sb, "Session:  %s (resumed)\n", inheritedName)
	}
	sb.WriteString("\n")
}

func (t *SessionStart) writeSessionRecommendedStart(sb *strings.Builder, hasErrors bool, lang, lspKey string) {
	sb.WriteString("## Recommended first step\n\n")
	switch {
	case hasErrors:
		sb.WriteString("Active errors detected — start with `diagnostics` to review them.\n\n")
	case t.lspAttached():
		sb.WriteString("LSP is available — use `workspace_symbols` to survey the codebase.\n\n")
	case t.topologyActive():
		sb.WriteString("No language server is attached, but the topology index is active — use " +
			"`topology_search` and `file_outline` for discovery and structure. " +
			"(`get_definition`/`find_references` still need a language server.)\n\n")
	case lang != "":
		t.writeNoLSPGuidance(sb, lang, lspKey)
	default:
		sb.WriteString("Use `list_files` to explore the codebase.\n\n")
	}
}

// writeNoLSPGuidance covers a recognised project with neither a language server
// nor a topology index — the case that misled a Java agent into thinking the
// semantic tools were broken. It names the concrete next step rather than
// silently advertising LSP tools that will error.
func (t *SessionStart) writeNoLSPGuidance(sb *strings.Builder, lang, lspKey string) {
	fmt.Fprintf(sb, "No language server is attached for %s. ", lang)
	switch lspKey {
	case "":
		sb.WriteString("plumb has no language server for it yet — use `search_in_files` and `list_files`, " +
			"or enable the topology index (`[topology] enabled = true`) for indexed symbol search.\n\n")
	case "go", "python":
		sb.WriteString("Its server ships on by default, so it likely isn't installed or failed to start — " +
			"check the server binary is on PATH. Meanwhile use `search_in_files`/`list_files`, or enable " +
			"`[topology] enabled = true` for indexed search.\n\n")
	default:
		fmt.Fprintf(sb, "Its adapter is opt-in — set `[lsp.%s] enabled = true` and ensure the server is on PATH. "+
			"For language-server-free symbol search, enable the topology index (`[topology] enabled = true`).\n\n", lspKey)
	}
}

func (t *SessionStart) hasActiveDiagnosticErrors() bool {
	if t.diag == nil {
		return false
	}
	for _, diags := range t.diag.AllDiagnostics() {
		for _, d := range diags {
			if d.Severity == protocol.SevError {
				return true
			}
		}
	}
	return false
}

func writeSessionContext(sb *strings.Builder, ws string) {
	data, err := os.ReadFile(filepath.Join(ws, ".plumb", "context.md"))
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	truncated := len(lines) > contextMDLines
	if truncated {
		lines = lines[:contextMDLines]
	}
	sb.WriteString("## Project context (.plumb/context.md)\n\n")
	sb.WriteString(strings.Join(lines, "\n"))
	if truncated {
		fmt.Fprintf(sb, "\n… (truncated at %d lines — use read_file to see the rest)\n", contextMDLines)
	}
	sb.WriteString("\n\n")
}

func writeSessionCommits(sb *strings.Builder, ws string) {
	commits := gitRecentCommits(ws, 3)
	if len(commits) == 0 {
		return
	}
	sb.WriteString("## Recent commits\n\n")
	for _, c := range commits {
		fmt.Fprintf(sb, "- %s\n", c)
	}
	sb.WriteString("\n")
}

// writeSessionGitPolicy reports the connection's live, resolved git tool policy
// so an agent learns up front whether it can commit through the git tool —
// rather than discovering it via a rejected call or, worse, trusting a stale
// memory and shelling out. Nil-safe (skipped when unwired) and only emitted
// inside a git repository (gitBranch is the cheap repo-presence signal).
func (t *SessionStart) writeSessionGitPolicy(sb *strings.Builder, ws string) {
	if t.gitPolicyFn == nil || gitBranch(ws) == "" {
		return
	}
	sb.WriteString("## Git (via the `git` tool — live policy)\n\n")
	sb.WriteString(formatGitPolicy(t.gitPolicyFn()))
	sb.WriteString("\n")
}

// formatGitPolicy renders the git policy body. Pure — no I/O. The closing line
// is always present so a stale "git is read-only" assumption is contradicted at
// the point of orientation.
func formatGitPolicy(p GitPolicy) string {
	var sb strings.Builder
	if p.AllowWrites {
		sb.WriteString("Commits & staging ENABLED — commit through the `git` tool, not the shell.\n")
		fmt.Fprintf(&sb, "Destructive (reset/checkout/rebase): %s.\n", gitGateLabel(p.AllowDestructive))
		fmt.Fprintf(&sb, "Push/fetch/pull: %s.\n", gitGateLabel(p.AllowPush))
		if len(p.ProtectedBranches) > 0 {
			fmt.Fprintf(&sb, "Protected branches: %s.\n", strings.Join(p.ProtectedBranches, ", "))
		}
	} else {
		sb.WriteString("Read-only — status/log/diff/show/blame run; commits and other writes are disabled (`[git] allow_writes = false`).\n")
	}
	sb.WriteString("\nThis is the resolved policy for this session — trust it over any cached note.\n")
	return sb.String()
}

// gitGateLabel renders a git policy gate flag as the on/off word used in the
// session_start report.
func gitGateLabel(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}

// writeSessionRecentFiles lists the 5 most recently modified files.
// Skips the walk if fsguard identifies ws as a protected macOS root (e.g.
// $HOME) — touching those would surface a TCC prompt attributed to plumb.
func (t *SessionStart) writeSessionRecentFiles(sb *strings.Builder, ws string) {
	refuse := t.refuseFn != nil && t.refuseFn()
	if skip, reason := fsguard.RefuseWalk(ws, refuse); skip {
		slog.Info("session_start: skipping recent-files walk", "workspace", ws, "reason", reason)
		return
	}
	files := recentlyModifiedFiles(ws, 5)
	if len(files) == 0 {
		return
	}
	sb.WriteString("## Recently modified files\n\n")
	for _, f := range files {
		fmt.Fprintf(sb, "- %s\n", f)
	}
	sb.WriteString("\n")
}

func writeSessionMemories(sb *strings.Builder, ws string) {
	mems, err := memory.List(ws)
	if err != nil {
		return
	}
	if len(mems) == 0 {
		sb.WriteString("## Memories\n\nNone yet. Use write_memory to save project notes.\n\n")
		return
	}
	fmt.Fprintf(sb, "## Memories (%d)\n\n", len(mems))
	for _, m := range mems {
		fmt.Fprintf(sb, "- **%s**", m.Name)
		if m.Description != "" {
			fmt.Fprintf(sb, " — %s", m.Description)
		}
		fmt.Fprintf(sb, " (%d bytes)\n", m.SizeBytes)
	}
	sb.WriteString("\nUse read_memory to load any of these.\n\n")
}

func writeSessionStats(sb *strings.Builder, ws, clientName string) {
	db, err := stats.OpenReadOnly()
	if err != nil || db == nil {
		return
	}
	defer db.Close()
	toolStats, err := db.Summary(stats.Filter{Workspace: ws})
	if err != nil || len(toolStats) == 0 {
		return
	}
	sb.WriteString("## Most-used tools (this workspace)\n\n")
	limit := min(len(toolStats), 5)
	var totalSaved int64
	for _, s := range toolStats[:limit] {
		fmt.Fprintf(sb, "- %s: %d calls, avg %dms, p95 %dms\n", s.Tool, s.Calls, int64(s.AvgMs), s.P95Ms)
		totalSaved += s.TokensSaved
	}
	if totalSaved > 0 {
		fmt.Fprintf(sb, "\n~%s %s\n", stats.FormatSavings(int(totalSaved)), stats.SavingsLabel(clientName))
	}
	sb.WriteString("\n")
}

func (t *SessionStart) writeSessionGuidance(sb *strings.Builder) {
	switch {
	case isClaudeCode(t.clientNameFn):
		t.writeClaudeCodeGuidance(sb)
	case isClaudeDesktop(t.clientNameFn):
		t.writeClaudeDesktopGuidance(sb)
	}
}

// writeClaudeCodeGuidance leads with topology (the Map) for discovery / structure
// / impact when the index is active, then the LSP-semantic tools (the GPS) for
// precise navigation. When topology is off it falls back to the LSP-led form
// with a one-line pointer to enabling the index.
func (t *SessionStart) writeClaudeCodeGuidance(sb *strings.Builder) {
	sb.WriteString("## Tool guidance (Claude Code)\n\n")
	sb.WriteString(nativeEditLaneWarning)
	if t.topologyActive() {
		sb.WriteString("Two complementary layers. **Topology (the Map)** is the primary path for " +
			"discovery, structure, and impact — it answers instantly, tolerates broken code, and " +
			"covers every indexed language. **LSP (the GPS)** is for precise, type-aware navigation " +
			"once you know where to work.\n\n")
		sb.WriteString("Topology — start here for where / what / what-if:\n\n")
		sb.WriteString("- **topology_affected** — THE post-change tool: which tests to run after an edit " +
			"(dependency edges + co-location, recall-biased, confidence-labelled). No language server gives this.\n")
		sb.WriteString("- **topology_search** — ranked symbol/file search across the index. Use over grep for discovery.\n")
		sb.WriteString("- **topology_explore** / **topology_impact** — neighbourhood and blast radius around a symbol.\n")
		sb.WriteString("- **file_outline** — a file's shape (signatures, bodies collapsed) in ~200 tokens.\n")
		sb.WriteString("- **topology_routes** — framework entry points (HTTP handlers, Cobra, Flask).\n\n")
		sb.WriteString("LSP-semantic — precise navigation (Claude Code lacks these natively):\n\n")
		sb.WriteString("- **get_definition** / **find_references** — exact definition and all call sites (scope-aware, not text search).\n")
		sb.WriteString("- **rename_symbol** — workspace-wide semantic rename.\n")
		sb.WriteString("- **call_hierarchy** / **type_hierarchy** — callers/callees, super/subtypes.\n")
		sb.WriteString("- **diagnostics** — live errors and warnings without running a build.\n\n")
		return
	}
	sb.WriteString("Plumb adds LSP-semantic tools Claude Code lacks natively:\n\n")
	sb.WriteString("- **workspace_symbols** — find a symbol by name instantly (LSP index). Use instead of grep/search_in_files for name lookups.\n")
	sb.WriteString("- **find_references** — all call sites for a symbol (LSP-semantic, not text search). Accepts name or position.\n")
	sb.WriteString("- **get_definition** — jump to definition by name or position without reading files first.\n")
	sb.WriteString("- **call_hierarchy** — callers and callees of a function.\n")
	sb.WriteString("- **type_hierarchy** — supertypes and subtypes of a class or interface.\n")
	sb.WriteString("- **rename_symbol** — workspace-wide LSP rename (updates all references; safer than find+replace).\n")
	sb.WriteString("- **list_symbols** with include_signatures=true — outline a file without reading it.\n")
	sb.WriteString("- **diagnostics** — live LSP errors and warnings without running a build.\n\n")
	sb.WriteString("Tip: enable the topology index (`[topology] enabled = true` in `.plumb/config.toml`) to add " +
		"ranked search, file outlines, and `topology_affected` — which tests to run after a change.\n\n")
}

func (t *SessionStart) writeClaudeDesktopGuidance(sb *strings.Builder) {
	sb.WriteString("## Tool guidance (Claude Desktop)\n\n")
	sb.WriteString("**Pin your project first.** plumb cannot detect which folder you are working in — " +
		"Claude Desktop does not report it, and the daemon is shared across conversations. If the " +
		"workspace shown above is wrong or unresolved, call `session_start` again with an explicit " +
		"absolute path, e.g. `session_start({\"workspace\": \"/Users/you/projects/myapp\"})` (passing " +
		"`workspace` or an absolute `path` to any tool also pins it). Until then, file operations may " +
		"target the wrong project.\n\n")
	sb.WriteString("Claude Desktop has no native filesystem or shell tools. Plumb is your only interface to the codebase.\n\n")
	sb.WriteString("**All file operations go through plumb** — there is no fallback:\n\n")
	sb.WriteString("- **read_file** / **read_multiple_files** — read any file or slice of a file.\n")
	sb.WriteString("- **write_file** / **edit_file** — create or modify files atomically.\n")
	sb.WriteString("- **list_files** / **find_files** / **search_in_files** — discover and search the codebase.\n")
	sb.WriteString("- **git** — read-only git queries (status, log, diff, blame).\n\n")
	if t.topologyActive() {
		sb.WriteString("**Topology (the Map)** — in-process, always-on structural index:\n\n")
		sb.WriteString("- **topology_affected** — which tests to run after a change (the headline answer).\n")
		sb.WriteString("- **topology_search** — ranked symbol/file discovery across the index.\n")
		sb.WriteString("- **file_outline** — a file's shape in ~200 tokens without reading it.\n\n")
	}
	sb.WriteString("**LSP-semantic tools** (no equivalent without a language server):\n\n")
	sb.WriteString("- **workspace_symbols** — find any symbol by name across the workspace instantly.\n")
	sb.WriteString("- **find_references** — all call sites for a symbol (scope-aware, not text search).\n")
	sb.WriteString("- **get_definition** — jump to definition without reading the file first.\n")
	sb.WriteString("- **rename_symbol** — workspace-wide semantic rename across all files.\n")
	sb.WriteString("- **diagnostics** — live compile errors and warnings from the language server.\n\n")
	sb.WriteString("If a plumb tool fails, retry or check `daemon_info`. Do not attempt native shell commands — they are unavailable.\n\n")
}

func (t *SessionStart) writeSessionDiagnostics(sb *strings.Builder) {
	if t.diag == nil {
		return
	}
	all := t.diag.AllDiagnostics()
	filtered := make(map[string][]protocol.Diagnostic)
	for uri, diags := range all {
		for _, d := range diags {
			if d.Severity <= protocol.SevWarning {
				filtered[uri] = append(filtered[uri], d)
			}
		}
	}
	if len(filtered) == 0 {
		return
	}

	// Gopls emits "not in your go.mod file" at go.mod:1:1 when the module cache
	// is cold — packages declared in go.mod but not yet downloaded. Collapse
	// these to a single advisory line so real errors are not buried.
	real, coldCount := partitionColdCacheGoMod(filtered)

	sb.WriteString("## Active diagnostics (errors and warnings)\n\n")
	if len(real) > 0 {
		// Flag entries whose file mtime is newer than the last publishDiagnostics:
		// the orientation packet is the most likely place to surface diagnostics
		// gopls produced before reconciling in-flight edits. Mirrors the diagnostics
		// tool's opt-in path. (Catches "edited after analysis"; a fresh-timestamp
		// analysis against a cold module cache is handled by the go.mod partition
		// above.)
		if ts, ok := t.diag.(timedDiagnosticsSource); ok {
			sb.WriteString(formatDiagnosticsWithTimes(real, ts.AllDiagnosticTimes()))
		} else {
			sb.WriteString(formatDiagnostics(real))
		}
	}
	if coldCount > 0 {
		sep := ""
		if len(real) > 0 {
			sep = "\n"
		}
		fmt.Fprintf(sb, "%sNote: %d go.mod package(s) flagged \"not in your go.mod file\" at 1:1 — "+
			"likely a cold module cache; run `go mod tidy`.\n", sep, coldCount)
	}
	sb.WriteString("\n")
}

// partitionColdCacheGoMod splits diagnostics into real issues and cold-cache
// false positives. Cold-cache entries match: URI ends with /go.mod, position
// is 1:1 (0-indexed line 0 col 0), and message ends with "is not in your go.mod file".
func partitionColdCacheGoMod(byURI map[string][]protocol.Diagnostic) (real map[string][]protocol.Diagnostic, coldCount int) {
	real = make(map[string][]protocol.Diagnostic)
	for uri, diags := range byURI {
		if !strings.HasSuffix(uri, "/go.mod") {
			real[uri] = diags
			continue
		}
		var kept []protocol.Diagnostic
		for _, d := range diags {
			if d.Range.Start.Line == 0 && d.Range.Start.Character == 0 &&
				strings.HasSuffix(d.Message, "is not in your go.mod file") {
				coldCount++
			} else {
				kept = append(kept, d)
			}
		}
		if len(kept) > 0 {
			real[uri] = kept
		}
	}
	return real, coldCount
}

// workspaceScale returns a human-readable file-count summary for the workspace
// identity section, e.g. "~342 files (287 Go)".
func workspaceScale(ws, lang string) string {
	exts, label := langFileProfile(lang)
	total, langCount := countWorkspaceFiles(ws, exts)
	if total == 0 {
		return ""
	}
	if label != "" && langCount > 0 {
		return fmt.Sprintf("~%d files (%d %s)", total, langCount, label)
	}
	return fmt.Sprintf("~%d files", total)
}

// langFileProfile returns the primary source-file extensions and a short
// display label for a detected language name.
func langFileProfile(lang string) (exts []string, label string) {
	switch lang {
	case "Go":
		return []string{".go"}, "Go"
	case "Python":
		return []string{".py"}, "Python"
	case "TypeScript":
		return []string{".ts", ".tsx"}, "TypeScript"
	case "JavaScript":
		return []string{".js", ".mjs", ".cjs", ".jsx"}, "JavaScript"
	case "JavaScript/TypeScript":
		return []string{".ts", ".js", ".tsx", ".jsx"}, "JS/TS"
	case "Rust":
		return []string{".rs"}, "Rust"
	case "Swift":
		return []string{".swift"}, "Swift"
	case "Zig":
		return []string{".zig"}, "Zig"
	case "Kotlin":
		return []string{".kt", ".kts"}, "Kotlin"
	case "Java (Maven)":
		return []string{".java"}, "Java"
	case "Java/Kotlin (Gradle)":
		return []string{".java", ".kt"}, "Java/Kotlin"
	case "C/C++ (CMake)":
		return []string{".c", ".cpp", ".cc", ".h", ".hpp"}, "C/C++"
	case "Elixir":
		return []string{".ex", ".exs"}, "Elixir"
	case "Ruby":
		return []string{".rb"}, "Ruby"
	default:
		return nil, ""
	}
}

// countWorkspaceFiles walks ws and returns the total file count and the count
// of files matching the given extensions. Skips .git, node_modules, vendor,
// dist, build, and hidden directories.
func countWorkspaceFiles(ws string, exts []string) (total, langCount int) {
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		extSet[e] = true
	}
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	}
	_ = filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			n := info.Name()
			if skipDirs[n] || (strings.HasPrefix(n, ".") && path != ws) {
				return filepath.SkipDir
			}
			return nil
		}
		total++
		if extSet[filepath.Ext(path)] {
			langCount++
		}
		return nil
	})
	return total, langCount
}

// detectLanguageInfo returns a human-readable language label and, when a plumb
// LSP adapter exists for it, that adapter's config key ([lsp.<key>]). Both are
// "" when no root marker matches; the key alone is "" for a recognised language
// plumb has no server for (C/C++, Elixir, Ruby). The key lets session_start
// name the exact knob to enable when a server is expected but not attached.
func detectLanguageInfo(ws string) (label, key string) {
	markers := []struct {
		file  string
		label string
		key   string
	}{
		{"go.mod", "Go", "go"},
		{"tsconfig.json", "TypeScript", "typescript"},
		{"jsconfig.json", "JavaScript", "typescript"},
		{"package.json", "JavaScript/TypeScript", "typescript"},
		{"Cargo.toml", "Rust", "rust"},
		{"pyproject.toml", "Python", "python"},
		{"setup.py", "Python", "python"},
		{"Package.swift", "Swift", "swift"},
		{"build.zig", "Zig", "zig"},
		{"pom.xml", "Java (Maven)", "java"},
		{"settings.gradle.kts", "Kotlin", "kotlin"},
		{"build.gradle.kts", "Kotlin", "kotlin"},
		{"build.gradle", "Java/Kotlin (Gradle)", "java"},
		{"CMakeLists.txt", "C/C++ (CMake)", ""},
		{"mix.exs", "Elixir", ""},
		{"Gemfile", "Ruby", ""},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(ws, m.file)); err == nil {
			return m.label, m.key
		}
	}
	return "", ""
}

// gitBranch returns the current branch name, or "" if not a git repo / git
// is unavailable. Best-effort with a short timeout.
func gitBranch(ws string) string {
	cmd := exec.Command("git", "-C", ws, "branch", "--show-current")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRecentCommits returns up to n recent commit subjects in "shortsha subject"
// form. Best-effort; returns nil on any error.
func gitRecentCommits(ws string, n int) []string {
	cmd := exec.Command("git", "-C", ws, "log", fmt.Sprintf("-%d", n), "--pretty=format:%h %s")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

// recentlyModifiedFiles returns up to n workspace-relative file paths sorted
// by mtime (newest first). Skips hidden directories, .git, node_modules,
// vendor — the usual noise.
func recentlyModifiedFiles(ws string, n int) []string {
	type fileEntry struct {
		path string
		mod  int64
	}
	var entries []fileEntry
	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, ".idea": true,
	}
	_ = filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if skipDirs[name] || (strings.HasPrefix(name, ".") && path != ws) {
				return filepath.SkipDir
			}
			return nil
		}
		entries = append(entries, fileEntry{path: path, mod: info.ModTime().UnixNano()})
		return nil
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].mod > entries[j].mod })
	if len(entries) > n {
		entries = entries[:n]
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		rel, err := filepath.Rel(ws, e.path)
		if err != nil {
			rel = e.path
		}
		out = append(out, rel)
	}
	return out
}

// isClaudeCode reports whether fn identifies the MCP client as Claude Code.
// Matches "claude-code" exactly or "claude-code/<version>" — a bare HasPrefix
// would falsely match names like "claude-codegen".
func isClaudeCode(fn func() string) bool {
	if fn == nil {
		return false
	}
	n := strings.ToLower(fn())
	return n == "claude-code" || strings.HasPrefix(n, "claude-code/")
}

// isClaudeDesktop reports whether fn identifies the MCP client as Claude Desktop.
// Claude Desktop identifies itself as "claude-ai" (e.g. "claude-ai 0.1.0") over
// MCP, not "claude-desktop" — both are matched so the guidance fires regardless
// of which name a build reports.
func isClaudeDesktop(fn func() string) bool {
	if fn == nil {
		return false
	}
	n := strings.ToLower(fn())
	return n == "claude-ai" || strings.HasPrefix(n, "claude-ai/") ||
		n == "claude-desktop" || strings.HasPrefix(n, "claude-desktop/")
}
