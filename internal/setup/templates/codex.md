plumb is registered as an MCP server in this project — LSP-backed navigation and edits, a code-structure index, and per-project memory.

**apply_patch is off-limits for a file plumb has read.** It bypasses plumb's per-path concurrency guard and diagnostics gate — use `edit_file`/`write_file` instead (`transaction_apply` for one atomic multi-file change), passing back the `expected_mtime`/`expected_sha` from your last `read_file`.

**Edit lane.** Read a file with plumb before editing it (`read_file` -> `edit_file`/`write_file`). Mixing a plumb read with `apply_patch` on the same file produces "has not been read" / "modified since read" errors.

**Compile truth on write.** Pass `fail_on_new_errors` or `await_diagnostics` on an edit/write to have plumb catch (or report) a change that breaks the build, instead of finding out later.

More detail lives in each tool's own description and in `session_start`'s full output.
