package tools

import "fmt"

// This file centralises the "stay in the plumb edit lane" guidance that
// session_start and read_file both surface, so the two can never drift.
//
// Why it exists: Claude Code (and any MCP client that ships its own native file
// Read/Edit tools) maintains a file read-state tracker that is independent of
// plumb's. A plumb read_file does NOT satisfy the harness's "you must Read
// before Edit" rule, so mixing a plumb read_file with the client-native Edit
// tool fails with "File has not been read yet" — and a native Edit after any
// external change fails with "File has been modified since read". These look
// like plumb errors but are produced by the harness when the two toolsets are
// mixed. plumb cannot satisfy the harness's tracker from the MCP side, so the
// only fix plumb can offer is guidance: keep reads and edits both on plumb
// (read_file -> edit_file, reusing the mtime header as expected_mtime).

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
// of the Claude Code tool-guidance block. The exact harness error strings are
// included verbatim so an agent that has already hit one can recognise it here.
const nativeEditLaneWarning = "> **Edit lane — read this before editing.** " +
	"Use plumb's `read_file` then `edit_file` for every in-workspace file change. " +
	"Do **NOT** use Claude Code's native Read / Edit / Write tools on files in this workspace. " +
	"plumb and the Claude Code harness track file read-state **separately**: a plumb `read_file` " +
	"does not satisfy the harness's read-before-edit rule, so a native `Edit` after a plumb " +
	"`read_file` fails with \"File has not been read yet\", and a native `Edit` after any external " +
	"change fails with \"File has been modified since read\". These are not plumb errors — they " +
	"come from mixing the two toolsets. Stay in one lane: `read_file` then `edit_file` " +
	"(reuse the mtime from read_file's header as `expected_mtime`).\n\n"

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
