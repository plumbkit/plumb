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
	ws, err := t.resolveSessionWorkspace(ctx, raw)
	if err != nil {
		return "", err
	}
	lang := detectLanguage(ws)
	hasErrors := t.hasActiveDiagnosticErrors()
	var sb strings.Builder
	t.writeSessionIdentity(&sb, ws, lang)
	t.writeSessionRecommendedStart(&sb, hasErrors, lang)
	writeSessionContext(&sb, ws)
	writeSessionCommits(&sb, ws)
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

func (t *SessionStart) resolveSessionWorkspace(ctx context.Context, raw json.RawMessage) (string, error) {
	var a struct {
		Workspace string `json:"workspace"`
	}
	_ = json.Unmarshal(raw, &a)
	ws := resolveWorkspace(a.Workspace, t.ws)
	if ws == "" && t.roots != nil {
		ws = t.roots(ctx)
	}
	if ws == "" {
		ws = coldStartWorkspace()
	}
	if ws == "" {
		return "", noWorkspaceError()
	}
	return ws, nil
}

func (t *SessionStart) writeSessionIdentity(sb *strings.Builder, ws, lang string) {
	fmt.Fprintf(sb, "# Workspace: %s\n\n", ws)
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
	sb.WriteString("\n")
}

func (t *SessionStart) writeSessionRecommendedStart(sb *strings.Builder, hasErrors bool, lang string) {
	sb.WriteString("## Recommended first step\n\n")
	switch {
	case hasErrors:
		sb.WriteString("Active errors detected — start with `diagnostics` to review them.\n\n")
	case t.diag != nil && lang != "":
		sb.WriteString("LSP is available — use `workspace_symbols` to survey the codebase.\n\n")
	case lang != "":
		sb.WriteString("No language server attached — use `list_files` to explore, then `search_in_files` to find relevant code.\n\n")
	default:
		sb.WriteString("Use `list_files` to explore the codebase.\n\n")
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
		fmt.Fprintf(sb, "- %s: %d calls, avg %dms\n", s.Tool, s.Calls, int64(s.AvgMs))
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
	case isClaudeDesktop(t.clientNameFn):
		sb.WriteString("## Tool guidance (Claude Desktop)\n\n")
		sb.WriteString("Claude Desktop has no native filesystem or shell tools. Plumb is your only interface to the codebase.\n\n")
		sb.WriteString("**All file operations go through plumb** — there is no fallback:\n\n")
		sb.WriteString("- **read_file** / **read_multiple_files** — read any file or slice of a file.\n")
		sb.WriteString("- **write_file** / **edit_file** — create or modify files atomically.\n")
		sb.WriteString("- **list_files** / **find_files** / **search_in_files** — discover and search the codebase.\n")
		sb.WriteString("- **git** — read-only git queries (status, log, diff, blame).\n\n")
		sb.WriteString("**LSP-semantic tools** (no equivalent without a language server):\n\n")
		sb.WriteString("- **workspace_symbols** — find any symbol by name across the workspace instantly.\n")
		sb.WriteString("- **find_references** — all call sites for a symbol (scope-aware, not text search).\n")
		sb.WriteString("- **get_definition** — jump to definition without reading the file first.\n")
		sb.WriteString("- **rename_symbol** — workspace-wide semantic rename across all files.\n")
		sb.WriteString("- **diagnostics** — live compile errors and warnings from the language server.\n\n")
		sb.WriteString("If a plumb tool fails, retry or check `daemon_info`. Do not attempt native shell commands — they are unavailable.\n\n")
	}
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
		sb.WriteString(formatDiagnostics(real))
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
	case "JavaScript/TypeScript":
		return []string{".ts", ".js", ".tsx", ".jsx"}, "JS/TS"
	case "Rust":
		return []string{".rs"}, "Rust"
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

// isClaudeDesktop reports whether fn identifies the MCP client as Claude Desktop.
func isClaudeDesktop(fn func() string) bool {
	if fn == nil {
		return false
	}
	n := strings.ToLower(fn())
	return n == "claude-desktop" || strings.HasPrefix(n, "claude-desktop/")
}
