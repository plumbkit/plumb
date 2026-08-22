package setup

import "strings"

// DefaultVersion is the managed-block template version this build ships.
// Bumping it is how a template change rolls out: Check reports every
// installed block recorded against an older version as StatusStale, and
// Apply (via a bare re-register or `--sync`) restores it to the new one.
const DefaultVersion = "v1"

// DefaultTemplate is the client-agnostic managed-block body used two ways:
// (1) the fallback for any client that has no entry in ClientTemplates
// (client_templates.go) yet, and (2) — the important case — the body written
// to a file whose canonical path is shared by MORE than one instruction-
// capable client (this repo's own layout: CLAUDE.md/GEMINI.md symlink to
// AGENTS.md, so claude-code/codex/gemini all name one real file). See
// internal/cli/setup_instructions.go's templateForGroup.
//
// Case (2) is why this body is deliberately conservative rather than the
// richest thing any one client could show: a shared file may belong to a
// lean-configured Codex or Gemini, whose client-side tool allowlist
// (tools.LeanToolNames) strips the peer mailbox and every symbol-scoped edit
// tool — see client_templates.go's doc comment. Naming only lean-safe tools
// here means the body written to a SHARED file is never a false claim for
// whichever of its several audiences turns out to be the strictest one.
//
// The edit-lane paragraph also deliberately does NOT quote the "has not
// been read" / "modified since read" strings — those are Claude Code
// HARNESS errors (internal/tools/edit_lane.go's isClaudeCode gate), not
// something Codex or Gemini would ever see from mixing a native edit with a
// plumb read. This body describes the real, client-agnostic mechanic
// instead: a native edit bypasses plumb's own read-tracking, so write_file
// refuses on the next call against that file (internal/tools/write_file.go's
// session-aware staleness guard), and a GUARDED edit_file call (expected_mtime
// / expected_sha) refuses too (internal/tools/write_guards.go's
// verifyExpectedVersion) — but a bare, unguarded edit_file only WARNS
// (internal/tools/edit_file.go's staleReadNote), since its str_replace anchor
// already protects the edited region and the warning is informational, not a
// refusal.
const DefaultTemplate = `plumb is registered as an MCP server in this project — LSP-backed navigation and edits, a code-structure index, and per-project memory. Prefer its tools over native file/search/git operations where both cover the same task.

**Edit lane.** Read a file with plumb before editing it (` + "`read_file`" + ` -> ` + "`edit_file`" + `/` + "`write_file`" + `), passing back ` + "`expected_mtime`" + `/` + "`expected_sha`" + `. If you edit that file with a native tool instead, plumb never sees the change — its own read-tracking goes stale, so your next ` + "`write_file`" + ` call, or an ` + "`edit_file`" + ` call passing ` + "`expected_mtime`" + `/` + "`expected_sha`" + `, on it is refused (` + "`edit_file`" + ` warns unless you pass that guard). Re-` + "`read_file`" + ` and retry.

**Compile truth on write.** Pass ` + "`fail_on_new_errors`" + ` or ` + "`await_diagnostics`" + ` on an edit/write to have plumb catch (or report) a change that breaks the build, instead of finding out later.

More detail lives in each tool's own description and in ` + "`session_start`" + `'s full output.`

// MaxTemplateLines is the size budget every managed-block template must fit
// inside — the ops check-agents-brief.sh pattern applied to this template
// instead of the repo brief. A managed block earns its place in someone
// else's file only by staying short; if it needs more room the answer is a
// pointer to a skill or doc, not a longer block. Enforced by
// TestManagedBlock_TemplateSizeGuard.
const MaxTemplateLines = 25

// TemplateLineCount returns the number of lines in body — the same measure
// TemplateWithinBudget applies, and the one an author bumping DefaultTemplate
// should check before committing.
func TemplateLineCount(body string) int {
	if body == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(body, "\n"), "\n") + 1
}

// TemplateWithinBudget reports whether body fits inside MaxTemplateLines.
func TemplateWithinBudget(body string) bool {
	return TemplateLineCount(body) <= MaxTemplateLines
}
