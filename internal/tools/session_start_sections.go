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
	"github.com/plumbkit/plumb/internal/langsupport"
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
	// Rendered independently of lang: a home root can also carry a stray
	// root marker (/~/go.mod), and the identity line must not let that marker
	// read as "a server is attached" while the skip goes unexplained (#316).
	if t.lspSkipNoteFn != nil {
		if note := t.lspSkipNoteFn(); note != "" {
			fmt.Fprintf(sb, "%s\n", note)
		}
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
	if note := uncoveredPrimaryLanguageNote(lang); note != "" {
		sb.WriteString(note)
	}
	sb.WriteString("\n")
}

// uncoveredPrimaryLanguageNote warns when the workspace's detected primary
// language is one the topology Map cannot index, and returns "" otherwise.
//
// The identity block above is the most misleading place in plumb for an
// uncovered language: it prints "Language: Ruby" and "Scale: ~900 files (683
// Ruby)" from a plain filesystem walk, which reads as confirmation that plumb
// understands the project. Every Map-backed tool then returns nothing, and the
// agent has no reason to attribute that to coverage rather than to the code. The
// warning belongs here, before the first query, because this is the one section
// an agent always reads.
//
// The detected label is a display name ("Ruby", "C/C++ (CMake)"), not a registry
// key, so it is resolved through langFileProfile's extensions — the same mapping
// that produced the Scale line — rather than by lower-casing the label.
func uncoveredPrimaryLanguageNote(lang string) string {
	exts, _ := langFileProfile(lang)
	for _, ext := range exts {
		l, ok := langsupport.ByPath("file" + ext)
		if !ok || l.Structural != langsupport.EngineNone {
			continue
		}
		return fmt.Sprintf("\nNOTE: the topology Map does not cover %s yet — topology_search, "+
			"workspace_search and file_outline will return little or nothing for this "+
			"workspace's %s sources. That is a coverage gap, not an empty codebase. "+
			"Use search_in_files (exact) and read_file instead, and see topology_status "+
			"for the full census.\n", l.Name, l.Name)
	}
	return ""
}

// writeSessionRecommendedStart picks the one next step that fits the session's
// actual state.
//
// ON NAMING NON-LEAN TOOLS HERE. The no-LSP fallbacks below steer to
// `find_files` and `search_in_files`, neither of which is in LeanTools — which
// looks like it contradicts the "never name a hidden tool" rule the Kimi
// guidance block follows (TestKimiCodeGuidance_LeanSetOnly). Three cases, and
// only one of them can strand an agent:
//
//   - A CLIENT-SIDE allowlist removes the tool from the client before plumb is
//     ever called, and plumb cannot see that filter — so a named non-lean tool
//     is genuinely uncallable and the rule is load-bearing. That is Kimi Code,
//     and now Codex and Gemini CLI too. For them lastResortSearch names no
//     plumb tool at all: it points at the client's own search, which every
//     client carrying an allowlist has (clientcaps NativeSearch).
//   - The lean PROFILE only hides a tool from tools/list; it stays callable by
//     name (see "Hidden ≠ unregistered"), and auto mode serves lean solely to a
//     client whose clientcaps entry declares ReliableDeferredToolDiscovery —
//     i.e. one demonstrated to invoke a tool it was never advertised.
//   - Everyone else (Claude Code, Claude Desktop, anything unrecognised) is
//     served full, where both tools are advertised anyway.
//
// So every client that can reach this text can act on it. Restricting the last
// two cases to the lean set would cost the case they exist for — a workspace
// with no language server and no topology index, where `find_files` and
// `search_in_files` are the only discovery left, and where Claude Desktop in
// particular has no native search to fall back on.
func (t *SessionStart) writeSessionRecommendedStart(sb *strings.Builder, hasErrors bool, lang, lspKey string) {
	sb.WriteString("## Recommended first step\n\n")
	switch {
	case hasErrors:
		sb.WriteString("Active errors detected — start with `diagnostics` to review them.\n\n")
	case t.writeLSPWarming(sb):
		// warming advisory already written
	case t.lspAttached():
		// Warming was checked first, so attached here means the handshake is
		// complete — "ready" is a guarantee, not a hope. A non-default diagnostics
		// mode (anything but push) is noted so the agent knows what was negotiated.
		if mode := t.lspDiagMode(); mode != "" && mode != "push" {
			fmt.Fprintf(sb, "LSP is ready (diagnostics: %s) — use `workspace_symbols` to survey the codebase.\n\n", mode)
		} else {
			sb.WriteString("LSP is ready — use `workspace_symbols` to survey the codebase.\n\n")
		}
	case t.writeLSPRouted(sb):
		// routed advisory already written
	case t.topologyActive():
		sb.WriteString("No language server is attached, but the topology index is active — use " +
			"`topology_search` and `file_outline` for discovery and structure. " +
			"(`get_definition`/`find_references` still need a language server.)\n\n")
	case lang != "":
		t.writeNoLSPGuidance(sb, lang, lspKey)
	default:
		fmt.Fprintf(sb, "No language server or index here — %s to explore the codebase.\n\n",
			t.lastResortSearch())
	}
}

