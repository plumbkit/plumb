# Plumb — Agent Instructions

> Source of truth: edit `AGENTS.md` only.
>
> `CLAUDE.md`, `GEMINI.md`, and `CHATGPT.md` are local symlinks to this file for client compatibility. Do not replace, unlink, rewrite, or edit those symlink paths directly. If an instruction change is needed, update `AGENTS.md`; the linked files will reflect it automatically.
>
> These agent-context files are intentionally ignored by git in this workspace.

This file is the canonical brief for AI agents working in the plumb codebase. Keep it accurate; it ages fast.

Current version: **0.6.3** (see `VERSION` and `CHANGELOG.md`).

## Project purpose

Plumb is an MCP (Model Context Protocol) server that exposes LSP (Language Server Protocol) capabilities to LLMs. It is also a complete filesystem toolkit: read, write, edit, delete, rename, transaction. It is designed so an LLM (especially Claude Desktop, Claude Code, Codex, or Gemini CLI, which may have limited filesystem access of their own) can navigate, understand, and modify a codebase entirely through structured semantic tools — no raw-file dumping, no shell.

The architectural commitments are:

1. **LSP-correct semantics.** When plumb writes a file, the language server learns via `workspace/didChangeWatchedFiles` (not the open-document lifecycle). Capabilities are negotiated. Server-initiated `client/registerCapability` requests are answered.
2. **Concurrency-safe writes.** Per-path locks across all write tools; atomic `tmpdir → rename`; symlink-aware; CRLF-tolerant; optimistic-concurrency via mtime; bounded retries.
3. **Per-session isolation.** The daemon hosts multiple MCP connections. Read-tracking, rate limits, and caches are scoped per-connection — never process-global.
4. **Configurable safety.** Strict mode, rate limits, and other safety knobs are configurable at three layers: global, per-project (`<workspace>/.plumb/config.toml`), and environment.

## Architecture

Strict layered architecture — lower layers must never import higher ones:

```
Transport (MCP/LSP) → Domain (symbols, edits, capabilities)
                    → Application (composite tools, caching, rate-limiting)
                    → Presentation (TUI, CLI)
```

Key packages:

| Package | Role |
|---|---|
| `internal/mcp/` | MCP server, tool registry, prompts, stdio transport |
| `internal/lsp/` | LSPClient interface, JSON-RPC 2.0 (with server-request support), process supervisor |
| `internal/lsp/adapters/gopls/` | Validated Go adapter |
| `internal/lsp/adapters/pyright/` | Real Python adapter, unit-tested only |
| `internal/tools/` | MCP tool implementations; `WriteDeps` bundles write-tool dependencies |
| `internal/cache/` | Session-scoped symbol cache + LSP-driven invalidator |
| `internal/config/` | TOML config, XDG paths, project-config merging |
| `internal/session/` | Session-file registration + client identity tracking |
| `internal/stats/` | Global SQLite tool-call statistics, row-scoped by workspace and session (WAL, per-tool summary, P95, `user_version` 5) |
| `internal/memory/` | Per-workspace markdown memory store; exposed as MCP resources |
| `internal/tui/` | Bubble Tea v2 TUI — live session + stats dashboard, recent-edits panel |
| `internal/cli/` | Cobra subcommands; daemon, proxy, pool, workspace detection, `config show` |

## Daemon architecture

`plumb serve` is a thin stdio proxy. The real server is `plumb daemon`:

```
Claude Desktop / Claude Code / Codex / Gemini CLI
  └── plumb serve  (per conversation — dials Unix socket, proxies bytes)
        └── ~/Library/Caches/plumb/plumb.sock  (macOS; os.UserCacheDir())
              └── plumb daemon  (one process, shared across all conversations)
                    ├── workspacePool  (one gopls per workspace root)
                    │     ├── poolEntry{proxy, inv, cache} for /projects/foo
                    │     └── poolEntry{proxy, inv, cache} for /projects/bar
                    └── handleConn()  (per-connection MCP session)
                          ├── readTracker        (per-connection strict-mode state)
                          ├── writeLimiter       (per-connection limit + shared client budget parent)
                          ├── editsCfg + strictFn (resolved per-project [edits])
                          └── sessionCache       (per-connection symbol cache)
```

