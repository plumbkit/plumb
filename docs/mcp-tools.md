# Available tools (48)

## Client capabilities and fallback behaviour

| Client | Native filesystem | Native shell/git | Notes |
|---|---|---|---|
| Claude Desktop | None | None | Plumb is the **only** interface — no fallback tools exist |
| Claude Code | `Read` / `Edit` / `Write` | `Bash` | Plumb adds LSP-semantic tools with no native equivalent |
| Codex | Shell (`shell` tool) | Yes | Plumb adds LSP-semantic layer and concurrency-safe writes |
| Gemini CLI | Filesystem tools | Yes | Plumb adds LSP-semantic layer and concurrency-safe writes |

**Implication for tool error messages:** When a plumb tool fails for a Claude Desktop user, the error must not suggest native alternatives (`cat`, `grep`, shell commands) — they are unavailable. Suggest retry or `daemon_info` instead.

**Implication for token savings:** For Claude Desktop, plumb's value is better expressed as "capabilities enabled" rather than "tokens saved vs alternative" — there is no alternative to compare against.

---

Plumb exposes 48 structured tools to AI assistants. Every write tool is concurrency-safe, atomic, and LSP-notified.

## Session
| Tool | Description |
|---|---|
| `session_start` | Bootstrap tool — call first in every session. Returns workspace info, language, git branch, recent commits, recently-modified files, memories, top-5 tool usage, and active diagnostics. |
| `daemon_info` | Current session name and ID, daemon version, daemon start timestamp, and uptime. |
| `rename_session` | Rename the current MCP session. |

## LSP Queries
| Tool | Description |
|---|---|
| `find_symbol` | Search for symbols by name within a single file. |
| `workspace_symbols` | Search for symbols across the entire workspace. |
| `get_definition` | Jump to the definition of a symbol. |
| `explain_symbol` | Hover documentation and type information. |
| `list_symbols` | Complete symbol outline of a file with line ranges. |
| `find_references` | All usages of a symbol across the workspace. |
| `call_hierarchy` | Incoming and outgoing call graphs. |
| `type_hierarchy` | Supertypes and subtypes. |
| `diagnostics` | Errors, warnings, and hints from the language server. Pass `uris` (array) to check specific files; omit for all files. A single call replaces multiple per-file calls. |

## LSP Semantic Edits
| Tool | Description |
|---|---|
| `rename_symbol` | Workspace-wide rename via LSP — scope- and type-aware. |
| `replace_symbol_body` | Replace a symbol's entire declaration. |
| `insert_before_symbol` | Insert text immediately before a symbol's declaration. |
| `insert_after_symbol` | Insert text immediately after a symbol's declaration. |
| `safe_delete_symbol` | Delete a symbol only if it has no remaining references. |

## Filesystem Reads
| Tool | Description |
|---|---|
| `read_file` | Read a file (supports line ranges and mtime headers). |
| `read_multiple_files` | Up to 20 files in parallel. |
| `list_directory` | Immediate children with metadata. |
| `list_files` | Recursive walk with glob filtering. |
| `find_files` | Glob/regex finder. |
| `search_in_files` | content search with symbol annotation. |

## Filesystem Writes
| Tool | Description |
|---|---|
| `write_file` | Create or overwrite a file atomically. |
| `edit_file` | Targeted str_replace with uniqueness locks. |
| `delete_file` | Atomic delete. |
| `rename_file` | **Primary move tool.** Atomic move/rename; notifies LSP with FileDeleted+FileCreated. |
| `copy_file` | Duplicate a file preserving permissions. Cross-device safe. Notifies LSP with FileCreated. |
| `transaction_apply` | Multi-file atomic edits with rollback. |

## Memory
| Tool | Description |
|---|---|
| `list_memories` | List all memories for a workspace. |
| `read_memory` | Read a memory by name. |
| `write_memory` | Create or overwrite a memory. |
| `delete_memory` | Delete a memory by name. |
| `search_memories` | Pattern search across memory bodies. |
| `relevant_memories` | Filter memories by file path relevance. |

## Topology
SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`. Enabled via `[topology] enabled = true` in `.plumb/config.toml`. All tools degrade gracefully when topology is disabled.
| Tool | Description |
|---|---|
| `topology_status` | Index health: file count, entity count, DB size, indexed languages, last sync time, last error. |
| `topology_search` | FTS5 ranked symbol/file search. Inputs: `query`, optional `kinds`/`language` filters, `limit` (default 20), `include_snippets`. |
| `topology_explore` | BFS neighbourhood around a named symbol. Configurable depth, node, and byte budgets. |
| `topology_impact` | Bidirectional blast-radius analysis. Two sub-graphs: what a symbol depends on and what depends on it. Use before refactoring to assess scope. |
| `topology_affected` | Given changed files or symbols, returns likely affected files and tests. Use after writing to suggest which tests to run. |
| `topology_routes` | Framework-aware entry-point scanner (Go HTTP handlers, Cobra commands, Python `@app.route`). Results annotated with confidence — heuristic. |

## VCS & Utils
| Tool | Description |
|---|---|
| `git` | Read-only git subcommands. |
| `git_add` | Stage explicit file paths for the next commit. |
| `git_commit` | Commit whatever is currently staged. |
| `git_init` | Initialise a git repository. |
| `file_diff` | Unified diff between any two files. |
| `find_replace` | Dry-run search-and-replace with formatting. |
| `version` | Plumb version and runtime info. |