// lastResortSearch names the discovery of last resort for a workspace with
// neither a language server nor a topology index: plumb's own search tools, or —
// for a client whose config may have filtered them out — its native ones.
//
// It deliberately names NO plumb tool in the allowlist case rather than naming
// them with a caveat. A caveat still puts the name in front of a model that may
// act on it, and these clients all have native search anyway; the honest, useful
// instruction is the one that works in both states.
func (t *SessionStart) lastResortSearch() string {
	if clientSideAllowlistCapable(t.clientNameFn) {
		return "use your client's own file search"
	}
	return "use `search_in_files` and `find_files`"
}

// writeNoLSPGuidance covers a recognised project with neither a language server
// nor a topology index — the case that misled a Java agent into thinking the
// semantic tools were broken. It names the concrete next step rather than
// silently advertising LSP tools that will error.
func (t *SessionStart) writeNoLSPGuidance(sb *strings.Builder, lang, lspKey string) {
	fmt.Fprintf(sb, "No language server is attached for %s. ", lang)
	switch lspKey {
	case "":
		fmt.Fprintf(sb, "plumb has no language server for it yet — %s, "+
			"or enable the topology index (`[topology] enabled = true`) for indexed symbol search.\n\n",
			t.lastResortSearch())
	case "go", "python":
		fmt.Fprintf(sb, "Its server ships on by default, so it likely isn't installed or failed to start — "+
			"check the server binary is on PATH. Meanwhile %s, or enable "+
			"`[topology] enabled = true` for indexed search.\n\n", t.lastResortSearch())
	default:
		fmt.Fprintf(sb, "Its adapter is opt-in — set `[lsp.%s] enabled = true` and ensure the server is on PATH. "+
			"For language-server-free symbol search, enable the topology index (`[topology] enabled = true`).\n\n", lspKey)
	}
}

