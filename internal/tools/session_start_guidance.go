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
	case isKimiCode(t.clientNameFn):
		t.writeKimiCodeGuidance(sb)
	default:
		t.writeGenericGuidance(sb)
	}
}

// writeGenericGuidance is the block for every client with no bespoke one — the
// eleven `plumb setup` targets that fell through to nothing before it existed
// (Codex, Gemini CLI, Cursor, Augment, Qwen, Antigravity, Antigravity Desktop,
// OpenCode, Crush, Goose, Hermes), plus any client plumb does not recognise at
// all.
//
// WHY IT EXISTS. Those clients used to be steered by the tool descriptions
// themselves: ~21 of them carried comparative routing ("prefer workspace_symbols
// for symbol lookups", "prefer this over search_in_files or grep", the discovery
// ladder). The DRY pass moved that routing into the skills, which is the right
// home for a client that installs them and no home at all for a client that does
// not — so for these clients the routing was removed with nothing put in its
// place. This is the replacement the plan always specified and had not built:
// the condensed render of the same authored source, "thinned, not deleted".
//
// WHY IT IS BLANDER than the client-specific blocks. It cannot assume much
// about the client — native file tools, a read-tracking harness — so it states
// plumb's own routing rather than arguing against an alternative it cannot see,
// and it never quotes another product's error strings. A client that earns
// specific advice should get its own branch above.
//
// Lean-aware throughout, on TWO independent triggers (nameLeanToolsOnly):
// under the lean profile the tools it names are hidden from tools/list, and for
// a client that can hold a plumb-written allowlist in its own config — Codex and
// Gemini CLI both route through here — a non-lean tool may have been removed
// client-side entirely. Either way, naming one would be a broken pointer.
func (t *SessionStart) writeGenericGuidance(sb *strings.Builder) {
	sb.WriteString("## Tool guidance\n\n")

	if t.topologyActive() {
		if t.nameLeanToolsOnly() {
			sb.WriteString("- Discovery: start with **topology_search** (ranked, works while the language " +
				"server warms), then **get_definition** / **find_references** for exact, type-aware answers.\n")
		} else {
			sb.WriteString("- Discovery ladder: **workspace_search** (ranked, across code, docs, and memories) " +
				"→ the topology tools and **get_definition** / **find_references** for exact answers → " +
				"**search_in_files** when you need every occurrence. The plumb-explore skill has the full " +
				"version, with the signal for leaving each rung.\n")
		}
		sb.WriteString("- **topology_affected** — which tests an edit touches (dependency edges + co-location, " +
			"recall-biased and confidence-labelled). No language server gives this.\n")
	} else if t.nameLeanToolsOnly() {
		sb.WriteString("- Discovery: **workspace_symbols** to find a symbol by name, then **get_definition** / " +
			"**find_references** for exact, type-aware answers.\n")
	} else {
		sb.WriteString("- Discovery: **workspace_search** (ranked, across code, docs, and memories), then " +
			"**get_definition** / **find_references** for exact, type-aware answers; **search_in_files** " +
			"when you need every occurrence.\n")
	}

	sb.WriteString("- Edit lane: **read_file** returns an mtime/sha header that **edit_file** takes back as " +
		"`expected_mtime`, so the write is concurrency-checked against exactly what you read, per-path " +
		"locked, announced to the language server, and reversible with **undo_edit**. **transaction_apply** " +
		"is one atomic multi-file change.\n")
	sb.WriteString("- **rename_symbol** — workspace-wide semantic rename; scope-aware, so it does what a " +
		"text find-and-replace cannot.\n")
	sb.WriteString("- **diagnostics** — live errors and warnings without running a build.\n")
	sb.WriteString("- **run_task** — the project's stored `[tasks.<lang>]` build/test/lint command, no shell " +
		"and bounded output; the Tasks section above says which slots this workspace actually has.\n\n")

	if !t.topologyActive() {
		sb.WriteString("Tip: enable the topology index (`[topology] enabled = true` in `.plumb/config.toml`) " +
			"for ranked search, file outlines, and `topology_affected` — which tests to run after a change.\n\n")
	}
}

// leanProfile reports whether the connection resolved to the lean tool profile,
// under which guidance must not steer the agent to a tool hidden from tools/list.
func (t *SessionStart) leanProfile() bool {
	profile, _, _ := t.resolvedToolProfile()
	return profile == "lean"
}

// nameLeanToolsOnly reports whether guidance must confine itself to the lean set.
// Two independent reasons, and the second is invisible to the first: the SERVER
// may have hidden the non-lean tools (leanProfile), or the CLIENT may have
// filtered them out in its own config, which plumb cannot see at all — for a
// --lean Codex or Gemini CLI the resolved profile is "full" and the tools are
// still gone. Guidance that keyed only off the profile therefore steered those
// users at tools their own config had removed.
func (t *SessionStart) nameLeanToolsOnly() bool {
	return t.leanProfile() || clientSideAllowlistCapable(t.clientNameFn)
}

