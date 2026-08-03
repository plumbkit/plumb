package tools

import "fmt"

// This file renders the always-on edit-lane safety line that session_start and
// read_file both surface. It is deliberately only that line: the expanded
// workflow — read → mtime → edit, atomic multi-file edits, semantic rename —
// belongs to the plumb-refactor skill, which is the canonical source for it.
// What stays here is the minimum every client must see whether or not it has
// the skills installed.
//
// Why it exists at all: Claude Code (and any MCP client that ships its own
// native file Read/Edit tools) maintains a file read-state tracker independent
// of plumb's. A plumb read_file does NOT satisfy the harness's "you must Read
// before Edit" rule, and plumb cannot satisfy that tracker from the MCP side,
// so the only fix plumb can offer is guidance.

// clientHasNativeEditConflict reports whether the MCP client has its own native
// file Read/Edit tools whose read-state tracking is independent of plumb's —
// the condition under which mixing a plumb read_file with the client-native
// Edit tool produces a spurious "File has not been read yet" error. Claude Code
// is the confirmed case; extend the predicate here as other agentic CLIs are
// validated. Clients with no native file tools (e.g. Claude Desktop) never hit
// this and must not receive the warning.
func clientHasNativeEditConflict(fn func() string) bool {
	return isClaudeCode(fn)
}

// nativeEditLaneWarning is the prominent callout session_start places at the top
// of the Claude Code tool-guidance block. The two harness error strings stay
// verbatim — they are the recognition hook for an agent that has already hit
// one. Everything else is deliberately terse and defers to the skill.
const nativeEditLaneWarning = "> **Edit lane — read this before editing.** " +
	"Every in-workspace file change goes `read_file` → `edit_file` (reuse the mtime header as " +
	"`expected_mtime`), never Claude Code's native Read / Edit / Write. Mixing the two toolsets " +
	"is what produces \"File has not been read yet\" and \"File has been modified since read\" — " +
	"harness errors, not plumb's. Details: the plumb-refactor skill.\n\n"

// nativeEditReadHint is the short call-to-action read_file appends as a second
// header comment line for clients with the native-edit conflict. It names the
// exact mtime so the follow-up edit_file call is copy-paste ready, and names
// the anti-pattern (the native Edit tool) at the precise moment the agent is
// about to act on the file it just read.
func nativeEditReadHint(mtime string) string {
	return fmt.Sprintf(
		"# To edit: use edit_file (not the native Edit tool) with expected_mtime=%s\n",
		mtime,
	)
}
