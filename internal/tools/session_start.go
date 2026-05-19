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
      "description": "Absolute workspace path. Defaults to the daemon's resolved workspace, falling back to walking up from the current working directory."
    }
  }
}`)

// contextMDLines bounds how much of .plumb/context.md is inlined into the
// session_start response. 200 lines is generous enough for a project bible
// but keeps the output well under the MCP message size limit.
const contextMDLines = 200

// RootsResolver asks the MCP client for its workspace roots (via roots/list)
// and returns the first one as an absolute path, or "" if unavailable. It is
// used as the third fallback in session_start's workspace resolution chain:
// explicit argument → daemon-resolved workspace → roots/list → cwd walk.
type RootsResolver func(ctx context.Context) string

// SessionStart is a bootstrap tool — call it first in every session to get
// oriented. It returns in one round-trip:
//   - workspace path, detected language, current git branch
//   - first 200 lines of .plumb/context.md (if it exists)
//   - names and descriptions of all memories
//   - top-5 most-used tools from session history
//   - 5 most recently-modified files (workspace-relative)
//   - 3 most recent git commits (subject only)
//   - active LSP diagnostics (errors and warnings only)
//
// Workspace resolution chain (each falls back to the next on empty):
//  1. explicit `workspace` argument
//  2. daemon's already-resolved workspace
//  3. roots/list query to the MCP client (Claude Desktop's roots support)
//  4. walk up from os.Getwd() looking for a project marker
type SessionStart struct {
	ws           WorkspaceFn
	diag         diagnosticsSource // may be nil; diagnostics section skipped when nil
	roots        RootsResolver     // may be nil; roots/list fallback skipped when nil
	refuseFn     func() bool       // may be nil; treated as false (no refusal)
	clientNameFn func() string     // may be nil; returns current MCP client name
}

// NewSessionStart wires the bootstrap tool. refuseHomeRoots is consulted
// before any directory walks under the resolved workspace — it should return
// the current value of walk.refuse_home_roots so live config changes are
// honoured. Pass nil to disable the guard. clientName returns the MCP client
// name negotiated during connection initialisation; pass nil to omit
// client-specific guidance.
func NewSessionStart(ws WorkspaceFn, diag diagnosticsSource, roots RootsResolver, refuseHomeRoots func() bool, clientName func() string) *SessionStart {
	return &SessionStart{ws: ws, diag: diag, roots: roots, refuseFn: refuseHomeRoots, clientNameFn: clientName}
}

func (*SessionStart) Name() string { return "session_start" }

func (*SessionStart) Description() string {
	return "Bootstrap tool — call this first at the start of every session. " +
		"Returns one-shot orientation: workspace path, language, current git branch, " +
		"first 200 lines of .plumb/context.md, all saved memory names/descriptions, " +
		"top-5 most-used tools, 5 most recently-modified files, 3 most recent commits, " +
		"and any active LSP errors/warnings. Falls back to walking up from the current " +
		"working directory if no workspace has been resolved yet (Claude Desktop cold-start). " +
		"Idempotent — safe to call multiple times."
}

func (*SessionStart) InputSchema() json.RawMessage { return sessionStartSchema }

func (t *SessionStart) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Workspace string `json:"workspace"`
	}
	_ = json.Unmarshal(raw, &a)
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" && t.roots != nil {
		// Roots/list fallback: ask the MCP client for its workspace roots.
		ws = t.roots(ctx)
	}
	if ws == "" {
		// Last-resort fallback: walk up from cwd looking for a project marker.
		ws = coldStartWorkspace()
	}
	if ws == "" {
		return "", noWorkspaceError()
	}

	var sb strings.Builder

	// ── 1. Workspace identity ─────────────────────────────────────────────
	fmt.Fprintf(&sb, "# Workspace: %s\n\n", ws)
	if lang := detectLanguage(ws); lang != "" {
		fmt.Fprintf(&sb, "Language: %s\n", lang)
	}
	if branch := gitBranch(ws); branch != "" {
		fmt.Fprintf(&sb, "Branch:   %s\n", branch)
	}
	sb.WriteString("\n")

	// ── 2. context.md (first N lines) ─────────────────────────────────────
	ctxPath := filepath.Join(ws, ".plumb", "context.md")
	if data, err := os.ReadFile(ctxPath); err == nil {
		lines := strings.Split(string(data), "\n")
		truncated := false
		if len(lines) > contextMDLines {
			lines = lines[:contextMDLines]
			truncated = true
		}
		sb.WriteString("## Project context (.plumb/context.md)\n\n")
		sb.WriteString(strings.Join(lines, "\n"))
		if truncated {
			fmt.Fprintf(&sb, "\n… (truncated at %d lines — use read_file to see the rest)\n", contextMDLines)
		}
		sb.WriteString("\n\n")
	}

	// ── 3. Recent commits ─────────────────────────────────────────────────
	if commits := gitRecentCommits(ws, 3); len(commits) > 0 {
		sb.WriteString("## Recent commits\n\n")
		for _, c := range commits {
			fmt.Fprintf(&sb, "- %s\n", c)
		}
		sb.WriteString("\n")
	}

	// ── 4. Recently-modified files ────────────────────────────────────────
	// fsguard skips the walk if ws is a macOS-protected root (e.g. $HOME,
	// $HOME/Documents) — touching one of those would surface a TCC prompt
	// attributed to plumb. The rest of session_start still works without
	// this section.
	refuse := false
	if t.refuseFn != nil {
		refuse = t.refuseFn()
	}
	if skip, reason := fsguard.RefuseWalk(ws, refuse); skip {
		slog.Info("session_start: skipping recent-files walk", "workspace", ws, "reason", reason)
	} else if files := recentlyModifiedFiles(ws, 5); len(files) > 0 {
		sb.WriteString("## Recently modified files\n\n")
		for _, f := range files {
			fmt.Fprintf(&sb, "- %s\n", f)
		}
		sb.WriteString("\n")
	}

	// ── 5. Memories ───────────────────────────────────────────────────────
	if mems, err := memory.List(ws); err == nil {
		if len(mems) == 0 {
			sb.WriteString("## Memories\n\nNone yet. Use write_memory to save project notes.\n\n")
		} else {
			fmt.Fprintf(&sb, "## Memories (%d)\n\n", len(mems))
			for _, m := range mems {
				fmt.Fprintf(&sb, "- **%s**", m.Name)
				if m.Description != "" {
					fmt.Fprintf(&sb, " — %s", m.Description)
				}
				fmt.Fprintf(&sb, " (%d bytes)\n", m.SizeBytes)
			}
			sb.WriteString("\nUse read_memory to load any of these.\n\n")
		}
	}

	// ── 6. Recent tool usage stats ────────────────────────────────────────
	// Use global stats DB filtered by workspace.
	if db, err := stats.OpenReadOnly(); err == nil && db != nil {
		defer db.Close()
		if toolStats, err := db.Summary(stats.Filter{Workspace: ws}); err == nil && len(toolStats) > 0 {
			sb.WriteString("## Most-used tools (this workspace)\n\n")
			limit := min(len(toolStats), 5)
			for _, s := range toolStats[:limit] {
				fmt.Fprintf(&sb, "- %s: %d calls, avg %dms\n", s.Tool, s.Calls, int64(s.AvgMs))
			}
			sb.WriteString("\n")
		}
	}

	// ── 7. Tool guidance (Claude Code only) ──────────────────────────────
	// Match "claude-code" exactly or "claude-code/…" (version-qualified).
	// A bare HasPrefix would incorrectly match "claude-codegen" etc.
	if isClaudeCode(t.clientNameFn) {
		sb.WriteString("## Tool guidance (Claude Code)\n\n")
		sb.WriteString("Plumb adds LSP-semantic tools Claude Code lacks natively:\n\n")
		sb.WriteString("- **workspace_symbols** — find a symbol by name instantly (LSP index). Use instead of grep/search_in_files for name lookups.\n")
		sb.WriteString("- **find_references** — all call sites for a symbol (LSP-semantic, not text search). Accepts name or position.\n")
		sb.WriteString("- **get_definition** — jump to definition by name or position without reading files first.\n")
		sb.WriteString("- **call_hierarchy** — callers and callees of a function.\n")
		sb.WriteString("- **type_hierarchy** — supertypes and subtypes of a class or interface.\n")
		sb.WriteString("- **rename_symbol** — workspace-wide LSP rename (updates all references; safer than find+replace).\n")
		sb.WriteString("- **list_symbols** with include_signatures=true — outline a file without reading it.\n")
		sb.WriteString("- **diagnostics** — live LSP errors and warnings without running a build.\n\n")
	}

	// ── 8. Active diagnostics (errors + warnings only) ────────────────────
	if t.diag != nil {
		all := t.diag.AllDiagnostics()
		filtered := make(map[string][]protocol.Diagnostic)
		for uri, diags := range all {
			for _, d := range diags {
				if d.Severity <= protocol.SevWarning {
					filtered[uri] = append(filtered[uri], d)
				}
			}
		}
		if len(filtered) > 0 {
			sb.WriteString("## Active diagnostics (errors and warnings)\n\n")
			sb.WriteString(formatDiagnostics(filtered))
			sb.WriteString("\n")
		}
	}

	return sb.String(), nil
}

// detectLanguage returns a human-readable language label by probing for
// well-known project-root markers.
func detectLanguage(ws string) string {
	markers := []struct {
		file string
		lang string
	}{
		{"go.mod", "Go"},
		{"package.json", "JavaScript/TypeScript"},
		{"Cargo.toml", "Rust"},
		{"pyproject.toml", "Python"},
		{"setup.py", "Python"},
		{"pom.xml", "Java (Maven)"},
		{"build.gradle", "Java/Kotlin (Gradle)"},
		{"CMakeLists.txt", "C/C++ (CMake)"},
		{"mix.exs", "Elixir"},
		{"Gemfile", "Ruby"},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(ws, m.file)); err == nil {
			return m.lang
		}
	}
	return ""
}

// coldStartWorkspace walks up from os.Getwd() looking for a project marker.
// Returns "" if nothing is found within reasonable depth.
func coldStartWorkspace() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	markers := []string{"go.mod", "package.json", "Cargo.toml", "pyproject.toml", "setup.py", "pom.xml", ".git", ".plumb"}
	dir := cwd
	for range 12 { // cap walk depth
		for _, m := range markers {
			if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
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
