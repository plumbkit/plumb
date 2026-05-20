# Available tools (34)

Plumb exposes 34 structured tools to AI assistants. Every write tool is concurrency-safe, atomic, and LSP-notified.

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
| `diagnostics` | Errors, warnings, and hints from the language server. |

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
| `rename_file` | Atomic move/rename. |
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

## VCS & Utils
| Tool | Description |
|---|---|
| `git` | Read-only git subcommands. |
| `file_diff` | Unified diff between any two files. |
| `find_replace` | Dry-run search-and-replace with formatting. |
| `version` | Plumb version and runtime info. |