// writeClaudeCodeGuidance leads with topology (the Map) for discovery / structure
// / impact when the index is active, names the handful of moves that matter
// after orientation, and closes with the cold-LSP ladder (what still works via
// tree-sitter while the server warms, what needs a ready one). Each multi-tool
// workflow points at the skill that owns it — plumb-explore for discovery,
// plumb-refactor for edits, plumb-testing for verification — rather than being
// restated here; the orientation packet is paid for on every session, the skills
// are not. When topology is off it falls back to the LSP-led form with a
// one-line pointer to enabling the index.
func (t *SessionStart) writeClaudeCodeGuidance(sb *strings.Builder) {
	sb.WriteString("## Tool guidance (Claude Code)\n\n")
	sb.WriteString(nativeEditLaneWarning)
	if t.topologyActive() {
		sb.WriteString("Two complementary layers. **Topology (the Map)** is the primary path for " +
			"discovery, structure, and impact — it answers instantly, tolerates broken code, and " +
			"covers every indexed language. **LSP (the GPS)** is for precise, type-aware navigation " +
			"once you know where to work.\n\n")
		sb.WriteString("- **topology_affected** — which tests to run after an edit (dependency edges + " +
			"co-location, recall-biased, confidence-labelled); the plumb-testing skill has the post-edit flow " +
			"(skills install via `plumb skills sync`).\n")
		// workspace_search is not in the lean set, so a lean client is pointed at
		// topology_search instead — guidance must never name a hidden tool.
		if t.leanProfile() {
			sb.WriteString("- Discovery: start with **topology_search**, then **get_definition** / " +
				"**find_references** for exact, type-aware answers — the plumb-explore skill has the full ladder.\n")
		} else {
			sb.WriteString("- Discovery: start with **workspace_search**, then the Map and **get_definition** / " +
				"**find_references** for exact, type-aware answers — the plumb-explore skill has the full ladder.\n")
		}
		sb.WriteString("- Refactors: **rename_symbol** (load via ToolSearch if not already in context) for " +
			"identifiers, **transaction_apply** for one atomic multi-file change — the plumb-refactor skill " +
			"has the rest.\n")
		sb.WriteString("- **diagnostics** — live errors and warnings without running a build; await_diagnostics " +
			"on edit_file/write_file returns the authoritative post-write pass.\n\n")
		// Lean hides these tools from tools/list, so the orientation packet does
		// not advertise them. Error messages DO name them (ColdLSPToolsHint) even
		// under lean: that is reactive — the agent has already hit a cold server
		// and needs the ladder — and a lean client can still reach a hidden tool
		// through deferred schema discovery.
		if !t.leanProfile() {
			sb.WriteString("Cold LSP: the symbol-edit tools (insert_before_symbol, insert_after_symbol, " +
				"replace_symbol_body, move_symbol) still work via the tree-sitter index while the language " +
				"server warms; find_references / explain_symbol / call_hierarchy / type_hierarchy / " +
				"safe_delete_symbol / rename_symbol need a ready server — retry shortly (see daemon_info). None " +
				"but find_references are pinned, so load one via ToolSearch first if it is not already in your " +
				"context. While it warms, diagnostics is labelled INCOMPLETE and an empty result from the query " +
				"tools is not evidence of absence.\n\n")
		}
		return
	}
	sb.WriteString("Plumb adds LSP-semantic tools Claude Code lacks natively:\n\n")
	sb.WriteString("- **workspace_symbols** / **get_definition** / **find_references** — find a symbol by name, " +
		"jump to its definition, list every call site (scope-aware, not text search).\n")
	sb.WriteString("- **rename_symbol** — workspace-wide LSP rename (load via ToolSearch if not already in " +
		"context); the plumb-refactor skill has the rest of the edit lane (skills install via " +
		"`plumb skills sync`).\n")
	sb.WriteString("- **file_outline** — a file's shape (signatures, bodies collapsed) without reading it.\n")
	sb.WriteString("- **diagnostics** — live LSP errors and warnings without running a build.\n\n")
	sb.WriteString("Tip: enable the topology index (`[topology] enabled = true` in `.plumb/config.toml`) to add " +
		"ranked search, file outlines, and `topology_affected` — which tests to run after a change " +
		"(the plumb-testing skill covers that flow).\n\n")
}

