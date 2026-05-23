# Plumb — Agent Instructions

> Source of truth: edit `AGENTS.md` only.
>
> `CLAUDE.md`, `GEMINI.md`, and `CHATGPT.md` are repository symlinks to this file for client compatibility. Do not replace, unlink, rewrite, or edit those symlink paths directly. If an instruction change is needed, update `AGENTS.md`; the linked files will reflect it automatically.
>
> These agent-context files are tracked in git to ensure a consistent, high-quality experience for AI assistants.

This file is the canonical brief for AI agents working in the plumb codebase. Keep it accurate; it ages fast.

> **CRITICAL — tool priority:** Always use plumb MCP tools for all tasks when plumb is present and the required capability is available through plumb. Do not fall back to native tools (Read, Edit, Bash, shell commands, etc.) for file reads, writes, edits, searches, symbol lookups, or git queries when the equivalent plumb tool exists. Plumb tools are LSP-aware, concurrency-safe, and session-tracked; native tools bypass all of that. The only exceptions are tasks plumb explicitly does not cover (e.g. running tests, compiling, interacting with external services).

Current version: see the `VERSION` file and `CHANGELOG.md` (not pinned in this brief, to avoid drift).

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
                    → Intelligence (topology)
                    → Application (composite tools, caching, rate-limiting)
                    → Presentation (TUI, CLI)
```

Key packages:

| Package | Role |
|---|---|
| `internal/mcp/` | MCP server, tool registry, prompts, stdio transport |
| `internal/lsp/` | LSPClient interface, JSON-RPC 2.0 (with server-request support), process supervisor |
| `internal/lsp/adapters/gopls/` | Validated Go adapter |
| `internal/lsp/adapters/pyright/` | Validated Python adapter |
| `internal/lsp/adapters/jdtls/` | Experimental Java adapter (jdtls); enabled via `[lsp.java] enabled = true` |
| `internal/topology/` | SQLite/FTS5 semantic graph; background indexer; Go AST, Python regex, and TypeScript/JS regex extractors; search + BFS explore/impact/affected/routes |
| `internal/topology/extractors/golang/` | Go extractor using `go/parser`+`go/ast`; no CGo |
| `internal/topology/extractors/python/` | Python extractor using line-by-line regex scan; no CGo |
| `internal/topology/extractors/typescript/` | TypeScript/JS extractor using regex scan; no CGo |
| `internal/langsupport/` | Per-language capability registry — structural engine (native-AST / tree-sitter / regex / none) and LSP adapter, keyed by language. Single source of truth consulted by `buildExtractors` (`internal/cli/topology_pool.go`); the seam for moving a language onto tree-sitter. Pure data, no topology/LSP imports. |
| `internal/tools/` | MCP tool implementations; `WriteDeps` bundles write-tool dependencies; `txlog` subpackage is the transaction rollback WAL |
| `internal/quality/` | Offline post-write code analysers (golangci-lint, …) against changed files; findings appended to write responses |
| `internal/cache/` | Session-scoped symbol cache + LSP-driven invalidator |
| `internal/config/` | TOML config, XDG paths, project-config merging |
| `internal/session/` | Session-file registration + client identity tracking |
| `internal/stats/` | Global SQLite tool-call statistics, row-scoped by workspace and session (WAL, per-tool summary, P95, client-aware, `user_version` 7) |
| `internal/memory/` | Per-workspace markdown memory store; exposed as MCP resources |
| `internal/tui/` | Bubble Tea v2 TUI — live session + stats dashboard, recent-edits panel |
| `internal/render/` | Shared, pure CLI/TUI presentation helpers (stdlib + rendering libs only) |
| `internal/fsguard/` | Guards filesystem walks against macOS TCC false-positive prompts on protected dirs |
| `internal/monitor/` | Process resource-usage snapshots (CPU %, memory) feeding the TUI daemon metrics |
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
                          ├── gitCfg + gitPolicyFn (resolved per-project [git])
                          └── sessionCache       (per-connection symbol cache)
```

On daemon start the binary writes the following files under the system cache directory (`os.UserCacheDir()/plumb`, e.g., `~/Library/Caches/plumb/` on macOS or `~/.cache/plumb/` on Linux):

| File | Purpose |
|---|---|
| `plumb.sock` | Unix socket — MCP wire |
| `plumb.pid` | PID for `plumb stop` |
| `plumb.version` | Build version; `plumb serve` warns on mismatch |
| `plumb.spawn.lock` | `flock`'d briefly by `plumb serve` to serialise daemon spawn decisions (see "Singleton enforcement" below) |
| `plumb.daemon.lock` | `flock`'d by `plumb daemon` for its lifetime; a second daemon sees `EWOULDBLOCK` and exits |
| `plumb.ctrl.sock` | Admin Unix socket; accepts line-based `set-level <level>` commands from `plumb log-level` |
| `daemon.log` | All daemon logs |