// writeLSPRouted covers a session with no primary language server whose files
// are nonetheless served by per-file routing — a workspace root with no
// detectable language (a bare .plumb/ root) never acquires a primary, so the
// checks above all miss and the agent used to be told, falsely, that no language
// server was attached. Reports whether it wrote anything.
func (t *SessionStart) writeLSPRouted(sb *strings.Builder) bool {
	routed := t.lspRouted()
	if len(routed) == 0 {
		return false
	}
	fmt.Fprintf(sb, "No primary language server is attached, but %s files here are served by a live language server — "+
		"`get_definition`, `find_references`, and `diagnostics` work for them. ", strings.Join(routed, "/"))
	if t.topologyActive() {
		sb.WriteString("For anything else, the topology index is active — use `topology_search` and `file_outline`.\n\n")
		return true
	}
	fmt.Fprintf(sb, "For anything else, %s.\n\n", t.lastResortSearch())
	return true
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
	sb.WriteString("`topology_search`, `workspace_symbols`, and `file_outline` answer now; " +
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
	sb.WriteString(t.projectGitNotice(ws))
	sb.WriteString("\n")
}

// projectGitNotice reports a project [git] block that was overruled, so the
// resolved policy printed above it cannot be mistaken for a bug when it
// contradicts the .plumb/config.toml in the workspace. It is emitted after the
// policy, and compared against it: a key the project asked for and already has
// is not named. Empty when unwired, or when there is nothing to say — see
// formatProjectGitNotice.
func (t *SessionStart) projectGitNotice(ws string) string {
	if t.projectGitFn == nil {
		return ""
	}
	return formatProjectGitNotice(ws, t.projectGitFn(), t.gitPolicyFn())
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

// maxListedUserMemories caps the user-authored memory listing so a
// memory-rich workspace still orients in a screenful; list_memories browses
// the rest.
const maxListedUserMemories = 10

// writeSessionMemories renders the memory orientation block in three tiers:
// user-authored memories listed (recently-relevant first, capped at
// maxListedUserMemories), and generated memories (episodic session summaries,
// shared findings) collapsed to a single count line. On a memory-heavy
// workspace enumerating dozens of generated summaries cost ~1,500 tokens of
// listing the agent had to scan past — their content is already surfaced by
// the "Last session" block, and list_memories / search_memories remain the
// full-fidelity paths.
func writeSessionMemories(sb *strings.Builder, ws string, recent []string) {
	mems, err := memory.List(ws)
	if err != nil {
		return
	}
	if len(mems) == 0 {
		sb.WriteString("## Memories\n\nNone yet. Use write_memory to save project notes.\n\n")
		return
	}
	var user, generated []memory.Memory
	for _, m := range mems {
		if m.UserAuthored() {
			user = append(user, m)
		} else {
			generated = append(generated, m)
		}
	}
	if len(generated) == 0 {
		fmt.Fprintf(sb, "## Memories (%d)\n\n", len(mems))
	} else {
		fmt.Fprintf(sb, "## Memories (%d: %d user, %d generated)\n\n", len(mems), len(user), len(generated))
	}
	writeUserMemoryList(sb, recentFirstMemories(user, recent))
	if len(generated) > 0 {
		if len(user) > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(sb, "%d generated %s (episodic session summaries and shared findings — not listed; "+
			"use search_memories to find relevant context, list_memories to browse).\n",
			len(generated), pluralMemories(len(generated)))
	}
	sb.WriteString("\nUse read_memory to load any of these.\n\n")
}

// writeUserMemoryList prints user-authored memories in the established
// one-line format, capped at maxListedUserMemories with a browse pointer for
// the remainder.
func writeUserMemoryList(sb *strings.Builder, user []memory.Memory) {
	listed := user
	if len(listed) > maxListedUserMemories {
		listed = listed[:maxListedUserMemories]
	}
	for _, m := range listed {
		fmt.Fprintf(sb, "- **%s**", m.Name)
		if m.Description != "" {
			fmt.Fprintf(sb, " — %s", m.Description)
		}
		fmt.Fprintf(sb, " (%d bytes)\n", m.SizeBytes)
	}
	if n := len(user) - len(listed); n > 0 {
		fmt.Fprintf(sb, "…and %d more — use list_memories to browse all.\n", n)
	}
}

// pluralMemories renders the count noun for the generated-memory summary line.
func pluralMemories(n int) string {
	if n == 1 {
		return "memory"
	}
	return "memories"
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
	realDiags, coldCount := partitionColdCacheGoMod(filtered)

	sb.WriteString("## Active diagnostics (errors and warnings)\n\n")
	if len(realDiags) > 0 {
		// Flag entries whose file mtime is newer than the last publishDiagnostics:
		// the orientation packet is the most likely place to surface diagnostics
		// gopls produced before reconciling in-flight edits. Mirrors the diagnostics
		// tool's opt-in path. (Catches "edited after analysis"; a fresh-timestamp
		// analysis against a cold module cache is handled by the go.mod partition
		// above.)
		if ts, ok := t.diag.(timedDiagnosticsSource); ok {
			sb.WriteString(formatDiagnosticsWithTimes(realDiags, ts.AllDiagnosticTimes()))
		} else {
			sb.WriteString(formatDiagnostics(realDiags))
		}
	}
	if coldCount > 0 {
		sep := ""
		if len(realDiags) > 0 {
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
func partitionColdCacheGoMod(byURI map[string][]protocol.Diagnostic) (realDiags map[string][]protocol.Diagnostic, coldCount int) {
	realDiags = make(map[string][]protocol.Diagnostic)
	for uri, diags := range byURI {
		if !strings.HasSuffix(uri, "/go.mod") {
			realDiags[uri] = diags
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
			realDiags[uri] = kept
		}
	}
	return realDiags, coldCount
}