On daemon start the binary writes:

| File | Purpose |
|---|---|
| `~/Library/Caches/plumb/plumb.sock` | Unix socket — MCP wire |
| `~/Library/Caches/plumb/plumb.pid` | PID for `plumb stop` |
| `~/Library/Caches/plumb/plumb.version` | Build version; `plumb serve` warns on mismatch |
| `~/Library/Caches/plumb/plumb.spawn.lock` | `flock`'d briefly by `plumb serve` to serialise daemon spawn decisions (see "Singleton enforcement" below) |
| `~/Library/Caches/plumb/plumb.daemon.lock` | `flock`'d by `plumb daemon` for its lifetime; a second daemon sees `EWOULDBLOCK` and exits |
| `~/Library/Caches/plumb/plumb.ctrl.sock` | Admin Unix socket; accepts line-based `set-level <level>` commands from `plumb log-level` |
| `~/Library/Caches/plumb/daemon.log` | All daemon logs |

Stats live in one persistent global database at `config.DataDir()/stats.db`
(for example `~/.local/share/plumb/stats.db` on Linux). This follows the
single-daemon architecture: every row must carry both `workspace` and
`session_id`, and project/session views filter on those row attributes.

### Singleton enforcement

The "one daemon, shared across all conversations" invariant is enforced with two advisory locks (`internal/cli/lock.go`):

- **`plumb.spawn.lock`** — `plumb serve` takes an exclusive `flock` *before* deciding to spawn a daemon, then re-dials inside the critical section. Without this, two serves racing from a cold start each observe "no daemon" and each call `startDaemonProcess`.
- **`plumb.daemon.lock`** — `plumb daemon` takes an exclusive non-blocking `flock` at the top of `runDaemon`. If held, the second daemon exits before touching the socket. Defence in depth against manual `plumb daemon` invocations and any future bug in the spawn-lock path.

Both locks live on the open file description, so the kernel releases them on process exit (clean SIGTERM or crash). The lock files themselves persist as zero-byte rendezvous points and are never cleaned up — they are not state.

## Configuration layers

Resolved configuration is built in four layers; each can override the prior. Use `plumb config show` to print the resolved config with provenance.

1. **Compiled defaults** in `internal/config/config.go` `defaults`.
2. **Global config** at `$XDG_CONFIG_HOME/plumb/config.toml` (falls back to `~/.config/plumb/config.toml`). Loaded once at daemon start.
3. **Project config** at `<workspace>/.plumb/config.toml`. Loaded once per connection when the workspace resolves; merged onto global. Only fields the project file sets are overridden; the rest inherit.
4. **Environment variables** — highest precedence. Useful for emergency overrides without editing files.

### `[edits]` section — write-tool safety

```toml
[edits]
strict = true                  # require read_file before edit_file (default false)
rate_limit_per_minute = 30     # 0 disables; default 120
```

| Field | Env var | Effect |
|---|---|---|
| `strict` | `PLUMB_STRICT_EDITS` | `true`/`1`/`yes` enables strict mode. Every `edit_file` target must have been read in this session AND the mtime must match. Closes the "edit without read" footgun. Per-session via `ReadTracker`. |
| `rate_limit_per_minute` | `PLUMB_WRITE_RATE_LIMIT` | Sliding-window cap on writes per session. `0` disables. Protects against runaway-loop scenarios. |

## Client setup commands

`plumb setup` registers the current `plumb` binary as a stdio MCP server for supported clients:

