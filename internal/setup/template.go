package setup

import "strings"

// DefaultVersion is the managed-block template version this build ships.
// Bumping it is how a template change rolls out: Check reports every
// installed block recorded against an older version as StatusStale, and
// Apply (via a bare re-register or `--sync`) restores it to the new one.
const DefaultVersion = "v1"

// DefaultTemplate is the generic managed-block body every client gets until
// per-client templates (a separate change) replace it with one template per
// client. It deliberately omits any client-specific claim — like Codex's
// apply_patch countermand — that only a per-client template can make safely,
// per the pointer-style content this block is meant to carry: the edit lane,
// the write-time compile-truth flags, the peer mailbox, and a pointer for
// subagents. No lore.
const DefaultTemplate = `plumb is registered as an MCP server in this project — LSP-backed navigation and edits, a code-structure index, and per-project memory. Prefer its tools over native file/search/git operations where both cover the same task.

**Edit lane.** Read a file with plumb before editing it (` + "`read_file`" + ` -> ` + "`edit_file`" + `/` + "`write_file`" + `), passing back ` + "`expected_mtime`" + `/` + "`expected_sha`" + `. Mixing a plumb read with a native edit on the same file produces "has not been read" / "modified since read" errors.

**Compile truth on write.** Pass ` + "`fail_on_new_errors`" + ` or ` + "`await_diagnostics`" + ` on an edit/write to have plumb catch (or report) a change that breaks the build, instead of finding out later.

**Peers.** If another agent may be working in this workspace, use ` + "`check_messages`" + `/` + "`leave_note`" + ` — delivery is poll-only, so silence is not refusal.

**Subagents.** Call ` + "`session_start({detail:\"brief\"})`" + ` first for a short orientation packet.

More detail lives in each tool's own description, in the plumb skills installed alongside this file, and in ` + "`session_start`" + `'s full output.`

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