Stats live in one persistent global database at `config.DataDir()/stats.db`
(for example `~/.local/share/plumb/stats.db` on Linux, `~/Library/Application Support/plumb/stats.db` on macOS). This follows the
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
show_write_diff = true         # append unified diff to edit_file/write_file responses (default true)
```

| Field | Env var | Effect |
|---|---|---|
| `strict` | `PLUMB_STRICT_EDITS` | `true`/`1`/`yes` enables strict mode. Every `edit_file` target must have been read in this session AND the mtime must match. Closes the "edit without read" footgun. Per-session via `ReadTracker`. |
| `rate_limit_per_minute` | `PLUMB_WRITE_RATE_LIMIT` | Sliding-window cap on writes per session. `0` disables. Protects against runaway-loop scenarios. |
| `show_write_diff` | `PLUMB_SHOW_WRITE_DIFF` | When `true` (default), `edit_file` and `write_file` append a unified diff of the change to their response. Set to `false` to suppress diffs in high-volume write workflows. |

### `[workspace]` section — root detection fallback

```toml
[workspace]
auto_attach = false         # opt-in synthetic root when no project marker exists
auto_attach_persist = false # create .plumb/ at the synthetic root on first attach
```

| Field | Env var | Effect |
|---|---|---|
| `auto_attach` | `PLUMB_AUTO_ATTACH` | When `true`, `OnBeforeTool` falls back to `SynthesiseRoot` (nearest `.git/` or seed dir) if `Detect` fails. Stats and TUI work; LSP unavailable. |
| `auto_attach_persist` | `PLUMB_AUTO_ATTACH_PERSIST` | When `true`, the daemon creates `<root>/.plumb/` on first attach so the next session resolves via the normal marker path. Implies `auto_attach`. |

### `[git]` section — tiered git tool gating

```toml
[git]
allow_writes = true                    # add, commit, switch, branch/tag create, stash (default true)
allow_destructive = false              # reset, clean, checkout, restore, rebase, … (default false)
allow_push = false                     # push, fetch, pull (default false)
protected_branches = ["main", "master"] # never force-pushable
```

| Field | Env var | Effect |
|---|---|---|
| `allow_writes` | `PLUMB_GIT_ALLOW_WRITES` | Gates the safe-write tier. `0`/`false`/`no` disables it; default on. |
| `allow_destructive` | `PLUMB_GIT_ALLOW_DESTRUCTIVE` | Gates the destructive tier. Each destructive call also requires `confirm:true`. Default off. |
| `allow_push` | `PLUMB_GIT_ALLOW_PUSH` | Gates the network tier (`push`/`fetch`/`pull`). Each network call also requires `confirm:true`. Default off. |
| `protected_branches` | — | Branch names that may never be force-pushed, even with `allow_push` + `confirm`. Default `["main", "master"]`. |

All `[git]` fields follow the standard layering: compiled defaults → global config → `<workspace>/.plumb/config.toml` → environment. A project file that sets only one field (e.g. `allow_destructive = true`) inherits the rest, and changes are hot-reloaded by the config watcher. The resolved values appear under the `git` section of `plumb config show` with per-field provenance. The daemon resolves the per-connection policy live via `gitPolicyFn` (`internal/cli/conn.go` `gitConfig()`), so a project-config edit takes effect on the next `git` call without reconnecting.

The `git` tool always runs the requested subcommand as the first argv element, so global flags supplied in `args` (e.g. `-c`, `--exec-path`) cannot reconfigure git. A small denylist also rejects `-c`, `-C`, `--git-dir`, `--work-tree`, `--namespace`, `--upload-pack`, `--receive-pack`, and `--exec-path` outright. There is no shell.

Tier classification is *safe-biased*: ambiguous subcommands (`checkout`, `switch`, `restore`, `branch`, `tag`, `stash`) inspect their args and round **up** to the higher tier when uncertain — e.g. `checkout -b` is a write but any other `checkout` is destructive, `restore --staged` is a write but `restore --worktree` is destructive, and bare `git stash` is a write. `add` and `commit` are typed, not pass-through: `commit` only ever runs `commit -m <message>` (so `--amend`, `--no-verify`, `-F`, and the editor are unreachable) and `add` only runs `add -- <files>`; pre-commit hooks always run. Every non-read call consumes one write-rate-limit slot, and output is capped (200 lines for `log`/`blame`, 100 KiB overall) with `add`/`commit` returning a concise summary rather than raw git output. Classification lives in `classifyGit` (`internal/tools/git.go`).

### `[ui]` section — TUI theme

```toml
[ui]
theme = "nordico"   # built-in theme name; default "nordico"
```

| Field | Effect |
|---|---|
| `theme` | Selects the active TUI colour theme. Must match a key in `tui.AvailableThemes`. Written live by the theme-picker popup (each cursor move) reachable from the Settings section. Read at `plumb` startup by `internal/cli/root.go` before `tui.Run()`. |

Built-in themes: `nordico` (dark, Nord-inspired), `darcula` (dark, JetBrains), `dracula` (dark, dracula.github.io), `gruvbox` (dark, earthy), `github-light` (light, GitHub), `solarized-light` (light, Solarized classic). Each theme maps to a chroma style used by `plumb config show`. `config.Save(apply func(*Config))` in `internal/config/config.go` performs a full-file rewrite (load → mutate → re-encode) and backs every TUI settings write; `SaveTheme(name)` is a thin wrapper over it. User-added TOML comments are lost on first save — known v1 limitation. Only the global config file is written; project config does not support `[ui]`.

### `[lsp_query]` section — LSP tool-call timeout

```toml
[lsp_query]
timeout = "30s"   # cap on a single LSP tool call; "0s" disables. Default 30s.
```

| Field | Env var | Effect |
|---|---|---|
| `timeout` | `PLUMB_LSP_QUERY_TIMEOUT` | Bounds every LSP-backed tool call (queries and symbol edits). If the language server has not answered within this window the tool fails fast with a clear message instead of blocking until the MCP client's own timeout fires. `0s` disables the cap. |

Note this is a top-level section (`[lsp_query]`), distinct from the per-language `[lsp.<lang>]` server tables — `LSP` in `internal/config/config.go` is a `map[string]LSPConfig`, so the scalar lives in its own section to avoid colliding with a language key. The deadline is applied at the tool layer (`withLSPDeadline` / `lspTimeoutErr` in `internal/tools/lsp_deadline.go`) and is a no-op when the caller's context already carries a deadline, so the cold-start `initialize`/`initialized` handshake — which runs on the adapter before the routing proxy is live — is never shortened. Independently, `jsonrpc.Conn.Call` logs any request slower than 2 s at WARN (`jsonrpc: slow call`) so a still-indexing or saturated server shows up in `daemon.log`.

When a query does error or time out, `find_symbol`, `workspace_symbols`, and `list_symbols` fall back to the topology index (when `[topology]` is enabled) instead of failing — returning approximate results annotated `source=topology, mode=indexed-approximate` with a `[topology fallback — … may be stale]` line. The fallback is wired via a nil-safe `WithTopologyFallback` setter in `registerAllTools`, runs under the original request context (not the expired LSP deadline), and is a no-op when topology is disabled or has no match (so the authoritative LSP error still surfaces). The position/semantic tools (`get_definition`, `find_references`, `explain_symbol`, hierarchies, `rename_symbol`) have no topology equivalent and surface the error unchanged.

### `[topology]` section — semantic index

```toml
[topology]
enabled                 = false   # opt-in; default false
resync_on_attach        = false   # full resync each time the workspace attaches
exclude_patterns        = []      # path globs to skip during indexing
max_file_size_bytes     = 524288  # 512 KiB cap on files considered
resync_batch            = 100     # files per pause during a full resync (0 disables pacing)
resync_pause_ms         = 25      # pause after each batch, ms (0 disables pacing)
resync_interval_minutes = 60      # periodic full resync; 0 disables
```

| Field | Default | Effect |
|---|---|---|
| `enabled` | `false` | Turn on the persistent SQLite/FTS5 index at `<workspace>/.plumb/topology.db`. |
| `resync_on_attach` | `false` | Force a full resync each time the workspace attaches. |
| `exclude_patterns` | `[]` | Path globs skipped during indexing. |
| `max_file_size_bytes` | `524288` | Largest file extracted (512 KiB). `0` uses the default. |
| `resync_batch` | `100` | Files the full `processResync` walk extracts before pausing `resync_pause_ms`, so the background indexer cannot saturate a core. Only the full resync walk is paced — write-triggered upserts are never delayed. `0` disables pacing. |
| `resync_pause_ms` | `25` | Pause (ms) after each `resync_batch` files. Interruptible (`errResyncAborted`) so daemon shutdown stays fast. `0` disables pacing. |
| `resync_interval_minutes` | `60` | Interval between periodic full resyncs for enabled workspaces, so the index recovers from external changes (`git pull`, branch switch, edits by other tools). `0` disables periodic resync. |

Topology is **disabled by default**. The index is exposed through six `topology_*` tools (see the tool table) and backs the LSP fallback above; `plumb doctor` reports its health (the "Indexing" check, also in `--json`) and the TUI Sessions detail panel shows a topology row when an index exists. `topology.db` and its `-wal`/`-shm` sidecars are added to `<workspace>/.plumb/.gitignore` automatically (`ensureGitignore`) so the rebuildable index is never committed. See the [Topology guide](docs/topology.md) for an accessible overview.

**Known limitation:** `topologyPool` (`internal/cli/topology_pool.go`) is built once from the daemon's *global* `cfg.Topology` and uses it for every workspace; per-connection logic only consults project config to decide *whether* to attach. So per-project `[topology]` *tuning* (interval, batch, excludes, max size) is not yet honoured — only enable/disable is per-project. Tracked in `docs/internal/todo.md`.

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

**Auto-attach fallback (opt-in, 0.6.4+).** When `Detect` returns an error (no marker found) and `[workspace].auto_attach = true`, `OnBeforeTool` calls `pool.SynthesiseRoot(seedDir)` which walks up to the nearest `.git/` directory, falling back to the seed directory itself. The synthetic root is used for stats, TUI, and project config; LSP is unavailable (language `none`). The session JSON and TUI session list mark synthetic sessions with `Synthetic = true` / `(auto)` so they are visually distinguishable. If `[workspace].auto_attach_persist = true`, the daemon also creates `<root>/.plumb/` on first attach so subsequent sessions resolve normally. Both flags default to `false`; existing sessions with markers are unaffected. Env vars: `PLUMB_AUTO_ATTACH`, `PLUMB_AUTO_ATTACH_PERSIST`.

When the daemon starts without a workspace (Claude Desktop launches the daemon from `$HOME`), `session_start` resolves it via this chain:

1. Explicit `workspace` argument to the tool call
2. Daemon's already-resolved workspace
3. `roots/list` query to the MCP client
4. Walk up from `os.Getwd()` for a project marker

Run `plumb init` in any project root to create a `.plumb/` marker directory (also holds `context.md`, the `memories/` store, and — when `[topology]` is enabled — `topology.db`). Stats are global (`config.DataDir()/stats.db`), not per-project. For non-Go/non-Python projects this is now sufficient to get the full daemon experience — no language server, but everything else.

## Adapter validation status

| Adapter | Status |
|---|---|
| `gopls` | **Validated** — unit-tested with mock transport; integration-tested against real gopls binary; `client/registerCapability` answered, `workspace/didChangeWatchedFiles` confirmed |
| `pyright` | **Validated** — unit-tested with mock transport; integration-tested against real pyright-langserver binary; `client/registerCapability` answered, `workspace/didChangeWatchedFiles` confirmed |
| `jdtls` | **Validated** — unit-tested with mock transport; integration-tested against real jdtls binary (`TestIntegration_DidOpen` passes); `client/registerCapability` answered (jdtls sends string IDs — the conn now uses `json.RawMessage` for ID to handle both integer and string forms); `workspace/didChangeWatchedFiles` + `textDocument/didOpen` confirmed. Enable with `[lsp.java] enabled = true` in config. Requires Java 21+ and jdtls on PATH. **Note**: unlike gopls/pyright, jdtls requires both `DidChangeWatchedFiles` (project-model update) and `DidOpen` (triggers reconcile) for reliable diagnostics after external file writes; `DidOpen` must be sent after the server's `ServiceReady` notification. |

Current real-binary adapter validation has been exercised on macOS. Linux and Windows validation are expected pre-v1 hardening work.

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
6. Document inputs, outputs, and required LSP capabilities in `docs/tools.md`.
7. Update this file's tool table.

## Available tools (47)

**Bootstrap**

| Tool | File | LSP method | Notes |
|---|---|---|---|
| `session_start` | `session_start.go` | — | Call FIRST in every session. Returns workspace, language, branch, recent commits, recently-modified files, memories, top-5 tool stats, active diagnostics. Cold-start chain: explicit → daemon-resolved → `roots/list` → cwd walk. Appends a client-specific tool guidance section: Claude Code gets LSP tools that have no native CC equivalent; Claude Desktop gets a full tool listing with the note that plumb is the only interface (no native fallbacks). |

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
| `rename_symbol` | `rename_symbol.go` | Atomic workspace-edit application (`textDocument/rename`). Detects "out of range" position errors (stale LSP index after in-session edits) and wraps them with a clear explanation and three recovery options. |
| `replace_symbol_body` | `symbol_edits.go` | `include_doc_comment` optional. |
| `insert_before_symbol` | `symbol_edits.go` | `include_doc_comment` optional. |
| `insert_after_symbol` | `symbol_edits.go` | No doc-comment ambiguity. |
| `safe_delete_symbol` | `symbol_edits.go` | Refuses if external refs exist. `include_doc_comment` optional. |

**Filesystem reads**

| Tool | File | Notes |
|---|---|---|
| `read_file` | `read_file.go` | Absolute path or `file://`. Line ranges stream via `bufio.Scanner`. 200 KiB cap. Binary detection. Records mtime in `ReadTracker` for strict mode. Output header: `# plumb-read mtime=<RFC3339Nano>` — copy into `edit_file.expected_mtime`. |
| `read_symbol` | `read_symbol.go` | Reads the source body of a named symbol (function, method, type) in one call via `textDocument/documentSymbol` + file read — collapses the `list_symbols` + `read_file` pattern. Accepts a plain name or dotted `ReceiverType.MethodName` form; returns all matches when the name is ambiguous. Records mtime in `ReadTracker` and emits the same `# plumb-read mtime=<RFC3339Nano>` header as `read_file`, so the value can be passed to `edit_file.expected_mtime`. |
| `read_multiple_files` | `read_multiple_files.go` | Up to 20 paths. Parallel (cap 8). Per-file errors inline. |
| `list_directory` | `list_directory.go` | Immediate children, `[FILE]`/`[DIR]` prefixes, sizes, mtimes. Glob `pattern`. Sort by name/size/modified. |
| `list_files` | `list_files.go` | Recursive; glob filter; depth control; respects `.gitignore`. |
| `find_files` | `find_files.go` | Glob/regex finder; honours `.gitignore`. |
| `search_in_files` | `search_in_files.go` | ripgrep-style; smart-case; honours `.gitignore`; `exclude` glob patterns prune directories and files. `include_enclosing_symbol: true` annotates each match with the deepest LSP symbol containing it (`[in: Name (kind)]`); one `DocumentSymbols` call per matched file, results cached. Requires LSP; silently omitted when unavailable. |

