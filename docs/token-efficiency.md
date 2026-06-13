# Token Efficiency & Agent Best Practices

Plumb is designed to minimize the "Token Tax" associated with using LLM agents in a codebase. This document outlines the best practices for both users and the agents themselves to keep conversations fast, cheap, and effective.

## The Mental Model: Where do tokens go?

The cost of an agent session is driven by the volume of text held in the conversation history. The biggest costs (in order) are:

1.  **Tool Output Bloat:** Dumping entire files, full test logs, or massive git diffs.
2.  **Redundant Reads:** Re-reading files to "verify" changes instead of trusting the tool's success state.
3.  **Schema Overhead:** The definitions of all available tools being sent on every turn.
4.  **Shadowed Context:** Multiple versions of the same file accumulating in the history as the agent works.

## Best Practices for Agents

### 1. Surgical Reads
*   **Never read a whole file** if you only need one function.
*   Use `list_symbols` or `find_symbol` to get line numbers first.
*   Use the `start_line` and `end_line` parameters in `read_file` to request only the necessary slice.

### 2. Optimistic Verification
*   Plumb's write tools (`edit_file`, `write_file`, `transaction_apply`) are designed to fail loudly if they cannot apply a change.
*   **Do not re-read a file immediately after writing it** unless you hit an error. Trust the tool's success response.

### 3. Symbolic Navigation
*   Use `workspace_symbols` to find where a type or function is defined across the whole project.
*   This is far cheaper than running `search_in_files` (grep) and then reading multiple files to find the definition.

### 4. Fragmented Logs
*   When investigating logs, use `grep` or `tail` equivalents. Do not read the entire log file into context.

### 5. Batch Reads Within a Turn
*   If you know up front that you need to read four files to plan an edit, use `read_multiple_files` once instead of four sequential `read_file` calls. One tool result is cheaper than four headers, and Anthropic's prompt cache hits the next turn either way.
*   Same rule for the LSP queries: a single `list_symbols` is cheaper than calling `find_symbol` four times.

### 6. Don't Read to Confirm What `git` or `diagnostics` Already Tells You
*   After a write, `diagnostics(uri)` reports whether the LSP server flagged anything. If it returns no errors and you trust the language server, that *is* the verification — re-reading the file just to "check it looks right" doubles your token cost on the round trip.
*   For multi-file refactors, `git({subcommand: "diff", args: ["--stat"]})` summarises what changed across the whole tree in one short response. Use it instead of re-reading each touched file.

## Anti-patterns From the Agent's Perspective

These are mistakes Claude (and other LLM agents) actually make in practice — captured here so they can be defended against either by the agent's own discipline or by plumb's defaults.

### Re-reading a file the agent itself just wrote
The single most common failure mode. The post-write response says "wrote 142 bytes", which feels too thin to "trust", so the agent calls `read_file` again to look at the result. This doubles the token cost of every write. On plumb's side, `edit_file`/`write_file` already return a unified diff by default (`[edits] show_write_diff`), so the success response shows exactly what changed; on the agent's side, the discipline is to stop reading unless an error was returned.

### Reading a 5 KB file to change one line
A diff that touches three lines doesn't justify reading 200 lines of surrounding code into the context window. `list_symbols → find_symbol → read_file(start_line=X, end_line=Y)` is the right ladder. The middle step exists precisely so the agent doesn't need a wide read.

### Calling `search_in_files` with a vague pattern, then reading every hit
A loose grep that returns 60 matches × ~200 bytes of context each is 12 KB of output that the agent will then partially re-read by opening files. Either tighten the pattern (often a quoted phrase or a regex anchor fixes it), or use `find_references` on the symbol — the LSP knows what the grep doesn't.

### Pasting tool output back to the user verbatim
Showing the user "here are the first 50 lines of the file" wastes both the agent's output budget and the user's reading time. Summarise: "the file defines X, Y, and Z; the bug is in Y at line 47". The user can ask plumb directly if they need the raw text.

### Running diagnostics on every workspace, not the changed URI
`diagnostics()` with no argument can return every error in the project. After editing one file, ask for diagnostics on that file's URI only.

