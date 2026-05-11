# plumb

**Real IDE intelligence for AI assistants** — go-to-definition, find-references, rename, diagnostics, and semantic edits, powered by the same language servers your editor uses.

Plumb is an [MCP](https://modelcontextprotocol.io) (Model Context Protocol) server that bridges LLMs to [LSP](https://microsoft.github.io/language-server-protocol/) (Language Server Protocol) language servers. Instead of dumping raw source files into an AI assistant's context window, plumb exposes structured semantic tools so the assistant can query a codebase the way an IDE would — finding symbols, jumping to definitions, listing references, applying scope-aware refactors — saving tokens and improving accuracy.

## Why plumb

LLM coding assistants typically reason about codebases by reading files into the context window. That approach is token-heavy, lossy at scale, and blind to symbol semantics — the assistant can't tell a method override from an unrelated identifier with the same name, can't safely rename a symbol across a project, and can't ask "where is this defined?" without re-reading every plausible file.

Plumb gives the assistant the same primitives your editor already has:

- **Semantic symbol search** scoped to a file or the whole workspace — case-sensitive, scope-aware, results filtered to the active project (no stdlib/dependency noise).
- **LSP-backed refactors** — `rename_symbol`, `replace_symbol_body`, `insert_before/after_symbol`, `safe_delete_symbol` — that respect scope, types, and references. `safe_delete_symbol` refuses to delete a symbol with remaining references rather than silently breaking the build.
- **Real diagnostics** — actual compiler/linter output from gopls or pyright, not LLM-guessed error patterns.
- **Per-workspace memory** — durable, file-backed notes that travel with the project (`<workspace>/.plumb/memories/`), exposed as MCP resources for clients that surface them.

**Supported languages:** Go (via [gopls](https://pkg.go.dev/golang.org/x/tools/gopls)) and Python (via [pyright](https://github.com/microsoft/pyright)).

**Architecture:** a shared background daemon keeps language servers warm across conversations, so the assistant doesn't pay re-indexing latency every time you open a new chat. Multi-workspace routing lets a single MCP session query and edit symbols across any number of projects — no "active workspace" to set up front. Per-project SQLite tool-call statistics let you see what the assistant actually does in each codebase.

## Quick start

**Prerequisites:** the language server(s) you need must be on `$PATH`:

```sh
# Go
go install golang.org/x/tools/gopls@latest

# Python
npm install -g pyright
```

```sh
# Install plumb
go install github.com/golimpio/plumb/cmd/plumb@latest

# Wire up Claude Desktop
plumb setup claude-desktop

# Wire up Claude Code (user-level — all projects)
plumb setup claude-code

# Wire up Claude Code (project-level — writes .mcp.json in cwd)
plumb setup claude-code --project

# Initialise a .plumb workspace in your project root (recommended)
cd /path/to/your/project
plumb init                # blank context.md
plumb init --discover     # auto-detect build system, entry points, test layout

# Monitor live sessions (TUI dashboard)
# ↑↓/jk navigate sessions; tab focuses the recent-calls panel
# (j/k then scrolls; selecting a failed call expands its error inline)
plumb          # or: plumb status

# View tool call statistics
plumb stats
plumb stats --workspace ~/Projects/myapp

# List all active sessions (non-interactive)
plumb sessions
plumb sessions --all      # include sessions still resolving a workspace

# Run diagnostics from the CLI (debugging tool)
plumb diag                        # workspace-wide — walks every file
plumb diag path/to/file.go        # single file

# Stop the background daemon
plumb stop

# Print version
plumb version
```

## How it works

When Claude Desktop opens a project, it invokes `plumb serve`. Rather than running a full server itself, `plumb serve` is a thin proxy — it dials a shared background daemon and forwards stdio. The daemon is started automatically on first use.

```
Claude Desktop / Claude Code
  └── plumb serve  (thin proxy per conversation)
        └── ~/Library/Caches/plumb/plumb.sock  (macOS)
        └── ~/.cache/plumb/plumb.sock           (Linux)
              └── plumb daemon  (one process, long-lived)
                    ├── gopls for /projects/foo
                    └── gopls for /projects/bar
```

Benefits:
- One gopls process per workspace, shared across all conversations about that project.
- Gopls stays warm between sessions — no re-indexing when you open a new chat.
- `plumb stop` shuts everything down cleanly.

## Workspace detection

Plumb determines the workspace root in this order:

1. **`.plumb/` directory** — run `plumb init` in your project root. This is the recommended approach; it takes priority over nested `go.mod` files (e.g. in testdata).
2. **MCP `roots/list`** — used if the client reports a root.
3. **`go.mod` walk** — the nearest `go.mod` above the first file URI seen.

## Available tools

### LSP tools (require a running language server)

| Tool | Description |
|---|---|
| `find_symbol` | Search for symbols by name within a single document (case-insensitive substring on names) |
| `workspace_symbols` | Search for symbols by name across the entire workspace (LSP `workspace/symbol`; fuzziness depends on the language server) |
| `get_definition` | Jump to the definition of the symbol at a given position |
| `explain_symbol` | Hover documentation and type information for a symbol |
| `list_symbols` | Complete symbol outline of a file — every function, type, method, field, and constant with line ranges |
| `find_references` | All usages of a symbol across the workspace, with source lines included |
| `call_hierarchy` | Who calls a function (incoming) and what it calls (outgoing) |
| `type_hierarchy` | Supertypes and subtypes for an interface or struct |
| `diagnostics` | Errors, warnings, and hints from the language server |

### Filesystem tools (no language server required)

| Tool | Description |
|---|---|
| `list_files` | Walk a directory tree with glob filtering and depth control |
| `search_in_files` | Ripgrep-style content search — regex, smart-case, context lines, glob filter |
| `find_files` | fd-style file finder — glob or regex, extension filter, type filter, depth limit |
| `file_diff` | Unified diff between any two files — no git required |
| `find_replace` | Text/regex search-and-replace across files (dry-run by default) |

### Edit tools (LSP-semantic refactoring)

| Tool | Description |
|---|---|
| `rename_symbol` | Workspace-wide rename via LSP — scope- and type-aware, won't touch unrelated identifiers |
| `replace_symbol_body` | Replace a symbol's entire declaration with new content |
| `insert_before_symbol` | Insert text immediately before a symbol's declaration |
| `insert_after_symbol` | Insert text immediately after a symbol's declaration |
| `safe_delete_symbol` | Delete a symbol only if it has no remaining references |

All edit tools default to `dry_run=true` — you see a preview before anything is written.

`insert_before_symbol`, `replace_symbol_body`, and `safe_delete_symbol` accept an optional `include_doc_comment` flag. LSP servers report a symbol's range starting at the declaration keyword, *excluding* the doc comment above it. With `include_doc_comment=true`, the operation extends to cover any contiguous comment lines (`//`, `#`, `/*`, `*`) directly above the symbol — so you can replace a function together with its doc comment, delete it without orphaning the comment, or insert a new declaration above an existing doc comment instead of between the comment and its symbol.

### Memory tools (per-workspace persistent notes)

| Tool | Description |
|---|---|
| `list_memories` | List memories saved at `<workspace>/.plumb/memories/` |
| `read_memory` | Read a memory by name |
| `write_memory` | Write or overwrite a memory (supports optional YAML frontmatter) |
| `delete_memory` | Delete a memory by name |
| `search_memories` | Grep across all memories — smart-case, regex |
| `relevant_memories` | Return memories whose `paths:` frontmatter globs match a given file |

Memories are also exposed as MCP **resources** under the `plumb-memory://` URI scheme, so Claude Desktop's resources panel browses them natively.

### VCS tools

| Tool | Description |
|---|---|
| `git` | Read-only git subcommands: diff, log, show, blame, status, branch, tag, shortlog, stash |

### Info tools

| Tool | Description |
|---|---|
| `version` | Plumb version, Go runtime, and OS/arch — useful for bug reports |

## Build from source

```sh
make build      # produces ./plumb, version stamped from git tag or VERSION file
make test-race  # full test suite with race detector
make lint       # golangci-lint
```

Requires Go 1.26+. gopls must be on `$PATH` for the Go adapter integration tests.

## Daemon logs

Daemon output is written to:
- **macOS:** `~/Library/Caches/plumb/daemon.log`
- **Linux:** `~/.cache/plumb/daemon.log`

## Development

### Versioning

The version is injected at build time. During development, bump the `VERSION` file:

```sh
echo "0.1.5" > VERSION
make build          # plumb version → 0.1.5
```

For a release, tag the commit — the tag takes precedence over `VERSION`:

```sh
git tag v1.0.0
make build          # plumb version → v1.0.0
```