| Client | Command | Config target |
|---|---|---|
| Claude Desktop | `plumb setup claude-desktop` | Platform-specific Claude Desktop JSON config |
| Claude Code, user scope | `plumb setup claude-code` | `~/.claude.json` |
| Claude Code, project scope | `plumb setup claude-code --project` | `.mcp.json` in the current directory |
| Codex | `plumb setup codex` | `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when `CODEX_HOME` is unset |
| Gemini CLI | `plumb setup gemini` | `~/.gemini/settings.json` |

Setup helpers must preserve existing MCP servers, back up existing config before modifying it, and resolve config locations via OS/user-home helpers or client environment variables — no hardcoded absolute user paths.

## Workspace detection

`workspacePool.Detect(dir)` walks up from `dir`. At each directory the priority order is:

1. **`.plumb/` marker.** The user explicitly declared this directory a plumb workspace. If an LSP language is also detectable (here or in an ancestor), returns `(dir, language)`. Otherwise returns `(dir, "none")` — filesystem tools, stats, and per-project config still apply; LSP tools fail until a language attaches.
2. **A configured language's root marker** (`go.mod`, `pyproject.toml`, `setup.py`, …). Returns `(dir, language)`.

If neither is found, walk to the parent. Returning an error only happens after we walk past the filesystem root.

`LanguageNone` (`"none"`) is the sentinel for case 1's second branch. The session is fully attached (Folder set, stats DB opened, project config loaded), the LSP-bearing routing proxies just have no primary. This was added in 0.5.26 to fix the "TUI stuck on resolving…" symptom in JS/TS and other non-Go/non-Python projects.

When the daemon starts without a workspace (Claude Desktop launches the daemon from `$HOME`), `session_start` resolves it via this chain:

1. Explicit `workspace` argument to the tool call
2. Daemon's already-resolved workspace
3. `roots/list` query to the MCP client
4. Walk up from `os.Getwd()` for a project marker

Run `plumb init` in any project root to create a `.plumb/` marker directory (also holds `context.md` and the project's `stats.db`). For non-Go/non-Python projects this is now sufficient to get the full daemon experience — no language server, but everything else.

## Adapter validation status

| Adapter | Status |
|---|---|
| `gopls` | **Validated** — unit-tested with mock transport; integration-tested against real gopls binary; `client/registerCapability` answered, `workspace/didChangeWatchedFiles` confirmed |
| `pyright` | **Validated** — unit-tested with mock transport; integration-tested against real pyright-langserver binary; `client/registerCapability` answered, `workspace/didChangeWatchedFiles` confirmed |

## How to add an LSP adapter

Pyright is the worked example.

1. Create `internal/lsp/adapters/<name>/`.
2. Add `doc.go` with a package doc comment stating validation status.
3. Implement every method of the `LSPClient` interface in `internal/lsp/client.go`. The interface includes `DidChangeWatchedFiles` — the LSP-correct primitive for external file changes. There are no per-adapter extension methods.
4. Register a request handler via `conn.SetRequestHandler(a.handleServerRequest)` that responds OK to `client/registerCapability` / `client/unregisterCapability`. Without this, the server may stall waiting for our response.
5. Implement initialisation: capability negotiation (use `protocol.DefaultClientCapabilities()` as the base — it declares `workspace.didChangeWatchedFiles.dynamicRegistration: true`), workspace model, init options.
6. Write unit tests using `internal/lsp/jsonrpc/mock.go`. Cover `DidChangeWatchedFiles` wire format (gopls and pyright both have explicit tests).
7. Document in `docs/adding-an-lsp.md`.

## How to add an MCP tool

1. Create `internal/tools/<name>.go`.
2. Implement the `Tool` interface from `internal/mcp/tools.go` (`Name`, `Description`, `InputSchema`, `Execute`).
3. For write/edit tools, take a single `WriteDeps` parameter — do not grow the constructor signature with new positional params. Add a field to `WriteDeps` if you need a new cross-cutting concern.
4. Register the tool in `handleConn` in `internal/cli/daemon.go`. Write tools use the shared `writeDeps` instance.
5. Write unit tests in `internal/tools/<name>_test.go`. Use `WriteDeps{}` for nil-safe test setups.
6. Document inputs, outputs, and required LSP capabilities in `docs/mcp-tools.md`.
7. Update this file's tool table.

## Available tools (34)

**Bootstrap**

| Tool | File | LSP method | Notes |
|---|---|---|---|
| `session_start` | `session_start.go` | — | Call FIRST in every session. Returns workspace, language, branch, recent commits, recently-modified files, memories, top-5 tool stats, active diagnostics. Cold-start chain: explicit → daemon-resolved → `roots/list` → cwd walk. |

**LSP queries**

| Tool | File | LSP method | Notes |
|---|---|---|---|
| `find_symbol` | `find_symbol.go` | `textDocument/documentSymbol` | Single-file; `uri` required. Cached by URI. |
| `workspace_symbols` | `workspace_symbols.go` | `workspace/symbol` | Workspace-wide. Stdlib/deps filtered. Cached. |
| `get_definition` | `get_definition.go` | `textDocument/definition` | Cached. |
| `explain_symbol` | `explain_symbol.go` | `textDocument/hover` | Cached. |
| `list_symbols` | `list_symbols.go` | `textDocument/documentSymbol` | Full hierarchy with line ranges. |
| `find_references` | `find_references.go` | `textDocument/references` | Includes source-line text. |
| `call_hierarchy` | `call_hierarchy.go` | `textDocument/prepareCallHierarchy` | Incoming + outgoing. |
| `type_hierarchy` | `type_hierarchy.go` | `textDocument/prepareTypeHierarchy` | Supertypes + subtypes. |
| `diagnostics` | `diagnostics.go` | notification subscriber | Errors, warnings, hints. |

**LSP edits**

| Tool | File | Notes |
|---|---|---|
| `rename_symbol` | `rename_symbol.go` | Atomic workspace-edit application (`textDocument/rename`). |
| `replace_symbol_body` | `symbol_edits.go` | `include_doc_comment` optional. |
| `insert_before_symbol` | `symbol_edits.go` | `include_doc_comment` optional. |
| `insert_after_symbol` | `symbol_edits.go` | No doc-comment ambiguity. |
| `safe_delete_symbol` | `symbol_edits.go` | Refuses if external refs exist. `include_doc_comment` optional. |

**Filesystem reads**

| Tool | File | Notes |
|---|---|---|
| `read_file` | `read_file.go` | Absolute path or `file://`. Line ranges stream via `bufio.Scanner`. 200 KiB cap. Binary detection. Records mtime in `ReadTracker` for strict mode. Output header: `# plumb-read mtime=<RFC3339Nano>` — copy into `edit_file.expected_mtime`. |
| `read_multiple_files` | `read_multiple_files.go` | Up to 20 paths. Parallel (cap 8). Per-file errors inline. |
| `list_directory` | `list_directory.go` | Immediate children, `[FILE]`/`[DIR]` prefixes, sizes, mtimes. Glob `pattern`. Sort by name/size/modified. |
| `list_files` | `list_files.go` | Recursive; glob filter; depth control; respects `.gitignore`. |
| `find_files` | `find_files.go` | Glob/regex finder; honours `.gitignore`. |
| `search_in_files` | `search_in_files.go` | ripgrep-style; smart-case; honours `.gitignore`; `exclude` glob patterns prune directories and files. |