**Filesystem writes** (all take `WriteDeps`; all hold per-path locks; all check git dirty state; all notify LSP via `didChangeWatchedFiles`; all invalidate the symbol cache; all consume one rate-limit slot)

| Tool | File | Notes |
|---|---|---|
| `write_file` | `write_file.go` | Atomic (tmpdir + rename + EXDEV-fallback). Symlink-aware. Permissions preserved. Post-write diagnostics appended to response. `FileCreated`/`FileChanged` notification. `dirty_ok` param (default false) — refused if target has uncommitted changes. |
| `edit_file` | `edit_file.go` | str_replace; uniqueness lock; CRLF tolerance; in-memory application (all-or-nothing); pre-rename mtime check; post-rename concurrent-write retry (max 3); optional `expected_mtime` opt-in concurrency check; strict-mode requires-read check; line-range summary + post-write diagnostics in response. `dirty_ok` param. `apply_partial: true` applies each edit independently — failures collected per-edit rather than rolling back the whole batch (not compatible with strict mode or `transaction_apply`). |
| `delete_file` | `delete_file.go` | Refuses directories. `FileDeleted` notification. `dirty_ok` param. |
| `rename_file` | `rename_file.go` | **Primary move tool** — use this to move files. Atomic. Two-path locks (lexical order, deadlock-safe). `FileDeleted` + `FileCreated` notifications. Refuses to overwrite without `overwrite=true`. `dirty_ok` param (checks source). |
| `copy_file` | `copy_file.go` | Duplicate a file preserving permissions. Cross-device safe (read+write, no `os.Rename` dependency). Two-path locks. `FileCreated` notification. Refuses to overwrite without `overwrite=true`. `dirty_ok` param. |
| `transaction_apply` | `transaction.go` | Multi-file atomic edits. Up to 50 ops. Phase 1 validates everything in memory; phase 2 writes under locks; phase 3 rolls back successful writes on partial failure. Each op consumes one rate-limit slot. Use for cross-file refactors. `dirty_ok` param — batched check per directory, all dirty files reported at once. |

