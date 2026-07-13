package tools

import "strings"

func (t *SessionStart) writeSessionGuidance(sb *strings.Builder) {
	profile, hidden, reason := t.resolvedToolProfile()
	sb.WriteString(ProfileNote(profile, hidden, reason))
	switch {
	case isClaudeCode(t.clientNameFn):
		t.writeClaudeCodeGuidance(sb)
	case isClaudeDesktop(t.clientNameFn):
		t.writeClaudeDesktopGuidance(sb)
	}
}

// leanProfile reports whether the connection resolved to the lean tool profile,
// under which guidance must not steer the agent to a tool hidden from tools/list.
func (t *SessionStart) leanProfile() bool {
	profile, _, _ := t.resolvedToolProfile()
	return profile == "lean"
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
		if t.leanProfile() {
			sb.WriteString("- **topology_explore** — the neighbourhood around a symbol.\n")
		} else {
			sb.WriteString("- **topology_explore** / **topology_impact** — neighbourhood and blast radius around a symbol.\n")
		}
		sb.WriteString("- **file_outline** — a file's shape (signatures, bodies collapsed) in ~200 tokens.\n")
		if !t.leanProfile() {
			sb.WriteString("- **topology_routes** — framework entry points (HTTP handlers, Cobra, Flask).\n")
		}
		sb.WriteString("\n")
		sb.WriteString("LSP-semantic — precise navigation (Claude Code lacks these natively):\n\n")
		sb.WriteString("- **get_definition** / **find_references** — exact definition and all call sites (scope-aware, not text search).\n")
		sb.WriteString("- **rename_symbol** — workspace-wide semantic rename.\n")
		if !t.leanProfile() {
			sb.WriteString("- **call_hierarchy** / **type_hierarchy** — callers/callees, super/subtypes.\n")
		}
		sb.WriteString("- **diagnostics** — live errors and warnings without running a build.\n\n")
		return
	}
	sb.WriteString("Plumb adds LSP-semantic tools Claude Code lacks natively:\n\n")
	sb.WriteString("- **workspace_symbols** — find a symbol by name instantly (LSP index). Use instead of grep/search_in_files for name lookups.\n")
	sb.WriteString("- **find_references** — all call sites for a symbol (LSP-semantic, not text search). Accepts name or position.\n")
	sb.WriteString("- **get_definition** — jump to definition by name or position without reading files first.\n")
	if !t.leanProfile() {
		sb.WriteString("- **call_hierarchy** — callers and callees of a function.\n")
		sb.WriteString("- **type_hierarchy** — supertypes and subtypes of a class or interface.\n")
	}
	sb.WriteString("- **rename_symbol** — workspace-wide LSP rename (updates all references; safer than find+replace).\n")
	if !t.leanProfile() {
		sb.WriteString("- **list_symbols** with include_signatures=true — outline a file without reading it.\n")
	}
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