// writeKimiCodeGuidance is the Kimi Code block. Two constraints shape it.
//
// LEAN-SET TOOLS ONLY, unconditionally. Kimi Code filters tools CLIENT-side via
// the enabledTools allowlist `plumb setup kimi-code --lean` writes into its
// mcp.json. plumb cannot observe that filter — tools/call arrives identically
// whether or not it is in force — so t.leanProfile() says nothing here. Naming
// a non-lean tool would therefore be a broken pointer for every user who took
// the --lean advice this block itself gives. Restricting the whole block to
// tools.LeanTools is the only shape that is correct in both states.
//
// The rule is no longer Kimi's alone: Codex and Gemini CLI carry the same
// client-side allowlist and get the same restraint through nameLeanToolsOnly in
// writeGenericGuidance. This block keeps its unconditional form because every
// line of it is already lean-only prose, and a flag it does not need would just
// be a way to get it wrong later (TestKimiCodeGuidance_LeanSetOnly pins it).
//
// A SOFT edit lane, not nativeEditLaneWarning. Kimi Code has its own file
// tools, but there is no evidence it enforces harness-side read-before-edit
// tracking, and that warning quotes Claude Code's exact harness error strings
// — quoting errors a Kimi user will never see would be worse than saying
// nothing. So this recommends the plumb lane on its merits instead.
func (t *SessionStart) writeKimiCodeGuidance(sb *strings.Builder) {
	sb.WriteString("## Tool guidance (Kimi Code)\n\n")
	sb.WriteString("**Edit lane.** Kimi Code has native file tools, so plumb is a choice here rather " +
		"than the only route — but route tracked edits through plumb: `read_file` returns an " +
		"mtime/sha header that `edit_file` takes back as `expected_mtime`, so the write is " +
		"concurrency-checked against exactly what you read, per-path locked, announced to " +
		"the language server, and reversible with `undo_edit`. Reading with plumb and then editing " +
		"natively gets you none of that. `write_file` and `transaction_apply` (one atomic " +
		"multi-file change) sit on the same lane.\n\n")
	if t.topologyActive() {
		sb.WriteString("Start from the Map, not from a text search:\n\n")
		sb.WriteString("- **topology_affected** — THE post-change tool: which tests to run after an edit " +
			"(dependency edges + co-location, recall-biased). No language server gives this.\n")
		sb.WriteString("- **topology_search** — ranked symbol/file search across the index. Use it before grep.\n")
		sb.WriteString("- **file_outline** — a file's shape (signatures, bodies collapsed) in ~200 tokens.\n\n")
	} else {
		sb.WriteString("Tip: enable the topology index (`[topology] enabled = true` in `.plumb/config.toml`) " +
			"for ranked search, file outlines, and `topology_affected` — which tests to run after a change.\n\n")
	}
	sb.WriteString("LSP-semantic — precise navigation no text search can do:\n\n")
	sb.WriteString("- **get_definition** / **find_references** — exact definition and all call sites, scope-aware.\n")
	sb.WriteString("- **workspace_symbols** — find a symbol by name across the workspace instantly.\n")
	sb.WriteString("- **rename_symbol** — workspace-wide semantic rename.\n")
	sb.WriteString("- **diagnostics** — live errors and warnings without running a build.\n\n")
	sb.WriteString("**run_task** runs the project's stored `[tasks.<lang>]` build/test/lint command with no " +
		"shell and bounded output — prefer it over shelling out to `go test`/`npm test`, for the slots the " +
		"Tasks section above lists as configured.\n\n")
	sb.WriteString("If plumb's full tool surface feels heavy, `plumb setup kimi-code --lean` writes a " +
		"client-side allowlist into Kimi's own mcp.json, trimming the loaded schemas to plumb's lean set.\n\n")
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
	sb.WriteString("- **find_files** / **search_in_files** — discover, list, and search the codebase.\n")
	sb.WriteString("- **git** — read-only git queries (status, log, diff, blame).\n\n")
	if t.topologyActive() {
		sb.WriteString("**Topology (the Map)** — in-process, always-on structural index:\n\n")
		sb.WriteString("- **topology_affected** — which tests to run after a change (the headline answer).\n")
		sb.WriteString("- **topology_search** — ranked symbol/file discovery across the index.\n")
		sb.WriteString("- **file_outline** — a file's shape in ~200 tokens without reading it.\n\n")
	}
	sb.WriteString("**LSP-semantic tools** (no equivalent without a language server):\n\n")
	sb.WriteString("- **workspace_symbols** / **get_definition** / **find_references** — find any symbol by name, " +
		"jump to its definition, list every call site (scope-aware, not text search).\n")
	sb.WriteString("- **rename_symbol** — workspace-wide semantic rename across all files.\n")
	sb.WriteString("- **diagnostics** — live compile errors and warnings from the language server.\n\n")
	sb.WriteString("If a plumb tool fails, retry or check `daemon_info`. Do not attempt native shell commands — they are unavailable.\n\n")
}