**Other**

| Tool | File | Notes |
|---|---|---|
| `find_replace` | `find_replace.go` | Text/regex find-and-replace across files; dry-run by default. `format_after: true` runs the workspace formatter (`gofumpt`/`gofmt` for Go, `ruff`/`black` for Python) on each modified file after replacement; formatter errors are warnings, not failures. |
| `git` | `git.go` | Unified tiered git tool. **Read** (always): `status`, `log`, `diff`, `show`, `blame`, `shortlog`, plus `branch`/`tag`/`stash` listing. **Write** (gated by `[git] allow_writes`, default on): `add` (typed `files`), `commit` (typed `message` → `-m`), `switch`, `mv`, `branch`/`tag` create, `stash` push/pop. **Destructive** (`allow_destructive` + `confirm:true`): `reset`, `clean`, `checkout`, `restore`, `rebase`, `revert`, `branch`/`tag` delete, `stash` drop. **Network** (`allow_push` + `confirm:true`): `push`, `fetch`, `pull`. Subcommand always leads the argv (no global-flag injection); ad-hoc URL pushes and force-pushes to a protected branch are always refused. Unknown subcommands rejected. |
| `git_init` | `git_init.go` | Initialise a git repo at a path. `init_plumb: true` also creates `.plumb/context.md`. |
| `file_diff` | `file_diff.go` | System `diff -U`. |
| `version` | `version.go` | Server version, Go runtime, OS/arch. |
| `daemon_info` | `daemon_info.go` | Current session name, session ID, daemon version, start time, uptime. |
| `rename_session` | `rename_session.go` | Rename the current MCP session. Letters, digits, and `-` only; user-provided case is preserved; max 25 characters. |