**Filesystem writes** (all take `WriteDeps`; all hold per-path locks; all check git dirty state; all notify LSP via `didChangeWatchedFiles`; all invalidate the symbol cache; all consume one rate-limit slot)

| Tool | File | Notes |
|---|---|---|
| `write_file` | `write_file.go` | Atomic (tmpdir + rename + EXDEV-fallback). Symlink-aware. Permissions preserved. Post-write diagnostics appended to response. `FileCreated`/`FileChanged` notification. `dirty_ok` param (default false) — refused if target has uncommitted changes. |
| `edit_file` | `edit_file.go` | str_replace; uniqueness lock; CRLF tolerance; in-memory application (all-or-nothing); pre-rename mtime check; post-rename concurrent-write retry (max 3); optional `expected_mtime` opt-in concurrency check; strict-mode requires-read check; line-range summary + post-write diagnostics in response. `dirty_ok` param. |
| `delete_file` | `delete_file.go` | Refuses directories. `FileDeleted` notification. `dirty_ok` param. |
| `rename_file` | `rename_file.go` | Atomic. Two-path locks (lexical order, deadlock-safe). `FileDeleted` + `FileCreated` notifications. Refuses to overwrite without `overwrite=true`. `dirty_ok` param (checks source). |
| `transaction_apply` | `transaction.go` | Multi-file atomic edits. Up to 50 ops. Phase 1 validates everything in memory; phase 2 writes under locks; phase 3 rolls back successful writes on partial failure. Each op consumes one rate-limit slot. Use for cross-file refactors. `dirty_ok` param — batched check per directory, all dirty files reported at once. |

