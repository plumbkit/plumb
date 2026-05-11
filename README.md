# plumb

Plumb is an MCP (Model Context Protocol) server that exposes LSP (Language Server Protocol) capabilities to LLMs. Instead of dumping raw source files into context, plumb lets LLMs query codebases through structured semantic tools — finding symbols, jumping to definitions, listing references, running git queries, and more — saving tokens and improving accuracy.

The server is consumed by MCP clients such as Claude Desktop and Claude Code. It ships with 27 tools across LSP queries, semantic refactoring, filesystem search/edit, git, and a per-workspace memory system. A shared-daemon architecture keeps gopls/pyright warm across conversations, and multi-workspace routing lets a single MCP session query and edit symbols in any number of projects without pre-declaring an "active" one.

## Quick start

```sh
# Install
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
plumb          # or: plumb status

# View tool call statistics
plumb stats
plumb stats --workspace ~/Projects/myapp

# List all active sessions (non-interactive)
plumb sessions

# Run gopls diagnostics from the CLI (debugging tool)
plumb diagnostics             # workspace-wide
plumb diagnostics path/to/file.go

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
| `find_symbol` | Search for a symbol by name across the workspace |
| `workspace_symbols` | Fuzzy-search symbols across the entire workspace |
| `get_definition` | Jump to the definition of a symbol |
| `explain_symbol` | Hover documentation for a symbol |
| `list_symbols` | All symbols in a file with line ranges and hierarchy |
| `find_references` | All usages of a symbol, with source lines |
| `call_hierarchy` | Incoming or outgoing call graph for a function |
| `type_hierarchy` | Supertypes and subtypes for an interface or struct |
| `diagnostics` | Errors, warnings, and hints from the language server |

### Filesystem tools (no language server required)

| Tool | Description |
|---|---|
| `list_files` | Walk a directory tree with glob filtering |
| `search_in_files` | Ripgrep-style content search — smart-case, regex, context lines |
| `find_files` | fd-style file finder — glob or regex, extension filter, max depth |
| `file_diff` | Unified diff between two files |
| `find_replace` | Text/regex search-and-replace across files (dry-run by default) |

### Edit tools (LSP-semantic refactoring)

| Tool | Description |
|---|---|
| `rename_symbol` | Workspace-wide rename via LSP (scope- and type-aware) |
| `replace_symbol_body` | Replace a symbol's full declaration |
| `insert_before_symbol` | Insert text immediately before a symbol's declaration |
| `insert_after_symbol` | Insert text immediately after a symbol's declaration |
| `safe_delete_symbol` | Delete a symbol only if it has no remaining references |

### Memory tools (per-workspace persistent notes)

| Tool | Description |
|---|---|
| `list_memories` | List memories saved at `<workspace>/.plumb/memories/` |
| `read_memory` | Read a memory by name |
| `write_memory` | Write or overwrite a memory (with optional frontmatter) |
| `delete_memory` | Delete a memory by name |
| `search_memories` | Grep across all memories (smart-case, regex) |
| `relevant_memories` | Return memories whose `paths:` frontmatter globs match a file |

Memories are also exposed as MCP **resources** under the `plumb-memory://` URI scheme, so Claude Desktop's resources panel browses them natively.

### VCS tools

| Tool | Description |
|---|---|
| `git` | Read-only git subcommands (diff, log, blame, status, show, stash, …) |

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

## Versioning

The version is injected at build time. During development, bump the `VERSION` file:

```sh
echo "0.1.5" > VERSION
make build          # plumb version → 0.1.5
```

For a release, tag the commit and the tag takes precedence over `VERSION`:

```sh
git tag v1.0.0
make build          # plumb version → v1.0.0
```

## Daemon logs

Daemon output is written to `~/Library/Caches/plumb/daemon.log` (macOS) or `~/.cache/plumb/daemon.log` (Linux).
