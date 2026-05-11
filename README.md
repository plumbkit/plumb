# plumb

**Real IDE intelligence for AI assistants** — go-to-definition, find-references, rename, diagnostics, file editing, and semantic refactors, powered by the same language servers your editor uses.

Plumb is an [MCP](https://modelcontextprotocol.io) (Model Context Protocol) server that bridges AI assistants to [LSP](https://microsoft.github.io/language-server-protocol/) (Language Server Protocol) language servers. Instead of dumping raw source files into the assistant's context window, plumb exposes structured tools so the assistant can query and edit a codebase the way an IDE would — finding symbols, jumping to definitions, applying scope-aware renames, reading and writing files, and getting real compiler diagnostics. This saves tokens, improves accuracy, and keeps the language server's view consistent with every change.

## Why plumb

LLM coding assistants typically work by reading files into the context window. That approach is token-heavy, lossy at scale, and blind to symbol semantics — the assistant can't tell a method override from an unrelated identifier with the same name, can't safely rename a symbol across a project, and has no way to read a specific function without loading the whole file.

Plumb gives the assistant the same primitives your editor already has:

- **Semantic symbol search** scoped to a file or the whole workspace, filtered to your code (no stdlib or dependency noise).
- **LSP-backed refactors** — `rename_symbol`, `replace_symbol_body`, `insert_before/after_symbol`, `safe_delete_symbol` — that understand scope, types, and references.
- **Real diagnostics** — actual compiler and linter output from gopls or pyright, not guessed error patterns.
- **Safe file I/O** — `read_file`, `write_file`, `edit_file` with atomic writes, concurrent-write detection, and automatic LSP notification after every change.
- **Per-workspace memory** — durable markdown notes that travel with the project, exposed as MCP resources in Claude Desktop's sidebar.
- **Session bootstrap** — `session_start` orients the assistant in one round-trip: workspace path, project context, saved memories, recent tool usage, and active diagnostics.

**Supported languages:** Go (via [gopls](https://pkg.go.dev/golang.org/x/tools/gopls)) and Python (via [pyright](https://github.com/microsoft/pyright)).

## How it works

`plumb serve` is a thin stdio proxy. When Claude Desktop opens a project, it calls `plumb serve`, which connects to a shared background daemon and forwards the MCP protocol over a Unix socket. The daemon manages one language server process per workspace, shared across all conversations about that project.

```
Claude Desktop / Claude Code
  └── plumb serve  (thin proxy, one per conversation)
        └── ~/Library/Caches/plumb/plumb.sock   (macOS)
        └── ~/.cache/plumb/plumb.sock            (Linux)
              └── plumb daemon  (one shared process)
                    ├── gopls for /projects/foo
                    └── gopls for /projects/bar
```

Benefits:
- One gopls process per workspace, shared across all conversations about that project.
- Gopls stays warm between sessions — no re-indexing when you open a new chat.
- A single MCP connection can query and edit symbols across any number of projects.

## Quick start

**Prerequisites:** the language server(s) you need must be on `$PATH`:

```sh
# Go
go install golang.org/x/tools/gopls@latest

# Python (optional)
npm install -g pyright
```

```sh
# Install plumb
go install github.com/golimpio/plumb/cmd/plumb@latest

# Wire up Claude Desktop (writes to its MCP config automatically)
plumb setup claude-desktop

# Wire up Claude Code — user-level (applies to all projects)
plumb setup claude-code

# Wire up Claude Code — project-level (writes .mcp.json in cwd)
plumb setup claude-code --project

# Initialise a .plumb workspace in your project root (recommended)
cd /path/to/your/project
plumb init              # creates .plumb/context.md
plumb init --discover   # auto-detects build system, entry points, test layout

# Monitor live sessions
plumb          # or: plumb status

# View tool call statistics for the current project
plumb stats
plumb stats --workspace ~/Projects/myapp

# List active sessions
plumb sessions
plumb sessions --all    # include sessions still resolving a workspace

# Run diagnostics from the terminal
plumb diag              # workspace-wide
plumb diag path/to/file.go

# Stop the daemon
plumb stop
```

## Workspace detection

Plumb determines the workspace root using this precedence:

1. **`.plumb/` directory** — running `plumb init` in your project root creates this marker. It takes priority over nested `go.mod` files (e.g. in `testdata/`). Recommended.
2. **MCP `roots/list`** — used if the client reports a root (Claude Desktop does this).
3. **`go.mod` walk** — the nearest `go.mod` above the first file URI seen by the daemon.

## Available tools

### Session

| Tool | Description |
|---|---|
| `session_start` | Bootstrap tool — call first in every session. Returns workspace info, project context, memories, recent tool usage, and active diagnostics in one call. Designed for Claude Desktop where no filesystem access is available without tool calls. |

### LSP tools — require a running language server

| Tool | Description |
|---|---|
| `find_symbol` | Search for symbols by name within a single file (case-insensitive substring) |
| `workspace_symbols` | Search for symbols across the entire workspace |
| `get_definition` | Jump to the definition of a symbol at a given position |
| `explain_symbol` | Hover documentation and type information for a symbol |
| `list_symbols` | Complete symbol outline of a file with line ranges |
| `find_references` | All usages of a symbol across the workspace, with source lines |
| `call_hierarchy` | Who calls a function and what it calls |
| `type_hierarchy` | Supertypes and subtypes for an interface or struct |
| `diagnostics` | Errors, warnings, and hints from the language server |

### File I/O

| Tool | Description |
|---|---|
| `read_file` | Read a file by path or `file://` URI. Supports line ranges for large files. Binary files are detected and rejected. Output capped at 200 KiB. |
| `read_multiple_files` | Read up to 20 files in a single call. Errors for individual files are reported inline. |
| `write_file` | Create or overwrite a file atomically. Content is staged in the system temp directory and renamed into place — never a partial write. Notifies the LSP server after writing. |
| `edit_file` | Apply one or more str_replace edits to an existing file. Each `old_str` must appear exactly once — absent or ambiguous strings are rejected, preventing silent corruption. Retries automatically if a concurrent write is detected (up to 3 times). |
| `list_directory` | List immediate directory contents with `[FILE]`/`[DIR]` type prefixes, sizes, and modification times. |
| `list_files` | Recursively walk a directory tree with glob filtering and depth control |
| `search_in_files` | Ripgrep-style content search — regex, smart-case, context lines, glob filter |
| `find_files` | fd-style file finder — glob or regex, extension filter, type filter, depth limit |
| `file_diff` | Unified diff between any two files — no git required |
| `find_replace` | Text/regex search-and-replace across files (dry-run by default) |

### LSP semantic edits

All edit tools default to `dry_run=true` — you see a preview before anything is written.

| Tool | Description |
|---|---|
| `rename_symbol` | Workspace-wide rename via LSP — scope- and type-aware |
| `replace_symbol_body` | Replace a symbol's entire declaration |
| `insert_before_symbol` | Insert text immediately before a symbol's declaration |
| `insert_after_symbol` | Insert text immediately after a symbol's declaration |
| `safe_delete_symbol` | Delete a symbol only if it has no remaining references |

`insert_before_symbol`, `replace_symbol_body`, and `safe_delete_symbol` accept an optional `include_doc_comment` flag. When true, the operation extends to cover any contiguous comment lines directly above the symbol declaration — so you can replace a function together with its doc comment, delete it without orphaning the comment, or insert above an existing doc comment rather than between the comment and its symbol.

### Memory

Memories are markdown notes stored at `<workspace>/.plumb/memories/<name>.md`. They persist project-specific context — conventions, architectural decisions, gotchas — across conversations. Each memory may carry YAML frontmatter with a `description` (shown in listings) and a `paths:` field (for auto-attaching relevant memories to specific files).

Memories are also exposed as MCP **resources** (`plumb-memory://` URI scheme) so Claude Desktop's resources panel can browse them natively. The project `context.md` is exposed as `plumb://workspace/context`.

| Tool | Description |
|---|---|
| `list_memories` | List all memories for a workspace |
| `read_memory` | Read a memory by name |
| `write_memory` | Write or overwrite a memory |
| `delete_memory` | Delete a memory by name |
| `search_memories` | Grep across all memories — smart-case, regex |
| `relevant_memories` | Return memories whose `paths:` globs match a given file |

### VCS

| Tool | Description |
|---|---|
| `git` | Read-only git subcommands: diff, log, show, blame, status, branch, tag, shortlog, stash |

### Info

| Tool | Description |
|---|---|
| `version` | Plumb version, Go runtime, and OS/arch |

## MCP Prompts

Plumb exposes three named workflows that Claude Desktop surfaces as menu items:

| Prompt | Description |
|---|---|
| `orient` | Calls `session_start` and delivers a structured project summary — what it does, architecture, active diagnostics, recent activity. |
| `whats-broken` | Chains `session_start` → `diagnostics` → `read_file` per broken file → triage and suggested fixes. |
| `recent-changes` | Chains `session_start` → `git log` → `git diff --stat` → `diagnostics` for a recent-activity summary. |

All prompts accept an optional `workspace` argument. `recent-changes` also accepts `since` (e.g. `'1 week ago'` or a commit SHA).

## Project context

Running `plumb init` creates `.plumb/context.md` — a markdown file you can fill with project-specific context: what the project does, architecture decisions, conventions, and known gotchas. The `session_start` tool loads the first 80 lines of this file automatically. Claude Desktop also shows it in the resources sidebar as "Project context".

```markdown
## Overview
A Go MCP server bridging AI assistants to language servers.

## Architecture
Layered: transport → tools → CLI. Lower layers never import higher.

## Conventions
Australian English in all prose. gofumpt on save. log/slog exclusively.

## Known gotchas
gopls initialisation is lazy — workspace resolves on first tool call.
```

## Monitoring

The `plumb` command (alias: `plumb status`) opens a live TUI dashboard showing active sessions, tool call statistics, and recent calls per session.

```
plumb 0.4.1
╭─ Sessions (1) ──────────────┬─ Session + Stats ──────────────────────────
│                             ┆
│▸ go: ~/Projects/myapp       ┆  ID          abc123-def456
│                             ┆  Language    go
│                             ┆  Folder      ~/Projects/myapp
│                             ┆  Adapter     gopls
│                             ┆  PID         12345
│                             ┆  Daemon      0.4.1
│                             ┆  Started     2026-05-11 14:00:00
│                             ┆  Client      claude-ai 0.1.0
│                             ┆
│                             ┆  ── Tool Statistics ──
│                             ┆  write_file           4 calls   8ms avg
│                             ┆  edit_file            6 calls  12ms avg
│                             ┆  search_in_files      9 calls  16ms avg
│                             ┆  workspace_symbols    3 calls  38ms avg
│                             ┆  diagnostics          2 calls   0ms avg
╰─────────────────────────────┴────────────────────────────────────────────
```

Navigation: `↑↓`/`jk` moves sessions, `tab` focuses the recent-calls panel (then `j`/`k` to scroll, selecting a failed call expands its error inline), `a` shows hidden sessions, `[`/`]` resizes the left panel, `q` quits.

## File write safety

All file writes in plumb use the same layered safety model:

1. **Atomic rename** — content is staged in `os.TempDir()` and renamed into place. `os.Rename` is a single POSIX syscall — the target is never partially written. If the temp directory and target are on different filesystems (EXDEV), plumb falls back to a `.plumb.tmp` sibling in the same directory automatically. No permanent backup files are left in your project tree.

2. **Uniqueness lock** (`edit_file`) — each `old_str` must appear exactly once. This is the concurrency safety lock: if the file was modified between when the assistant read it and when it issues the edit, the old string will be absent or match different context, and the edit is rejected cleanly. No silent corruption is possible.

3. **Concurrent-write retry** (`edit_file`) — after the rename, plumb re-stats the file. If the mtime is significantly newer than the write time, a third party wrote the file during the operation. The edit is automatically re-read and re-applied up to 3 times before failing with a diagnostic message.

4. **LSP notification** — after every successful write, plumb sends `didOpen`/`didChange`/`didClose` to the language server so diagnostics and symbol lookups reflect the new content without requiring an editor to open the file.

## Statistics

Tool call statistics are stored in a per-project SQLite database at `<workspace>/.plumb/stats.db`. The TUI and `plumb stats` read from it concurrently with the daemon writing, using WAL journal mode.

```sh
plumb stats                         # current directory's project
plumb stats --workspace ~/Projects/myapp
plumb stats --session <session-id>  # one session only
```

## Daemon logs

| Platform | Path |
|---|---|
| macOS | `~/Library/Caches/plumb/daemon.log` |
| Linux | `~/.cache/plumb/daemon.log` |

## Build from source

```sh
make build      # produces ./plumb, version stamped from git tag or VERSION file
make test-race  # full test suite with race detector
make lint       # golangci-lint
```

Requires Go 1.22+. `gopls` must be on `$PATH` for integration tests.

## Versioning

The version is injected at build time. To bump during development, edit the `VERSION` file:

```sh
echo "0.4.1" > VERSION
make build
```

For a release, tag the commit — the tag takes precedence:

```sh
git tag v0.4.1
make build
```

## Contributing

See `AGENTS.md` for architecture details, code style rules, and the checklist for adding new tools or LSP adapters.
