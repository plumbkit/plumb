package clienttemplates

// DefaultTemplate is the client-agnostic body plumb renders into the MCP
// `initialize` response's `instructions` field for any client that has no
// entry in ByClient — one clientcaps does not recognise, or recognises but has
// no per-client body for yet.
//
// It is deliberately conservative rather than the richest thing any one client
// could show. An unrecognised client may be a lean-configured Codex or Gemini
// whose client-side tool allowlist (tools.LeanToolNames) strips the peer
// mailbox and every symbol-scoped edit tool — see client_templates.go's doc
// comment. Naming only lean-safe tools here means the body sent to an
// UNRECOGNISED client is never a false claim for whichever audience turns out
// to be the strictest one.
//
// It describes what plumb needs in order to work, and never instructs the
// agent to create a file: plumb writes nothing into a workspace it was not
// explicitly asked to (`plumb init`), and its guidance must not ask an agent
// to do so on its behalf either. Note that `plumb init` does create a
// `.plumb/` directory, which is why that line says to ask the USER to run it
// rather than telling the agent to — the removed clause ("or create it
// yourself if you have write authorisation") is what made it plumb's
// decision instead of theirs. The "Persisting this" line is deliberately a
// SUGGESTION, conditioned on a file the project already has and on asking the
// user — the agent decides whether these conventions belong in its own
// instruction file. plumb used to make that decision for the user by writing
// a managed block into AGENTS.md/CLAUDE.md/GEMINI.md itself; that is the
// behaviour this replaces.
//
// The edit-lane paragraph also deliberately does NOT quote the "has not
// been read" / "modified since read" strings — those are Claude Code
// HARNESS errors (internal/tools/edit_lane.go's isClaudeCode gate), not
// something Codex, Gemini, or an unrecognised client would ever see from
// mixing a native edit with a plumb read. This body describes the real,
// client-agnostic mechanic instead: a native edit bypasses plumb's own
// read-tracking, so write_file refuses on the next call against that file
// (internal/tools/write_file.go's session-aware staleness guard), and a
// GUARDED edit_file call (expected_mtime / expected_sha) refuses too
// (internal/tools/write_guards.go's verifyExpectedVersion) — but a bare,
// unguarded edit_file only WARNS (internal/tools/edit_file.go's
// staleReadNote), since its str_replace anchor already protects the edited
// region and the warning is informational, not a refusal.
//
// Size is guarded by internal/mcp's MaxInstructionsBytes, the budget for the
// channel this body is actually delivered over.
const DefaultTemplate = `plumb is registered as an MCP server in this project — LSP-backed navigation and edits, a code-structure index, and per-project memory. Prefer its tools over native file/search/git operations where both cover the same task.

If ` + "`session_start`" + ` reports the workspace as resolving or empty, plumb has no ` + "`.plumb/`" + ` workspace marker for this project, so its index and memory are unavailable — ask the user to run ` + "`plumb init`" + ` in the project root, which creates one.

**Edit lane.** Read a file with plumb before editing it (` + "`read_file`" + ` -> ` + "`edit_file`" + `/` + "`write_file`" + `), passing back ` + "`expected_mtime`" + `/` + "`expected_sha`" + `. If you edit that file with a native tool instead, plumb never sees the change — its own read-tracking goes stale, so your next ` + "`write_file`" + ` call, or an ` + "`edit_file`" + ` call passing ` + "`expected_mtime`" + `/` + "`expected_sha`" + `, on it is refused (` + "`edit_file`" + ` warns unless you pass that guard). Re-` + "`read_file`" + ` and retry.

**Compile truth on write.** Pass ` + "`fail_on_new_errors`" + ` or ` + "`await_diagnostics`" + ` on an edit/write to have plumb catch (or report) a change that breaks the build, instead of finding out later.

**Persisting this.** If this project already has an agent instruction file (` + "`AGENTS.md`" + `, or your client's own), these conventions are worth recording there — ask the user first; don't create the file just for this.

More detail lives in each tool's own description and in ` + "`session_start`" + `'s full output.`
