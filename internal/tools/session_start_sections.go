package tools

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/plumbkit/plumb/internal/clientcaps"
	"github.com/plumbkit/plumb/internal/fsguard"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
	"github.com/plumbkit/plumb/internal/memory"
	"github.com/plumbkit/plumb/internal/stats"
)

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
	case t.writeLSPWarming(sb):
		// warming advisory already written
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

// writeLSPWarming writes a warm-up advisory when the primary language server is
// attached but its handshake has not finished, and reports whether it did. A
// cold server (rust-analyzer running cargo metadata, a large gopls module) can
// take minutes; meanwhile the tree-sitter index already answers, so the agent is
// steered there rather than into a semantic tool that would block on the warm-up.
func (t *SessionStart) writeLSPWarming(sb *strings.Builder) bool {
	warming, elapsed := t.lspWarming()
	if !warming {
		return false
	}
	if elapsed > 0 {
		fmt.Fprintf(sb, "Language server is still warming up (~%s elapsed). ", elapsed.Round(time.Second))
	} else {
		sb.WriteString("Language server is still warming up. ")
	}
	sb.WriteString("`topology_search`, `find_symbol`, and `file_outline` answer now; " +
		"`get_definition`, `find_references`, and the hierarchies will work once it's ready (retry shortly).\n\n")
	return true
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

// writeSessionWorkingTree shows a compact diffstat of uncommitted changes to
// tracked files, so an agent sees what was already modified in the worktree
// (often a peer agent's in-flight work) before it starts editing.
func writeSessionWorkingTree(sb *strings.Builder, ws string) {
	const maxStatLines = 12
	stat := gitWorkingTreeSummary(ws, maxStatLines)
	if stat == "" {
		return
	}
	sb.WriteString("## Uncommitted changes (git diff --stat HEAD)\n\n")
	sb.WriteString("```\n")
	sb.WriteString(stat)
	sb.WriteString("\n```\n\n")
}

// writeSessionSubmodules surfaces any git submodules in the workspace. A
// submodule is a separate repository nested in the superproject, so an agent
// that edits a file inside one must run the git tool against the submodule
// (repo=<path>) — a commit run against the superproject records only the moved
// pointer, not the file change. That is the single most common submodule
// footgun, so it is stated at orientation. Skipped when the repo has none.
func writeSessionSubmodules(sb *strings.Builder, ws string) {
	subs := gitSubmodules(ws)
	if len(subs) == 0 {
		return
	}
	sb.WriteString("## Submodules (nested git repositories)\n\n")
	fmt.Fprintf(sb, "Each path below is a separate git repository. To stage or commit a file inside one, "+
		"call the `git` tool with `repo` pointing inside that submodule (e.g. `repo: %q`) and give `files` relative to it. "+
		"A commit run against the superproject records only the submodule's pointer, not the file change.\n\n",
		filepath.Join(ws, subs[0]))
	for _, s := range subs {
		fmt.Fprintf(sb, "- %s/\n", s)
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

// writeSessionRecentFiles lists the 5 most recently modified files and
// returns them so the memories section can rank recently-relevant memories
// first without a second tree walk.
// Skips the walk if fsguard identifies ws as a protected macOS root (e.g.
// $HOME) — touching those would surface a TCC prompt attributed to plumb.
func (t *SessionStart) writeSessionRecentFiles(sb *strings.Builder, ws string) []string {
	refuse := t.refuseFn != nil && t.refuseFn()
	if skip, reason := fsguard.RefuseWalk(ws, refuse); skip {
		slog.Info("session_start: skipping recent-files walk", "workspace", ws, "reason", reason)
		return nil
	}
	files := recentlyModifiedFiles(ws, 5)
	if len(files) == 0 {
		return nil
	}
	sb.WriteString("## Recently modified files\n\n")
	for _, f := range files {
		fmt.Fprintf(sb, "- %s\n", f)
	}
	sb.WriteString("\n")
	return files
}

// writeSessionMemories lists every memory, ordering those attached to a
// recently modified file (paths glob or provenance) first — the memories most
// likely to matter for the work in flight lead the list.
func writeSessionMemories(sb *strings.Builder, ws string, recent []string) {
	mems, err := memory.List(ws)
	if err != nil {
		return
	}
	if len(mems) == 0 {
		sb.WriteString("## Memories\n\nNone yet. Use write_memory to save project notes.\n\n")
		return
	}
	mems = recentFirstMemories(mems, recent)
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

// recentFirstMemories stably partitions mems: memories related to a recently
// modified file first, the rest in their original (name) order after.
func recentFirstMemories(mems []memory.Memory, recent []string) []memory.Memory {
	if len(recent) == 0 {
		return mems
	}
	refs := make([]memory.CodeRef, 0, len(recent))
	for _, f := range recent {
		refs = append(refs, memory.CodeRef{File: f})
	}
	var hot, rest []memory.Memory
	for _, m := range mems {
		if len(memory.MemoriesForRefs([]memory.Memory{m}, refs, 1)) > 0 {
			hot = append(hot, m)
		} else {
			rest = append(rest, m)
		}
	}
	return append(hot, rest...)
}

func writeSessionStats(sb *strings.Builder, ws string) {
	db, err := stats.SharedReadOnly()
	if err != nil || db == nil {
		return
	}
	toolStats, err := db.Summary(stats.Filter{Workspace: ws})
	if err != nil || len(toolStats) == 0 {
		return
	}
	sb.WriteString("## Most-used tools (this workspace)\n\n")
	limit := min(len(toolStats), 5)
	for _, s := range toolStats[:limit] {
		fmt.Fprintf(sb, "- %s: %d calls, avg %dms, p95 %dms\n", s.Tool, s.Calls, int64(s.AvgMs), s.P95Ms)
	}
	// Two honest axes instead of one "tokens saved" label: capability (work the
	// client could not do natively) and efficiency (fewer tokens for the same
	// result). Legacy rows carry neither and are simply absent here.
	axes := db.SavingsAxes(stats.Filter{Workspace: ws})
	if axes.Total() > 0 {
		fmt.Fprintf(sb, "\n~%s capability + ~%s efficiency tokens (estimated, model v%d)\n",
			stats.FormatSavings(int(axes.Capability)), stats.FormatSavings(int(axes.Efficiency)), clientcaps.ModelVersion)
	}
	sb.WriteString("\n")
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