**Other**

| Tool | File | Notes |
|---|---|---|
| `find_replace` | `find_replace.go` | Text/regex find-and-replace across files; dry-run by default. |
| `git` | `git.go` | Read-only subcommands (status, log, diff, show, blame, branch, tag, shortlog, stash). |
| `file_diff` | `file_diff.go` | System `diff -U`. |
| `version` | `version.go` | Server version, Go runtime, OS/arch. |
| `daemon_info` | `daemon_info.go` | Current session name, session ID, daemon version, start time, uptime. |
| `rename_session` | `rename_session.go` | Rename the current MCP session. Letters and `-` only; stored uppercase; max 16 characters. |

**Memory** — per-workspace markdown notes at `<workspace>/.plumb/memories/`. Also exposed as MCP resources.

| Tool | Notes |
|---|---|
| `list_memories` | All memory names + descriptions. |
| `read_memory` | One memory by name. |
| `write_memory` | Create or overwrite a memory. |
| `delete_memory` | Remove a memory. |
| `search_memories` | Pattern search across memory bodies. |
| `relevant_memories` | Filename-based relevance from a path. |

## TUI conventions (Bubble Tea v2)

- Import paths: `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`.
- `Model` is exported; `NewModel(logPath string)` is the constructor; `Run(logPath string)` is the entry point.
- `View()` returns `tea.View`. Use `tea.NewView(content)` and set `v.AltScreen = true`.
- Key handling: `tea.KeyPressMsg`, match via `msg.String()`.
- Section menu opened with `/`; sections are `Dashboard`, `Sessions`, `Memory`, `Logs`, `Settings` (indices 0–4).
- Sessions section (index 1, default): two-panel layout — **Sessions** list (left) and **Details** / **Tool Statistics** / **Recent Tools** (right).
- Logs section (index 3): full-width live log viewer (`internal/tui/log_view.go`). Tails `daemon.log` from `daemonLogPath()` (passed by CLI at startup). Filters by substring (`logFilter`); follows newest entry when `logFollow = true` (default). Press `G` to re-engage follow, `esc` to clear filter, type to filter.
- `panelFocus` constants: `focusSessions`, `focusToolStats`, `focusStats`, `focusDetails`, `focusLogs` — only the first four are used in the Sessions section; `focusLogs` is reserved for future use within the Logs section.
- Overlays: dim the background with `dimLines()`, splice the box via `spliceOverlay()`.

## Code style rules

- **Australian English** in all prose: docs, comments, log messages, error strings. Use -ise/-isation (initialise, serialise, synchronise, organise, recognise). Use behaviour, colour, honour, favour. **Exception:** identifiers defined by external specifications keep their canonical spelling — LSP method names (`initialize`, `publishDiagnostics`), MCP protocol fields, and Go standard library names are never changed.
- **`gofumpt`** on save. `golangci-lint` v2.12.2 before every commit; CI enforces.
- **`log/slog`** exclusively. Never `log` package or `fmt.Println` for logging.
- **Errors wrap context:** `fmt.Errorf("loading config: %w", err)`.
- **Context everywhere:** every blocking/I/O operation takes `context.Context` first.
- **Concurrency contract** stated in doc comments on every type.
- **No `init()` doing real work.** Wire dependencies in constructors.
- **No globals** except package-level style vars in `internal/tui/styles.go` (rebuilt, not stateful) and the `pathLocks` map in `internal/tools/file_write_helpers.go` (process-global by design).
- **Max ~400 lines per file.** Split if it grows.
- **Comments only when the WHY is non-obvious.** No what-comments.