**Topology** — SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`. Enabled via `[topology] enabled = true`. Disabled by default.

| Tool | File | Notes |
|---|---|---|
| `topology_status` | `topology_status.go` | Index health: file count, entity count, DB size, indexed languages, last sync time, last error. Returns a clear "topology is disabled" message when the store is nil. |
| `topology_search` | `topology_search.go` | FTS5 ranked symbol/file search. Inputs: `query`, optional `kinds` filter, optional `language` filter, `limit` (default 20), `include_snippets` (default true). Reports `source=topology`, `mode=ranked`, index freshness. |
| `topology_explore` | `topology_explore.go` | BFS neighbourhood around a named symbol. Inputs: `name`, `depth` (default 2, max 4), `max_nodes` (default 50, max 200), `max_bytes` (default 30 000, max 100 000), `include_source` (`none`/`signatures`/`snippets`/`full`), `edge_kinds`. Reports truncation when budget is exhausted. |
| `topology_impact` | `topology_impact.go` | Bidirectional blast-radius analysis around a named symbol. Two sub-graphs: outward (what this symbol depends on) and inward (what depends on this symbol). Inputs: `name` (required), `depth` (default 3, max 4), `max_nodes` (default 100, max 200), `max_bytes` (default 30 000, max 100 000), `edge_kinds` (default `["imports","calls"]`). |
| `topology_affected` | `topology_affected.go` | Given changed files or symbols, return likely affected files and tests. Inputs: `files` ([]string), `symbols` ([]string), `max_results` (default 50). Two sections in output: "affected files" and "likely affected tests" (KindTest nodes). Use after writing to suggest which tests to run. |
| `topology_routes` | `topology_routes.go` | Framework-aware entry-point scanner. Inputs: `framework` (optional: `"go"`, `"python"`, `"cobra"` — omit for all), `path_prefix` (optional), `limit` (default 20). Matches Go HTTP handlers, Cobra Run/RunE, Python `@app.route`/`@router.get` patterns. Confidence annotation mandatory — results are heuristic. |

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
- Do not import or add `charm.land/bubbletea`, `charm.land/lipgloss`, or `charm.land/bubbles` v1 packages. Mixing Charm v1 and v2 packages causes type/API incompatibilities; keep every Charm dependency on the `/v2` module path.
- `Model` is exported; `NewModel(logPath string)` is the constructor; `Run(logPath string)` is the entry point.
- `View()` returns `tea.View`. Use `tea.NewView(content)` and set `v.AltScreen = true`.
- Key handling: `tea.KeyPressMsg`, match via `msg.String()`.
- Section menu opened with `/`; sections are `Dashboard`, `Sessions`, `Memory`, `Logs`, `Settings` (indices 0–4).
- Sessions section (index 1, default): two-panel layout — **Sessions** list (left) and **Details** / **Tool Statistics** / **Recent Tools** (right).
- Logs section (index 3): full-width live log viewer (`internal/tui/log_view.go`). Tails `daemon.log` from `daemonLogPath()` (passed by CLI at startup). Filters by substring (`logFilter`); follows newest entry when `logFollow = true` (default). Press `G` to re-engage follow, `esc` to clear filter, type to filter.
- Settings section (index 4): full-width, **scrollable** grouped settings screen (`internal/tui/model_settings.go`, keys in `model_settings_keys.go`). Rows are grouped Appearance / Logging / Editing / Indexing / Git / Others; each group header is a faded dotted rule (`╌`). Columns are aligned (label / value / control) via `settingsColumnWidths`; the scrollable list is described width-independently by `settingsLogicalLines` (shared by the renderer and the click resolver). The Git group (`allow_writes`/`allow_destructive`/`allow_push`) and Others group (cache TTL + max size, lsp-query timeout, workspace auto-attach) are fully editable. `↑↓`/`jk`/`pgup`/`pgdown` move, `←→`/`-`/`+` change the focused row (toggle / cycle / number / duration-preset), `enter`/`space` toggles or opens the theme popup. **Mouse:** wheel scrolls (`scrollSettings`), click selects a row (`selectSettingAtBodyRow`). A global `ctrl+t` shortcut (`maybeOpenThemePicker`) opens the theme picker from *any* section — it is listed only in the help overlay, not the status bar, and every full-width renderer composites overlays via the shared `applyOverlays`. A two-line pinned footer bar sits just above the bottom border on a subtle `SettingsBar*` background inset one column from each border (text begins one further column in): line 1 carries the `*` restart legend on the left and the navigation shortcuts (brighter `SettingsBarKeyStyle` keys) on the right; line 2 always shows `settingsStatus` (seeded to the active theme on open). Each edit persists to the global config via `config.Save` (`buildSettingItems` is rebuilt from the live `settingsCfg` snapshot). Only **Theme** (TUI-local) and **Log level** (pushed live to the daemon via the control socket in `m.ctrlPath`) take effect immediately; every other row is marked `*` (applies on next daemon start). The theme picker is a centred popup overlay (`renderThemePicker`) split into **Dark** and **Light** sections — each a faded dotted-rule header (`╌`) over its rows (`❯` cursor, `✓` after the active theme's name) — with a bottom status bar reusing the `SettingsBar*` treatment. It dims the whole screen via `spliceOverlay`; cursor navigation follows `themePickerOrder` (dark-then-light) and each move applies+saves the theme live (no revert), `esc`/`enter` closes. State lives on `Model`: `settingsCfg`, `settingsItems`, `settingsCursor`, `settingsScroll`, `settingsStatus`, `showThemePicker`, `themePickerCursor`.
- `panelFocus` constants: `focusSessions`, `focusToolStats`, `focusStats`, `focusDetails`, `focusLogs` — only the first four are used in the Sessions section; `focusLogs` is reserved for future use within the Logs section.
- Overlays: dim the background with `dimLines()`, splice the box via `spliceOverlay()`.
- **Theme system:** `ActiveTheme` (`tui.Theme`) and `ActiveThemeName` (`string`) are package-level globals in `internal/tui/theme.go`. All 40+ lipgloss styles are package-level vars rebuilt by `RebuildStyles()` — call it after every `ActiveTheme` mutation. `AvailableThemes map[string]Theme` is the built-in catalogue. `ThemeNames() []string` returns sorted keys. `Theme` has 16 `color.Color` fields, plus `ContrastText` (text on coloured backgrounds; `"0"` dark, `"15"` light) and `ChromaStyle` (chroma style name for `plumb config show`). Adding a new field to `Theme` requires updating every theme literal — `TestTheme_AllFieldsSet` catches omissions.

## Code style rules

- **Australian English** in all prose: docs, comments, log messages, error strings. Use -ise/-isation (initialise, serialise, synchronise, organise, recognise). Use behaviour, colour, honour, favour. **Exception:** identifiers defined by external specifications keep their canonical spelling — LSP method names (`initialize`, `publishDiagnostics`), MCP protocol fields, and Go standard library names are never changed.
- **`gofumpt`** on save. `golangci-lint` v2.12.2 before every commit; CI enforces.
- **`log/slog`** exclusively. Never `log` package or `fmt.Println` for logging.
- **Errors wrap context:** `fmt.Errorf("loading config: %w", err)`.
- **Context everywhere:** every blocking/I/O operation takes `context.Context` first.
- **Concurrency contract** stated in doc comments on every type.
- **No `init()` doing real work.** Wire dependencies in constructors.
- **No globals** except package-level style vars in `internal/tui/styles.go` (rebuilt, not stateful) and the `pathLocks` map in `internal/tools/file_write_helpers.go` (process-global by design).
- **Max ~600 lines per file.** Split if it grows. Exception allowlist — files where a single unit is the correct design and splitting harms readability: `internal/lsp/protocol/types.go` (protocol type catalogue mirroring the LSP spec). No other file qualifies without explicit justification added here.
- **Comments only when the WHY is non-obvious.** No what-comments.
- **Gocyclo-15 contract.** No first-party non-test function may have cyclomatic complexity above 15. Functions that exceed the gate must be decomposed before merging. Run `golangci-lint run` to check — CI enforces.

## Tool implementation pattern

Every `Tool.Execute()` must be a thin orchestrator over four named, individually-testable steps. This is the required pattern — PRs that add a monolithic `Execute()` are non-conforming.

```go
func (t *Foo) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
    args, err := parseFooArgs(raw)        // JSON decode + shape validation only
    if err != nil { return "", err }
    if err := args.validate(); err != nil { return "", err }
    res, err := t.run(ctx, args)          // domain logic — no formatting
    if err != nil { return "", err }
    return formatFooResult(res), nil      // presentation — no logic
}
```

**Before (monolithic):**

```go
func (t *FindFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
    var a struct { Pattern string; Path string; MaxResults int /* ... */ }
    json.Unmarshal(raw, &a)
    if a.Pattern == "" { return "", fmt.Errorf("pattern required") }
    if a.MaxResults == 0 { a.MaxResults = 500 }
    root := resolvePath(a.Path, t.ws)
    // 80 lines of walk, match, format all inlined …
}
```

**After (decomposed):**

```go
func (t *FindFiles) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
    a, err := parseFindFilesArgs(raw)
    if err != nil { return "", err }
    hits, walkErr := t.collectFiles(ctx, a)
    return formatFindFilesResult(hits, a.MaxResults, walkErr), nil
}
```

Each inner function stays under gocyclo 15; each is independently unit-testable.

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

The daemon writes its build version to `~/Library/Caches/plumb/plumb.version` on start. `plumb serve` reads it and warns on mismatch ("run `plumb stop` to refresh"). If you've just rebuilt and a different daemon version is running, **restart the daemon** — no amount of new code will activate against the old process. Use `plumb stop --force` to skip the confirmation prompt when no interactive terminal is available (scripts, Makefiles).

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
make verify      # build + test + lint — definition of "ready to commit"
make tidy        # go mod tidy
make clean       # remove ./plumb
make install-hooks  # install pre-commit hook (required after every fresh clone)
```

