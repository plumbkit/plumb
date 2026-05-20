# plumb

**Real IDE intelligence for AI assistants** — go-to-definition, find-references, rename, diagnostics, atomic file editing, and semantic refactors, powered by the same language servers your editor uses.

Plumb is an [MCP](https://modelcontextprotocol.io) (Model Context Protocol) server that bridges AI assistants to [LSP](https://microsoft.github.io/language-server-protocol/) (Language Server Protocol) language servers. Instead of dumping raw source files into the assistant's context window, plumb exposes 33 structured tools so the assistant can query and edit a codebase the way an IDE would — finding symbols, jumping to definitions, applying scope-aware renames, reading and writing files atomically, and getting real compiler diagnostics back inline with every write. This saves tokens, improves accuracy, and keeps the language server's view consistent with every change.

## Why plumb

LLM coding assistants typically work by reading files into the context window. That approach is token-heavy, lossy at scale, and blind to symbol semantics — the assistant can't tell a method override from an unrelated identifier with the same name, can't safely rename a symbol across a project, and has no way to read a specific function without loading the whole file.

Plumb gives the assistant the same primitives your editor already has:

- **Semantic symbol search** scoped to a file or the whole workspace, filtered to your code (no stdlib or dependency noise).
- **LSP-backed refactors** — `rename_symbol`, `replace_symbol_body`, `insert_before/after_symbol`, `safe_delete_symbol` — that understand scope, types, and references.
- **Real diagnostics inline with every write** — actual compiler output from gopls or pyright is appended to `write_file` and `edit_file` responses, so the assistant learns it broke the build in the same turn.
- **Concurrency-safe file I/O** — atomic writes, per-path locks across all write tools, symlink-aware, CRLF-tolerant, optimistic-concurrency mtime checks, multi-file transactions with rollback.
- **Per-workspace memory** — durable markdown notes that travel with the project, exposed as MCP resources in Claude Desktop's sidebar.
- **Session bootstrap** — `session_start` orients the assistant in one round-trip: workspace path, language, git branch, recent commits, recently-modified files, memories, top-5 tool usage, active diagnostics.

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
- LSP capability negotiation is correct — `workspace/didChangeWatchedFiles` is registered and consumed, so symbol indexes stay live after every plumb-initiated write.

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

# Wire up Codex
plumb setup codex

# Initialise a .plumb workspace in your project root (recommended)
cd /path/to/your/project
plumb init              # creates .plumb/context.md
plumb init --discover   # auto-detects build system, entry points, test layout

# Monitor live sessions
plumb          # or: plumb status

# View tool call statistics for the current project
plumb stats
plumb stats --workspace ~/Projects/myapp

# Inspect resolved configuration (defaults + global + project + env)
plumb config show
plumb config show --workspace ~/Projects/myapp

# List active sessions
plumb sessions
plumb sessions --all    # include sessions still resolving a workspace

# Run diagnostics from the terminal
plumb diag              # workspace-wide
plumb diag path/to/file.go

# Stop the daemon (e.g. to pick up a newly-built binary)
plumb stop
```

## Workspace detection

Plumb determines the workspace root using this precedence:

1. **`.plumb/` directory** — running `plumb init` in your project root creates this marker. It takes priority over nested project markers (e.g. in `testdata/`). Recommended.
2. **MCP `roots/list`** — used if the client reports a root (Claude Desktop does this).
3. **Cwd walk** — walks up from `os.Getwd()` looking for a project marker (`go.mod`, `package.json`, `Cargo.toml`, `pyproject.toml`, etc.).

## Available tools (34)

### Session

| Tool | Description |
|---|---|
| `session_start` | Bootstrap tool — call first in every session. Returns workspace info, language, git branch, recent commits, recently-modified files, memories, top-5 tool usage, and active diagnostics in one call. Cold-start fallback chain: explicit → daemon-resolved → `roots/list` → cwd walk. Includes client-specific tool guidance: Claude Code gets the LSP tools with no native CC equivalent; Claude Desktop gets a full listing noting plumb is its only code interface. |
| `daemon_info` | Current session name and ID, daemon version, daemon start timestamp, and uptime. |
| `rename_session` | Rename the current MCP session. Names are stored uppercase, may contain only letters and `-`, and are capped at 16 characters. |

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

### Filesystem reads

| Tool | Description |
|---|---|
| `read_file` | Read a file by path or `file://` URI. Streams line ranges with `bufio.Scanner` (no whole-file load for slicing). Output header carries the file's mtime in RFC3339Nano — copy into `edit_file.expected_mtime` for optimistic-concurrency guarantees. 200 KiB cap; binary detection. |
| `read_multiple_files` | Up to 20 files; parallel (cap 8); per-file errors inline. |
| `list_directory` | Immediate children with `[FILE]`/`[DIR]` prefixes, sizes, mtimes. Glob `pattern` filter. Sort by name/size/modified. |
| `list_files` | Recursively walk a directory tree with glob filtering and depth control. |
| `find_files` | fd-style file finder — glob or regex, extension filter, type filter, depth limit. |
| `search_in_files` | ripgrep-style content search — regex, smart-case, context lines, glob filter. `include_enclosing_symbol: true` annotates each match with the deepest LSP symbol containing it. |

### Filesystem writes

All write tools share these properties: per-path locks across the daemon (concurrent writes to the same path serialise); atomic `tmpdir → rename` (with EXDEV fallback); symlink-aware (writes through links rather than replacing them); LSP-notified via `workspace/didChangeWatchedFiles`; symbol-cache invalidated by URI; rate-limited per session; post-write diagnostics appended to the response.

| Tool | Description |
|---|---|
| `write_file` | Create or overwrite a file atomically. Content staged in `os.TempDir()` and renamed into place — never a partial write. Preserves existing permissions. Sends `FileCreated`/`FileChanged` per situation. Appends fresh diagnostics if gopls/pyright republishes within 300ms. |
| `edit_file` | Apply str_replace edits to an existing file. Each `old_str` must appear exactly once (uniqueness lock — rejects ambiguous matches). CRLF/LF differences tolerated automatically. All edits applied in memory first, then a single atomic write. Optional `expected_mtime` for optimistic concurrency. Retries up to 3 times on detected concurrent writes. Response includes line-range summary and post-write diagnostics. `apply_partial: true` applies edits independently and returns per-edit pass/fail results instead of rolling back on first failure. |
| `delete_file` | Atomic delete with `FileDeleted` notification. Refuses directories. |
| `rename_file` | Atomic move/rename. Distinct from `rename_symbol` (LSP-semantic identifier rename). Two-path locks acquired in lexical order (deadlock-safe). |
| `transaction_apply` | Apply str_replace edits across multiple files atomically. Up to 50 operations. Phase 1 validates everything in memory — if any old_str is missing/ambiguous or expected_mtime mismatches, no writes happen. Phase 2 writes each file under locks; on partial failure, already-written files are rolled back to their pre-transaction content. Phase 3 fires `didChangeWatchedFiles` and invalidates the symbol cache per file. Use for cross-file refactors that must land as one unit. |

### LSP semantic edits

| Tool | Description |
|---|---|
| `rename_symbol` | Workspace-wide rename via LSP — scope- and type-aware. Detects stale LSP position index errors and returns a clear message with recovery options. |
| `replace_symbol_body` | Replace a symbol's entire declaration |
| `insert_before_symbol` | Insert text immediately before a symbol's declaration |
| `insert_after_symbol` | Insert text immediately after a symbol's declaration |
| `safe_delete_symbol` | Delete a symbol only if it has no remaining references |

`insert_before_symbol`, `replace_symbol_body`, and `safe_delete_symbol` accept an optional `include_doc_comment` flag — when true, the operation covers any contiguous comment lines directly above the declaration.

### Memory

Memories are markdown notes stored at `<workspace>/.plumb/memories/<name>.md`. They persist project-specific context — conventions, architectural decisions, gotchas — across conversations. Each memory may carry YAML frontmatter with a `description` and a `paths:` field for auto-attaching to specific files.

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
| `file_diff` | Unified diff between any two files — no git required |
| `find_replace` | Text/regex search-and-replace across files (dry-run by default). `format_after: true` runs the workspace formatter on each modified file after replacement. |

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

## Configuration

Plumb resolves configuration in four layers, lowest precedence to highest:

1. **Defaults** compiled into the binary.
2. **Global config:** `$XDG_CONFIG_HOME/plumb/config.toml` (falls back to `~/.config/plumb/config.toml`).
3. **Project config:** `<workspace>/.plumb/config.toml`. Loaded once per connection when the workspace resolves; only fields the project file sets are overridden.
4. **Environment variables.** Highest precedence — useful for one-off overrides.

### Top-level

```toml
log_level = "info"             # one of: debug, info, warn, error. Default "info".
log_file  = ""                 # path; empty = OS log dir (~/Library/Logs/plumb/daemon.log on macOS)
```

### `[edits]` — write-tool safety knobs

```toml
[edits]
strict = true                  # require read_file before edit_file (default false)
rate_limit_per_minute = 30     # 0 disables; default 120
```

| Field | Env var | Effect |
|---|---|---|
| `strict` | `PLUMB_STRICT_EDITS=1` | Every `edit_file` target must have been read in this session AND the on-disk mtime must match what was observed at read time. |
| `rate_limit_per_minute` | `PLUMB_WRITE_RATE_LIMIT=N` | Sliding-window cap on writes per session. `0` disables. |

### `[cache]` — in-memory tool-result cache

```toml
[cache]
ttl       = "5m"               # human-readable duration; default 5m
max_size  = 1000               # max entries; default 1000
```

Caches repeat queries (same tool + same args) within a session for the duration of `ttl`. Set `ttl = "0s"` to disable.

### `[walk]` — filesystem-walk safety

```toml
[walk]
refuse_home_roots = true       # default true on macOS, no-op elsewhere
```

On macOS, walking `$HOME` or one of its TCC-protected subdirectories (Desktop, Documents, Downloads, Pictures, Music, Movies, Public, iCloud Drive) triggers a consent prompt attributed to the plumb binary. With `refuse_home_roots = true`, plumb refuses walks rooted *exactly* at one of those directories so a misconfigured client (e.g. one that returns `$HOME` from `roots/list`) doesn't trip the prompt. Subpaths like `~/Documents/MyProject` are still walked normally.

### `[lsp.<lang>]` — per-language LSP server

```toml
[lsp.go]
command      = "gopls"
args         = []
root_markers = ["go.mod"]
enabled      = true

[lsp.python]
command      = "pyright-langserver"
args         = ["--stdio"]
root_markers = ["pyproject.toml", "setup.py", "pyrightconfig.json"]
enabled      = false

[lsp.rust]
command      = "rust-analyzer"
root_markers = ["Cargo.toml"]
enabled      = true
```

Each entry overrides the compiled default for that language. `enabled = false` skips spawning the adapter even if its root marker is found. `env` may be set to a string→string table to inject environment variables for the spawned server.

Run `plumb config show` (optionally with `--workspace <dir>`) to see the resolved config with provenance — each field's value plus which layer supplied it.

## Project context

Running `plumb init` creates `.plumb/context.md` — a markdown file you can fill with project-specific context: what the project does, architecture decisions, conventions, and known gotchas. The `session_start` tool loads the first 200 lines of this file automatically. Claude Desktop also shows it in the resources sidebar as "Project context".

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

The `plumb` command (alias: `plumb status`) opens a live Bubble Tea TUI. The Dashboard section shows daemon health, activity history, tokens saved, top tools, alerts, and project-scoped stats as compact widgets. Other sections cover sessions, workspace memory, live daemon logs, and settings.

Use `/` to open the section selector, `ctrl+h` for help, and `ctrl+q` to quit. In the Sessions section, `↑↓`/`j k` moves through sessions and calls, `tab` cycles focus, and selecting a failed call expands its error details.

## File-write safety model

Every write through plumb (`write_file`, `edit_file`, `delete_file`, `rename_file`, `transaction_apply`) goes through the same layered safety:

1. **Per-path lock.** A process-global lock keyed by `filepath.Clean(path)` serialises all concurrent writes to the same file from any session. Two parallel agents cannot interleave reads and writes on the same file.

2. **Atomic rename.** Content is staged in `os.TempDir()` and renamed into place. `os.Rename` is a single POSIX syscall — the target is never partially written. EXDEV cross-device rename falls back to a `.plumb.tmp` sibling automatically. No backup files left in the project tree.

3. **Symlink-aware.** If the target is a symlink, plumb resolves it before writing — the write goes through the link, not replacing it with a regular file.

4. **Uniqueness lock + CRLF tolerance** (`edit_file`). Each `old_str` must match exactly once. Line endings are normalised against the file before matching, so an LF `old_str` matches a CRLF file. If the file changed between read and edit and `old_str` becomes ambiguous, the edit is rejected — no silent corruption.

5. **Optimistic concurrency** (`edit_file`, `transaction_apply`). Pass `expected_mtime` from a prior `read_file`; the operation is rejected if the file's current mtime differs. Pre-rename mtime re-check inside the write loop closes the inner TOCTOU window.

6. **Strict mode** (opt-in via `[edits].strict = true`). Every edit requires a prior `read_file` in the same MCP session AND a matching mtime. Tracked per-session via `ReadTracker` — no cross-session leakage.

7. **Concurrent-write retry** (`edit_file`). After the rename, the file's mtime is re-checked. A significant jump triggers a re-read and retry (up to 3 attempts).

8. **LSP notification + cache invalidation.** Every successful write fires `workspace/didChangeWatchedFiles` to the language server and evicts cache entries by URI. Diagnostics for the touched file are polled for up to 300ms and any fresh ones are appended to the response.

## Statistics

Tool-call statistics are stored in one global SQLite database under plumb's user data directory (stamped in `PRAGMA user_version`). Each row records its workspace and session identity, matching the single global daemon model. The TUI and `plumb stats` read concurrently with the daemon writing, using WAL journal mode.

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

A version mismatch between a running daemon and a freshly-built `plumb serve` triggers a stderr warning on next connect: `plumb: warning: connected daemon is 0.4.1 but this binary is 0.5.4 — run \`plumb stop\` to refresh.` Always restart the daemon after rebuilding.

## Build from source

```sh
make build      # produces ./plumb, version stamped from git tag or VERSION file
make test       # full test suite
make test-race  # with race detector
make lint       # golangci-lint
```

Requires Go 1.22+. `gopls` must be on `$PATH` for integration tests.

## Versioning

The version is injected at build time. To bump during development, edit the `VERSION` file:

```sh
echo "0.5.5" > VERSION
make build
```

For a release, tag the commit — the tag takes precedence:

```sh
git tag v0.5.4
make build
```

## Contributing

See [`AGENTS.md`](AGENTS.md) for architecture details, code style rules, and the checklist for adding new tools or LSP adapters. AGENTS.md is the canonical brief for AI agents working in the codebase and is kept current with every release.