## Testing requirements

- Tests live next to the code (`_test.go` files in the same package).
- Table-driven where the shape fits.
- `internal/lsp/`, `internal/cache/`, `internal/tools/` require meaningful coverage.
- For write tools, `WriteDeps{}` is the zero-value test setup.
- Per-session isolation tests (e.g. `TestEditFile_StrictMode_TrackerIsolation`) belong in the package they test.
- Do not chase TUI coverage.
- Integration tests that require external binaries (gopls, pyright) must be gated with `//go:build integration`.

## Versioning

Version is injected at build time via `-ldflags`:

```
-X github.com/golimpio/plumb/internal/cli.Version=<version>
```

`internal/cli.Version` defaults to `"dev"`. The Makefile resolves it from:

1. Exact git tag on the current commit (release builds)
2. Contents of the `VERSION` file (development)
3. Short git commit hash (fallback)

To bump during development, edit the `VERSION` file. Do not create a git tag for every iteration.

The daemon writes its build version to `~/Library/Caches/plumb/plumb.version` on start. `plumb serve` reads it and warns on mismatch ("run `plumb stop` to refresh"). If you've just rebuilt and a different daemon version is running, **restart the daemon** — no amount of new code will activate against the old process.

## Commit conventions

```
<type>(<scope>): <short summary>

[optional body: why, not what]
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`.

For multi-step work, prefer one commit per discrete change with a CHANGELOG.md entry. Bisectable history > squashed PRs.

## Build commands

```sh
make build       # compile to ./plumb, version stamped from git/VERSION
make test        # go test ./...
make test-race   # go test -race ./...
make lint        # golangci-lint run
make tidy        # go mod tidy
make clean       # remove ./plumb
make install-hooks  # install pre-commit hook
```

## Known limitations and pending work

Outstanding items, footguns, and "subtle things to be aware of" live in [`docs/todo.md`](docs/todo.md). Always check it before adding a feature that touches concurrency, the rate limiter, the read tracker, or the stats schema — there are known limitations in each of those that any new work needs to either respect or address.

When you complete a TODO item, delete the section from `docs/todo.md` *in the same commit* that adds the `CHANGELOG.md` entry.

## Quick reference for agents

You are likely an AI agent reading this through plumb. Treat the following as the most common patterns:

- **First call:** `session_start({})`. Returns the orientation packet.
- **Read before edit:** call `read_file`, copy its `mtime` header value, pass it as `expected_mtime` to `edit_file`. Mandatory under strict mode; recommended always.
- **One-shot file create:** `write_file({path, content})`.
- **Targeted edit:** `edit_file({path, edits: [{old_str, new_str}], expected_mtime})`. `old_str` must appear exactly once. CRLF differences are tolerated automatically.
- **Cross-file refactor:** `transaction_apply({operations: [{path, edits, expected_mtime}]})`. All-or-nothing.
- **Delete:** `delete_file({path})`. Refuses directories.
- **Move/rename:** `rename_file({from, to})`. Distinct from `rename_symbol` (LSP-semantic identifier rename).
- **Discover what changed:** `git({subcommand: "status"})` or `git({subcommand: "log", args: ["-10", "--oneline"]})`.
- **See your own activity:** `plumb` TUI's right panel shows "Recent Edits" for the selected session.
- **Throttled?** You hit the rate limit (default 120/min). Wait or set `PLUMB_WRITE_RATE_LIMIT=0`.
- **Rejected for "has not been read"?** Strict mode is on. Call `read_file` first.
- **Rejected for "uncommitted changes"?** The target file is dirty in git. Review and commit the changes, or pass `dirty_ok: true` to overwrite anyway.
- **Too much log noise from the daemon?** `plumb log-level warn` raises the floor instantly, no restart required. `plumb log-level reset` restores the config-file default.

When in doubt about the resolved config, `plumb config show --workspace .` from the project directory.