### Treating each turn as a clean slate
If you read `internal/cli/daemon.go` two turns ago, it's still in context. Don't read it again — refer to what's already there. Likewise, if you've established that the project uses gopls, you don't need to call `session_start` again on the next turn.

### Acting on stale mtime
When an `edit_file` fails with a concurrent-write error, the agent's instinct is often to call `read_file` again purely to grab a new mtime. That works but pulls in the file body it doesn't need. Plumb could offer a lightweight `stat` (roadmap item below); until then, accept that the body re-read is necessary in this specific case, but don't over-generalise the pattern.

## Best Practices for Users

### 1. Task-Scoped Sessions
*   Start a fresh session when moving to an unrelated task. This clears out "cruft" from previous research that is no longer relevant.
*   A single session for a coherent task is better than multiple small sessions because of **Prompt Caching** (Claude's 5-minute cache TTL makes active sessions very cheap).

### 2. Path Referencing
*   Instead of pasting code into the chat, give the agent the path: *"Look at internal/cli/daemon.go and tell me..."*. The agent can then surgically read just the lines it needs.

### 3. Use Edit Over Write
*   `edit_file` is generally more token-efficient than `write_file` for existing files because the agent only needs to send the specific changes (`old_str` -> `new_str`) rather than the entire file content.

## Per-tool Token Guidance

Quick reference for the highest-traffic tools. Pick the parameter or pattern that matches the question you're trying to answer.

| Tool | Default cost | Cheaper alternative |
|---|---|---|
| `read_file` | ~1 token per byte (text) | Always pass `start_line` / `end_line` once you know them. Stream in slices, not full files. |
| `read_multiple_files` | Sum of per-file costs | Prefer over many sequential `read_file` calls when you know the set up front. |
| `list_files` | Linear in file count | Pair with a tight `pattern` glob. `**/*.go` is cheaper than dumping the whole tree. |
| `search_in_files` | Hit count × ~200 bytes per hit | Use a quoted phrase or anchored regex. Use `find_references` for symbol-shaped queries. |
| `find_replace` | Doubles in dry-run + commit cycle | Run dry-run *only when* you don't trust your pattern. For deterministic patterns, go straight to commit. |
| `workspace_symbols` | Compact (one line per symbol) | Almost always cheaper than the grep+read alternative. Use this first when navigating by name. |
| `list_symbols` | Compact tree per file | Use to plan a surgical `read_file` slice. |
| `get_definition` | Small (location only) | Don't chain into `read_file` unless you actually need the body. |
| `diagnostics` | Linear in error count | Pass a `uri` to scope to one file. Workspace-wide is for "give me the picture", not for routine verification. |
| `edit_file` | Cost of `old_str + new_str` | Far cheaper than `write_file` for any change under ~80% of file size. |
| `write_file` | Full new content | Use only for new files or whole-file replacements. |
| `transaction_apply` | Sum of operation costs | Cheaper than N separate `edit_file` calls when the changes are related — one tool result, not N. |
| `git diff` | Bounded; very efficient | The verification tool of choice for multi-file refactors. |
| `session_start` | Chunky upfront (~1-2 KB) | Call once per session. Subsequent re-orientation should reuse what's in context. |

## Prompt Caching Considerations

Anthropic's prompt cache has a 5-minute TTL on the conversation prefix. Plumb's design intersects with this in three ways:

1.  **Tool schemas are stable across turns.** Plumb registers the same 51 tools at session start; their schemas don't mutate during a conversation. This means the bulk of the system prompt (tool definitions) caches reliably across the whole session, and the per-turn marginal cost is dominated by *new* content (your messages + tool outputs).
2.  **Tool outputs do not cache.** Anything plumb returns is part of the conversation, not the cached prefix. Returning shorter outputs is *always* a direct win — there's no "cached call" rebate.
3.  **`session_start` output is not cached.** It runs once at session start, but its output sits in the conversation thereafter. Keep it lean by default and lazy-load (a roadmap item) for the heavy bits.

The practical agent rule: a single long-running session is cheaper than many short sessions for the *same* task because the cached prefix amortises. Switch sessions when the *task* changes, not when the conversation gets long.

## Future Roadmap for Plumb Efficiency

Features that would shift token efficiency from "the agent has to remember to be careful" to "plumb does it automatically." Ordered by estimated impact.

### High impact

*   **Automatic diff returns from write tools (shipped — `[edits] show_write_diff`, default on).** `edit_file`/`write_file` return a unified diff (or "no diff: nothing changed") instead of just a "wrote N bytes" line. Closes the largest single token sink (the post-write re-read). For very large diffs, return the diff stat + first hunk + "use `file_diff` for the rest".
*   **Error payloads designed for one-round recovery.** Today, `edit_file` failures like "old_str matched twice" send back the error and force the agent to re-read the file to figure out where. Instead: include the surrounding 5 lines of *each* match in the error, plus the file's current mtime. The agent gets enough to retry with a more specific `old_str` without burning a `read_file` call.
*   **`stat` tool.** A lightweight tool that returns only mtime + size + line count for a path. Used by agents recovering from concurrent-write errors so they don't have to re-read the body just to grab a fresh mtime.
*   **Mtime auto-propagation within a session.** Plumb already tracks per-session `ReadTracker` state including mtime. `edit_file` could default to the last-read mtime for that path automatically; the agent only needs to override it explicitly. Removes the verbose `expected_mtime` copy step from every edit.
*   **Smart output truncation with continuation handles.** Large `search_in_files`, `list_files`, or `find_references` results return the first N hits + a `next_page_token`. The token is short and lets the agent decide whether to spend more context on the rest. No need to truncate silently or dump everything.

### Medium impact

*   **Lazy memory loading.** `session_start` currently includes memories. Switch to descriptions only; the agent calls `read_memory(name)` if it actually needs the body. Pairs well with MCP Resources — memories already are resources, so this is mostly tuning what gets pulled vs referenced.
*   **Output-format hints.** A `format: "compact" | "default" | "verbose"` parameter on read tools, list tools, and diagnostics. Compact strips repeated path prefixes, drops surrounding context lines in search hits, and uses YAML-style key:value instead of indented JSON where appropriate. The agent opts into compact when it's running tight on context.
*   **Relative-path output mode.** Every output today repeats the workspace's absolute prefix. A connection-level "use relative paths" toggle would shave 30-60 bytes off every path mentioned in a tool result. Especially valuable for `list_files`, `find_files`, `git diff`.
*   **Context pruning advisories.** Plumb's stats DB knows the input/output sizes per tool call. Surface a "your last 10 `read_file` calls averaged 4 KB output — try `start_line/end_line`" hint inside `session_start`'s footer when the average is high. Self-correcting feedback for agents.
*   **Cap on session_start orientation packet.** Currently includes recent commits, recently-modified files, top-5 tool stats, active diagnostics, memory list. Each section is useful for *some* tasks and noise for others. A `sections: ["git", "memories"]` parameter lets the agent ask for what it needs.

### Lower impact / longer term

*   **Tool-subset registration.** A Go-only project doesn't need pyright-shaped tools in the schema. Conditional tool registration based on the resolved workspace language would cut the cached prompt prefix by ~5% (small per-call but compounds across all sessions).
*   **Result-handle returns.** A `find_replace` dry-run returns a short handle (e.g. `"fr-94a3e"`); committing references the handle rather than re-sending the pattern + paths. Saves a full duplication of the dry-run input on the commit call.
*   **`recent_edits()` tool.** A per-session record of writes the agent has made, returned as a compact list. Cheaper than scrolling back in the conversation to remember "what did I just do."
*   **Bundled symbol-explainer mega-tool.** `explain_full(symbol)` runs `find_symbol → get_definition → find_references → explain_symbol` in one tool call and returns a unified report. One tool-call round trip instead of four, with deduplicated output.
*   **Streaming / partial outputs.** For tools that can produce results incrementally (search, list), supporting incremental delivery would let the agent abort early if it has what it needs. Requires MCP protocol-level support — listed here as a watch-this-space item.

### Considered but rejected

*   **Output compression (gzip).** Tokens are charged on the decoded text; compression at the MCP transport layer doesn't help.
*   **Returning binary line numbers as integers in JSON.** Already done. Not a missed optimisation.
*   **Auto-summarising old conversation turns.** Out of scope for plumb — that's an agent-host responsibility (Claude Code, Claude Desktop) and forcing it from the MCP server side would break the assumption that tool outputs are stable history.