**`make install-hooks` is required after every fresh clone.** The pre-commit hook runs `golangci-lint run --fix ./...` so formatting and lint issues are caught before commit. Without the hook, unlinted code can reach the tree.

**Formatting note:** `gofumpt -w` (standalone binary) may disagree with the `gofumpt` formatter embedded in `golangci-lint` v2.12.2 — the two can pin different versions. Always apply formatting via `golangci-lint run --fix ./...`, never via the standalone binary, to avoid phantom lint failures.

## Known limitations and pending work

Outstanding items, footguns, and "subtle things to be aware of" live in [`docs/internal/todo.md`](docs/internal/todo.md). Always check it before adding a feature that touches concurrency, the rate limiter, the read tracker, or the stats schema — there are known limitations in each of those that any new work needs to either respect or address.

When you complete a TODO item, delete the section from `docs/internal/todo.md` *in the same commit* that adds the `CHANGELOG.md` entry.

## Quick reference for agents

You are likely an AI agent reading this through plumb. Treat the following as the most common patterns:

- **First call:** `session_start({})`. Returns the orientation packet.
- **Read before edit:** call `read_file`, copy its `mtime` header value, pass it as `expected_mtime` to `edit_file`. Mandatory under strict mode; recommended always.
- **One-shot file create:** `write_file({path, content})`.
- **Targeted edit:** `edit_file({path, edits: [{old_str, new_str}], expected_mtime})`. `old_str` must appear exactly once. CRLF differences are tolerated automatically.
- **Cross-file refactor:** `transaction_apply({operations: [{path, edits, expected_mtime}]})`. All-or-nothing.
- **Delete:** `delete_file({path})`. Refuses directories.
- **Move/rename:** `rename_file({from, to})` — primary move tool. Distinct from `rename_symbol` (LSP-semantic identifier rename).
- **Copy:** `copy_file({from, to})`. Preserves permissions; cross-device safe. Use when you want to keep the source.
- **Discover what changed:** `git({subcommand: "status"})` or `git({subcommand: "log", args: ["-10", "--oneline"]})`.
- **See your own activity:** `plumb` TUI's right panel shows "Recent Edits" for the selected session.
- **Throttled?** You hit the rate limit (default 120/min). Wait or set `PLUMB_WRITE_RATE_LIMIT=0`.
- **Rejected for "has not been read"?** Strict mode is on. Call `read_file` first.
- **Rejected for "uncommitted changes"?** The target file is dirty in git. Review and commit the changes, or pass `dirty_ok: true` to overwrite anyway.
- **Too much log noise from the daemon?** `plumb log-level warn` raises the floor instantly, no restart required. `plumb log-level reset` restores the config-file default.

When in doubt about the resolved config, `plumb config show --workspace .` from the project directory.
