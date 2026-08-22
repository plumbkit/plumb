plumb is registered as an MCP server in this project — LSP-backed navigation and edits, a code-structure index, and per-project memory. Prefer its tools over native file/search/git operations where both cover the same task.

**Edit lane.** Read a file with plumb before editing it (`read_file` -> `edit_file`/`write_file`), passing back `expected_mtime`/`expected_sha`. Mixing a plumb read with a native edit on the same file produces "has not been read" / "modified since read" errors.

**Compile truth on write.** Pass `fail_on_new_errors` or `await_diagnostics` on an edit/write to have plumb catch (or report) a change that breaks the build, instead of finding out later.

**Peers.** If another agent may be working in this workspace, use `check_messages`/`leave_note` — delivery is poll-only, so silence is not refusal.

**Subagents.** Call `session_start({detail:"brief"})` first for a short orientation packet.

More detail lives in each tool's own description, in the plumb skills (`plumb skills sync claude-code` installs them), and in `session_start`'s full output.
