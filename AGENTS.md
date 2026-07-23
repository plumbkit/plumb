# Plumb — Agent Instructions

> Source of truth: edit `AGENTS.md` only.
>
> `CLAUDE.md` and `GEMINI.md` are repository symlinks to this file for client compatibility; Codex and ChatGPT read `AGENTS.md` directly. Do not replace, unlink, rewrite, or edit those symlink paths directly. If an instruction change is needed, update `AGENTS.md`; the linked files will reflect it automatically.
>
> These agent-context files are tracked in git to ensure a consistent, high-quality experience for AI assistants.

This file is the canonical brief for AI agents working in the plumb codebase. Keep it accurate; it ages fast.

> **CRITICAL — tool priority:** Always use plumb MCP tools for all tasks when plumb is present and the required capability is available through plumb. Do not fall back to native tools (Read, Edit, Bash, shell commands, etc.) for file reads, writes, edits, searches, symbol lookups, or git queries when the equivalent plumb tool exists. Plumb tools are LSP-aware, concurrency-safe, and session-tracked; native tools bypass all of that. The only exceptions are tasks plumb explicitly does not cover (interacting with external services, ad-hoc shell). Under the **lean** tool profile the read-only commodity search/list/find tools (`search_in_files`, `find_files`, `list_directory`, …) are hidden from `tools/list` because a lean client's native equivalents suffice — there the always-plumb lane is writes/edits, symbol operations, `git`, and `run_task` (set `[tools] profile = "full"` to also advertise the plumb read tools). Auto-mode serves **full** (not lean) to a client that can only invoke advertised tools — e.g. Claude Code, which builds even its tool-search list from `tools/list`, so a hidden tool would be unreachable rather than merely undisplayed — so for those clients every plumb read tool is advertised and the always-plumb rule applies in full. Build/test/lint now have a plumb path: `run_task` (and the `plumb build/test/lint/e2e/verify` CLI) run the user's stored `[tasks.<lang>]` commands — prefer it over a raw shell `go test`/`npm test` when a task is configured.

> **Per-tool detail lives in the tool's own MCP description.** Each tool registers its full description and input schema (`tools/list`), and `session_start` emits client-specific tool guidance at runtime. This file is orientation, not the authoritative tool reference — when a tool's behaviour matters, read its description.

Current version: see the `VERSION` file and `CHANGELOG.md` (not pinned in this brief, to avoid drift).

## Project purpose

Plumb gives coding agents the intelligence layer of an IDE. It is an MCP (Model Context Protocol) server exposing LSP (Language Server Protocol) capabilities, a tree-sitter topology index, and per-project memory, plus a complete filesystem toolkit (read, write, edit, delete, rename, transaction). It lets an LLM — especially Claude Desktop, Claude Code, Codex, or Gemini CLI, which may have limited filesystem access — navigate, understand, and modify a codebase entirely through structured semantic tools, no raw-file dumping or shell.

The architectural commitments are:

1. **LSP-correct semantics.** Plumb's writes reach the language server via `workspace/didChangeWatchedFiles` (not the open-document lifecycle); capabilities are negotiated; server-initiated `client/registerCapability` requests are answered.
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
| `internal/lsp/adapters/{gopls,pyright,jdtls,rust,swift,zig,typescript,kotlin,html}/` | All languages enabled by default and activated automatically when their server binary is installed (set `[lsp.<lang>] enabled = false` to exclude one). Go + Python validated; the rest experimental. See Adapter validation status below. |
| `internal/topology/` | SQLite/FTS5 semantic graph; background indexer; Go AST, gotreesitter (many langs, below), and canonical-tree-sitter-via-WASM TypeScript/TSX/JSX + Swift (`wasmts`); search + BFS explore/impact/affected/routes |
| `internal/topology/extractors/golang/` | Go extractor (`go/parser`+`go/ast`; no CGo) |
| `internal/topology/extractors/treesitter/` | gotreesitter extractors (pure-Go, no CGo): Python, JavaScript, Rust, Zig, Kotlin, Swift, Java, Bash, HCL, SQL, Dockerfile, TOML, YAML, Markdown, HTML. Config/IaC/markup grammars extract named declarations (TOML/YAML/Markdown/HTML also index nesting via containment edges; HTML and Markdown are flagged `PreferStructuralOutline` so outline tools use the Map over the noisy LSP). JavaScript (`.js`/`.mjs`/`.cjs`) is here; TS/TSX/JSX and Swift moved to `wasmts` (the gotreesitter Swift extractor + its `recoverIUOBangs` IUO workaround remain only as the wasm init-failure fallback). Embeds the `grammars` package (~+26 MB); pinned v0.20.1. **Memory discipline:** each extractor decodes its grammar **lazily** (a `lazyGrammar` resolved on first `Extract`, not in the constructor) and `defer tree.Release()`s its parse arena back to gotreesitter's pool after the walk — so grammar memory scales with the languages a workspace actually contains, not the full supported set, and a resync recycles one arena instead of allocating per file. |
| `internal/topology/extractors/typescript/` | Legacy regex TS/JS extractor; retained only as the `wasmts` init-failure fallback. |
| `internal/topology/extractors/wasmts/` | Grammar-generic WASM extractor driven by wazero (pure-Go). **TypeScript + TSX/JSX** via the **canonical** `tree-sitter-typescript` grammar (`ts.wasm`, ~2.9 MB, `make ts-wasm`) — parses typed-arrow TSX cleanly where gotreesitter cascades. **Swift** via the **canonical** alex-pinkus `tree-sitter-swift` grammar + its C external scanner (`swift.wasm`, ~3.5 MB, `make swift-wasm`) — parses implicitly-unwrapped optional types (`var x: T!`) that collapse the gotreesitter port. Each bundle has its own builder; both need Zig only to regenerate. |
| `internal/langsupport/` | Per-language capability registry (structural engine + LSP adapter). Single source of truth for `buildExtractors`; the seam for moving a language onto tree-sitter. |
| `internal/tools/` | MCP tool implementations; `WriteDeps` bundles write-tool deps; `txlog` subpackage is the transaction rollback WAL |
| `internal/quality/` | Offline post-write analysers (golangci-lint, …) on changed files; findings appended to write responses |
| `internal/cache/` | Session-scoped symbol cache + LSP-driven invalidator |
| `internal/config/` | TOML config, XDG paths, project-config merging |
| `internal/session/` | Session-file registration + client identity tracking |
| `internal/stats/` | Global SQLite tool-call statistics, row-scoped by workspace and session (WAL, P95, client-aware). Writes funnel through one batched-transaction `Writer` (single-writer goroutine; non-blocking enqueue, never on the response path); reads use a process-cached `SharedReadOnly` handle. Also holds the `episodic_memories` table; stats schema `user_version` 13 |
| `internal/memory/` | Per-workspace markdown memory store (source of truth), exposed as MCP resources. Plus a rebuildable per-workspace FTS5 index (`memory.db`, separate from `topology.db`) backing ranked `search_memories`; generated-memory provenance + redaction (`internal/redact`); and `paths:`-glob hint matching for response injection |
| `internal/redact/` | Secret scrubber (API keys, tokens, PEM keys, URL credentials, secret assignments) applied before any generated/episodic memory is persisted |
| `internal/tui/` | Bubble Tea v2 TUI — live session + stats dashboard, recent-edits panel |
| `internal/render/` | Shared, pure CLI/TUI presentation helpers (stdlib + rendering libs only) |
| `internal/fsguard/` | Guards filesystem walks against macOS TCC false-positive prompts on protected dirs |
| `internal/monitor/` | Process resource-usage snapshots (CPU %, memory) plus daemon start time, feeding the TUI daemon metrics |
| `internal/cli/` | Cobra subcommands; daemon, proxy, pool, workspace detection, `config show` |

## Daemon architecture

`plumb serve` is a resilient stdio proxy. The real server is `plumb daemon`:

```
Claude Desktop / Claude Code / Codex / Gemini CLI
  └── plumb serve  (per conversation — dials Unix socket, frame-aware proxy)
        └── ~/Library/Caches/plumb/plumb.sock  (macOS; os.UserCacheDir())
              └── plumb daemon  (one process, shared across all conversations)
                    ├── workspacePool  (one LS per (root, language) — a root may run several, e.g. Go + HTML; the primary is refcounted/torn down after idle grace, secondaries are lazy and live to daemon shutdown)
                    │     └── poolEntry{proxy, inv, cache, refs, graceTimer} per (root, language)
                    └── handleConn()  (per-connection MCP session: readTracker,
                          writeLimiter, editsCfg/strictFn, gitCfg/gitPolicyFn,
                          sessionCache, lsRefRoot — all per-connection/per-project)
```

On daemon start the binary writes these under `os.UserCacheDir()/plumb` (`~/Library/Caches/plumb/` on macOS, `~/.cache/plumb/` on Linux):

| File | Purpose |
|---|---|
| `plumb.sock` | Unix socket — MCP wire |
| `plumb.pid` | PID for `plumb stop` |
| `plumb.version` | Build version; `plumb serve` warns on mismatch |
| `plumb.spawn.lock` | `flock`'d briefly by `plumb serve` to serialise daemon spawn decisions |
| `plumb.daemon.lock` | `flock`'d by `plumb daemon` for its lifetime; a second daemon sees `EWOULDBLOCK` and exits |
| `plumb.ctrl.sock` | Admin socket; line-based `set-level <level>` commands from `plumb log-level` |

plumb resolves all of its own base directories through `internal/paths`, which delegates to the de-facto cross-platform library `github.com/adrg/xdg` — no hand-rolled per-OS path logic. The runtime files above (socket/pid/locks/version) stay in the cache dir; **config**, **data** (sessions, `stats.db`) and **state** live under `~/Library/Application Support/plumb` on macOS and the XDG base dirs on Linux (`~/.config`, `~/.local/share`, `~/.local/state`). The daemon **log** is the one exception — it uses the OS-native log location (`~/Library/Logs/plumb/daemon.log` on macOS, the XDG state dir on Linux), resolved by `internal/paths.LogDir`, since no base-directory spec models a logs directory. A pre-0.9.8 config at `~/.config/plumb/config.toml` is still honoured as a read fallback.

Stats live in one global DB at `config.DataDir()/stats.db`; every row carries `workspace` and `session_id`, and project/session views filter on those.

**Memory bounds & introspection.** At startup the daemon applies a *soft* heap limit via `debug.SetMemoryLimit` (`internal/cli/memlimit.go`): `PLUMB_MEMORY_LIMIT` (a byte size like `1500MiB`, or `0`/`off`/`unlimited` to disable) overrides a tight-but-comfortable 1 GiB anti-OOM backstop default — Go GCs harder near the limit and never hard-fails, so a transient spike is bounded. The active limit is logged. Three admin commands over `plumb.ctrl.sock` expose live state: `plumb debug mem` prints a `runtime.ReadMemStats` snapshot (`HeapAlloc`/`HeapInuse`/`HeapSys`/`HeapReleased`/`NumGC`/`Goroutines`), `plumb debug heap` forces a GC and writes a `runtime/pprof` heap profile to the cache dir (`plumb.heap.<ns>.pprof`) for `go tool pprof`, and `plumb debug stacks` writes a full goroutine stack dump (`plumb.stacks.<ns>.txt`, the pprof `goroutine` profile at `debug=2` — the non-destructive `SIGQUIT` equivalent) for diagnosing a live hang. A full topology resync ends with `debug.FreeOSMemory()` so the large transient working set returns to the OS rather than lingering as idle heap spans. Note: the TUI daemon widget's RSS row is the *current* sample, not a peak.

**Singleton enforcement** (`internal/cli/lock.go`): the two `flock`s above serialise `plumb serve`'s spawn decision and keep `plumb daemon` a singleton (a second daemon exits on `EWOULDBLOCK`); both release on process exit.

**Resilient proxy** (`internal/cli/serve_proxy*.go`): `plumb serve` is a frame-aware reconnecting proxy that survives a daemon crash or hang without the client noticing. On a daemon failure it keeps the client's stdio open, dial-or-spawns a fresh daemon, and **replays the captured MCP handshake** (the client only sends `initialize` once). In-flight requests get a synthesised retryable error (`code -32000`) instead of hanging; non-idempotent writes are never auto-replayed. A *hung* daemon is caught by an idle `ping` heartbeat, then `SIGTERM`→`SIGKILL`'d and respawned. Reconnects are bounded. The replayed handshake also carries a stable per-proxy session ID (in the `initialize` params' `_meta`) so the fresh daemon recognises the reconnected connection as a continuation and rehydrates its persisted per-connection state (see `[session]` `persist_state`) — making the restart transparent rather than merely non-fatal. Knobs: `PLUMB_PROXY_RECONNECT` (default on; off ⇒ legacy `io.Copy` proxy), `PLUMB_PROXY_HEARTBEAT` (`0` disables hang detection), `plumb serve --no-reconnect`.

**Per-connection write deadline** (`internal/mcp/server.go`): each MCP response write to the socket is bounded by a `SetWriteDeadline` (default 30 s, `PLUMB_WRITE_TIMEOUT`; `0`/`off` disables). Without it a blocked `conn.Write` would hold the connection's write mutex forever and wedge every later reply on that connection (`daemon_info` included) to the client timeout. A lapsed deadline fails the write, marks the connection broken, and cancels its `Serve` loop so the connection is torn down with a clear error — the resilient proxy then reconnects — rather than hanging. Transports without `SetWriteDeadline` (test pipes) are unaffected.

## Configuration layers

Built in four layers, each overriding the prior; `plumb config show` prints the resolved config with provenance.

1. **Compiled defaults** in `internal/config/config.go` `defaults`.
2. **Global config** at `$XDG_CONFIG_HOME/plumb/config.toml` (falls back to `~/.config/plumb/config.toml`). Held in a live `config.Store` and hot-reloaded via fsnotify, the `reload-config` control command, or `plumb config reload`. Settings the daemon cannot apply live (LSP servers, cache, log format) are flagged restart-needed by `plumb config show` and `daemon_info`.
3. **Project config** at `<workspace>/.plumb/config.toml`. Merged onto global per connection — only fields the project sets are overridden.
4. **Environment variables** — highest precedence.

### `[edits]` — write-tool safety

```toml
[edits]
strict = false                # require read_file (matching mtime) before edit_file and the symbol-edit tools (rename_symbol exempt); per-session
rate_limit_per_minute = 120   # sliding-window cap per session; 0 disables. A shared parent budget (keyed by (client, workspace)) caps combined rate across connections from the same client to one project
show_write_diff = true        # append a unified diff to the content-modifying tools' responses: edit_file/write_file/undo_edit, the semantic symbol edits (replace_symbol_body, insert_before/after_symbol, safe_delete_symbol — preview + applied), and transaction_apply
block_dirty_writes = true     # refuse a destructive write to a file with uncommitted git changes plumb did not write this session, unless dirty_ok; set false to disable the guard entirely (iterate on uncommitted WIP without dirty_ok). Re-editing a file plumb wrote this session is never blocked either way
post_write_diagnostics_ms = 300 # ceiling on the wait for the LSP to re-publish diagnostics after a write; adapts down to observed latency; 0 disables
post_write_cross_file = true  # after a write, compare workspace diagnostics against a pre-write baseline and flag NEW errors the edit introduced in OTHER files (the "edit A silently breaks B" case); the single-file block keeps priority
post_write_cross_file_settle_ms = 200 # bounded grace the cross-file sweep waits, after the edited file's own diagnostics land, for dependent-file re-publishes; 0 compares immediately
```

The cross-file sweep is honest by construction: it only reports a file whose error count ROSE versus the pre-write baseline AND that the language server re-published after the write, so pre-existing errors and untouched files are never mis-attributed; the edited file's own block is unaffected and returned first, and the heads-up hedges the mid-series case rather than claiming "the build is broken".

Env: `PLUMB_STRICT_EDITS`, `PLUMB_WRITE_RATE_LIMIT`, `PLUMB_SHOW_WRITE_DIFF`, `PLUMB_BLOCK_DIRTY_WRITES`, `PLUMB_POST_WRITE_DIAG_MS`, `PLUMB_POST_WRITE_CROSS_FILE`, `PLUMB_POST_WRITE_CROSS_FILE_SETTLE_MS`.

### `[workspace]` — root detection fallback + path-access roots

```toml
[workspace]
auto_attach = false           # fall back to SynthesiseRoot (nearest .git/ or seed) when no marker found; LSP unavailable
auto_attach_persist = false   # create .plumb/ at the synthetic root on first attach (implies auto_attach)
allow_dependency_reads = true # read/search may reach the session language's toolchain stdlib + dependency cache read-only (Go: GOMODCACHE/GOROOT; Zig: stdlib + cache; Rust: rust-src + cargo registry; Python: stdlib + site-packages; Swift: SDK; JVM: Gradle/Maven caches). TypeScript excluded (node_modules is in-workspace). Writes there refused
extra_roots = []              # additional read-WRITE dirs, additive ($VAR-expanded)
read_roots = []               # additional read-ONLY dirs, additive ($VAR-expanded)
child_scan_depth = 2          # levels below a markerless .plumb/ root to scan for language markers in subdirs (monorepo); 0 disables
```

The workspace boundary is enforced per-connection by a **`PathPolicy`** (`internal/tools/pathpolicy.go`): an allowlist of roots tagged read-only or read-write. The detected workspace is always read-write; `extra_roots` add read-write roots; `read_roots` (and, with `allow_dependency_reads`, the session language's toolchain stdlib + dependency cache, resolved per language — Go's GOMODCACHE/GOROOT, Zig's stdlib + cache, Rust's rust-src + cargo registry, Python's stdlib + site-packages, Swift's SDK, JVM's Gradle/Maven caches; TypeScript is intentionally excluded as its node_modules is in-workspace) add read-only roots. Read/search tools admit any allowed root; write tools demand read-write, so a write outside the workspace is refused by construction.

### `[git]` — tiered git tool gating

```toml
[git]
allow_writes = true                     # add, commit, switch, branch/tag create, stash
allow_destructive = false               # reset, clean, checkout, restore, rebase, … (each call also needs confirm:true)
allow_push = false                      # push/fetch/pull (each call also needs confirm:true)
protected_branches = ["main", "master"] # never force-pushable, even with allow_push + confirm
```

Layered and hot-reloaded (`gitPolicyFn`). Classification is *safe-biased* (`classifyGit`, `internal/tools/git.go`): ambiguous subcommands round **up** a tier (`checkout -b` is a write, any other `checkout` is destructive). `add`/`commit` are typed, not pass-through — `commit` only runs `commit -m <message>`, `add` only `add -- <files>`; pre-commit hooks always run. The subcommand leads argv and a denylist rejects `-c`/`-C`/`--git-dir`/`--work-tree`/etc., so global flags can't reconfigure git; no shell. Output is capped (200 lines for `log`/`blame`, 100 KiB overall).

### `[ui]` — TUI theme (global config only)

```toml
[ui]
theme = "plumb"   # built-in theme name; must match a key in tui.AvailableThemes
```

Built-ins: `nordico`, `darcula`, `dracula`, `gruvbox`, `plumb` (dark); `github-light`, `solarized-light`, `plumb-light` (light). The `plumb`/`plumb-light` pair is derived from the project website's own terracotta/sage palette (`site/index.html`). Written live by the theme picker — via a sparse `SetGlobalValue(["ui", "theme"])` that rewrites only the `[ui].theme` key (preserving the rest of the file and never baking in `PLUMB_*` env overrides) — and read at startup. (The whole-file `config.Save` path that other global Settings writes use still rewrites the file, so user TOML comments are lost on first save there.) Project config ignores `[ui]`. The palette catalogue lives in the UI-agnostic `internal/theme` package (hex strings, no bubbletea import) and is consumed by both the TUI and the web UI; `internal/tui/theme.go` holds the TUI `Theme` (lipgloss colours, some terminal-palette indices).

### `[web]` — web UI (global config only)

```toml
[web]
port = 8870   # loopback TCP port for `plumb web`; bound to 127.0.0.1 only
```

The opt-in, loopback-only web UI launched with `plumb web` (full TUI parity: Dashboard, Sessions, Memory, Logs, scope-aware Settings + theme picker). The HTTP server is hosted **inside** the daemon (`internal/web`), bound only when `plumb web` sends `web-start` over the control socket, on `127.0.0.1` only, behind a per-start 256-bit token (query param on first load → `HttpOnly`/`SameSite=Strict` cookie; `Origin`/`Host` CSRF check on the two write paths). Read-only JSON snapshots + SSE streams reuse the existing stats/monitor/session/topology/memory read paths; the only writes are config + theme (via `config.Save`/`SetProjectValue` + reload). The Svelte 5 + Vite + Tailwind SPA (ECharts + uPlot) is `go:embed`'d from `internal/web/ui/dist` — a committed placeholder keeps a bare `go build` compiling; run `make web-ui` then `make build` to pick up SPA changes. Like `[ui]`, `[web]` is global-only (ignored in project config). No env override. The web UI is a daemon surface, not an MCP tool, so the tool count is unchanged. Full guide: [`docs/web.md`](docs/web.md).

### `[lsp_query]` — LSP tool-call timeout

```toml
[lsp_query]
timeout = "30s"   # PLUMB_LSP_QUERY_TIMEOUT — cap on a single LSP tool call; "0s" disables
```

Top-level section (distinct from per-language `[lsp.<lang>]` tables). Applied at the tool layer (`withLSPDeadline`) and a no-op when the context already carries a deadline, so the cold-start handshake is never shortened.

**LSP → topology fallback:** on LSP error/timeout, `find_symbol`, `workspace_symbols`, and `list_symbols` fall back to the topology index (when enabled), annotated `source=topology, mode=indexed-approximate`; a no-op when topology is disabled or has no match. `get_definition` **by name** (`symbol_name`) also falls back to the index when the server is unavailable — approximate (the declaration line resolved by name, annotated `source=topology, mode=indexed-approximate`), since the index has no position-level go-to-definition. The raw-position form of `get_definition` and the other position/semantic tools (`find_references`, the call/type hierarchies, `rename_symbol`) have no equivalent and surface the error unchanged — they need a precise position or a whole-workspace reference graph the index does not hold. **Empty-result fill:** `workspace_symbols` additionally supplements an *empty-but-no-error* LSP answer from the index for **tree-sitter** languages (annotated `topology fill … source=topology, mode=indexed-approximate`) — lazy servers like zls only answer for files they have already analysed, so a freshly-attached session would otherwise report "No symbols found" for a symbol the Map knows. Native-AST languages (Go via gopls, which indexes eagerly) are excluded so an authoritative empty answer is never supplanted.

### `[topology]` — semantic index

```toml
[topology]
enabled                 = true    # on by default. SQLite/FTS5 index at <workspace>/.plumb/topology.db
resync_on_attach        = false   # full resync each time the workspace attaches
exclude_patterns        = []      # path globs to skip during indexing
max_file_size_bytes     = 524288  # 512 KiB cap per file; 0 = default
resync_batch            = 100     # files per pause during a full resync; 0 disables pacing
resync_pause_ms         = 25      # pause after each batch, ms; 0 disables pacing
resync_interval_minutes = 60      # periodic full resync FALLBACK; suppressed while the watcher is live; 0 disables
watch                   = true    # OS-level file watching: re-index on change, whoever made it
```

Enabled by default. On first attach the index is created at `<workspace>/.plumb/topology.db` — the one case where plumb materialises `.plumb/` for a project that lacked it. Only the full resync walk is paced; write-triggered upserts are never delayed. Exposed through six `topology_*` tools and backs the LSP fallback above; `topology.db` (+ `-wal`/`-shm`) is auto-added to `<workspace>/.plumb/.gitignore`. See the [Topology guide](docs/topology.md).

**Live indexing (`watch = true`, default on).** An OS-level watcher ([`fswatcher`](https://github.com/sgtdi/fswatcher)) re-indexes a file the moment it changes on disk, whoever changed it. It feeds the bounded indexer queue, so a mass change (`git checkout`, a formatter) coalesces and, on overflow, collapses to a single paced full resync. While the watcher is live the periodic `resync_interval_minutes` poll is **suppressed** (a full resync still runs at startup and on any dropped signal); the poll is the fallback when `watch = false` or the watcher can't start. `.plumb/` is excluded. Per-project `[topology]` config is honoured on attach and re-applied on reload.

### `[session]` — idle detection & eviction

```toml
[session]
idle_threshold_minutes    = 30    # mark a session idle in the TUI after this long with no tool call
eviction_ttl_minutes      = 60    # daemon force-closes a connection idle this long; 0 disables eviction
persist_state             = true  # persist read-tracking + pinned workspace so they survive a daemon restart
persist_state_ttl_minutes = 1440  # how long persisted per-connection state lingers before pruning; 0 disables pruning
```

Global or per-project. `idle_threshold_minutes` is cosmetic (a `~` marker in the TUI). `eviction_ttl_minutes` has teeth: a daemon-side reaper (5-min tick) cancels a connection whose last tool call was longer ago than the TTL, reclaiming a `plumb serve` whose agent silently disconnected. Read live; `0` disables. The activity signal is a tool call (`LastSeenAt` = session file mtime).

`persist_state` (default on; env `PLUMB_PERSIST_SESSION_STATE`) makes a **daemon restart transparent to a connected agent**: strict-mode read-tracking and the pinned workspace are written to `session_state.db` (in the data dir, beside `stats.db`), keyed by a stable proxy session ID that `plumb serve` injects into the `initialize` handshake `_meta` and replays on every reconnect. On reconnect the fresh daemon rehydrates that state, so a strict-mode `edit_file` of a file read before the restart is not refused, and a client that reports no roots (e.g. Claude Desktop) comes back pinned without an explicit `session_start`. Rehydration is **safe by construction**: a restored read still passes `checkStrictRead`'s on-disk `os.Stat`+mtime comparison, so it can only satisfy an unchanged file, never bypass a dirty-file check. Read-tracking is scoped by `(proxy session, workspace)`, so a re-pin to a different project never resurrects the old project's reads. `persist_state_ttl_minutes` (config-only, default 24h; `0` disables pruning) bounds how long state left by a serve proxy that died without reconnecting lingers; it is independent of `eviction_ttl_minutes` (eviction must not delete state a reconnect may rehydrate).

### `[memory]` — Advanced Memory Engine

```toml
[memory]
enabled               = true   # the memory.db FTS5 index; off ⇒ search_memories uses grep only
generated_summaries   = true   # rule-based episodic summaries (no LLM) written when a session goes idle
inject_hints          = true   # append a "[Hint: relevant memory …]" block to path-bearing tool responses
hint_budget_bytes     = 512    # cap on an injected hint block
episodic_budget_bytes = 1024   # cap on the session_start "Last session" summary
max_hints             = 3      # max memories hinted per response
idle_summary_minutes  = 0      # idle threshold for episodic generation; 0 ⇒ falls back to [session] idle_threshold_minutes
generated_memory_keep = 50     # newest episodic-* markdown memories retained per workspace; 0 disables pruning
```

Project-overridable; no env override; surfaced with provenance in `plumb config show`. The markdown files under `.plumb/memories/` stay the source of truth; `memory.db` is a rebuildable index (mtime+size freshness anchor, grep fallback when stale/absent). Generated and episodic memories are always redaction-scrubbed and clearly lower-confidence than user-authored ones. Hint injection reads only frontmatter (never bodies) on the hot path via a per-connection snapshot of the resolved `[memory]` config (no per-call config read); when user-authored and generated memories compete for the capped hint slots, user-authored ones always win. Hybrid memory v1 (0.9.16): `write_memory` accepts `paths` globs (stored as frontmatter, driving `relevant_memories` and hints), and an idle session that wrote workspace files also leaves a durable `episodic-*` markdown memory — redacted, provenance-stamped, indexed, and pruned to the newest `generated_memory_keep`.

Three behaviours worth knowing (settled in 0.9.16): **per-project `generated_summaries` is honoured both ways** — a project may enable episodic summaries under a global opt-out *or* disable them under a global opt-in; only the idle *threshold* is global-resolved (`idle_summary_minutes` → `[session] idle_threshold_minutes`), and a session is always summarised before it is evicted, even when `eviction_ttl_minutes` is shorter than the threshold. **`search_memories` auto mode greps when FTS finds nothing** — a fresh index that returns zero FTS5 hits (the tokeniser is whole-token, so a substring like `essio` inside `UserSession` won't match) falls through to substring grep; `case_sensitive: true` always uses grep (FTS5 is case-insensitive); a literal `mode: fts` keeps the empty FTS result. **Hint and episodic budgets are byte caps** (`*_bytes`), enforced in bytes on a UTF-8 boundary, so a multi-byte summary cannot overrun.

### `[collab]` — cross-agent sharing

```toml
[collab]
peer_awareness     = true   # tier-1 passive peer awareness (observed facts; a richer version of shipped behaviour)
hint_budget_bytes  = 512    # byte cap (UTF-8 boundary) on any injected peer-signal block
intents            = false  # tier-2 opt-in: share_intent + intent-aware write hint (agent claims)
mailbox            = false  # tier-2 opt-in: leave_note + note delivery at session_start
knowledge_handoff  = false  # tier-3 opt-in: share_findings on the episodic memory pipeline (agent-generated)
intent_ttl_minutes = 120    # expiry for a new intent/note; pruned on the reaper tick, filtered on every read
```

Project-overridable in **both directions** (the `generated_summaries` precedent — a project may disable a tier under a global opt-in, or enable it under a global opt-out); no env override; hot-reloaded; surfaced with provenance in `plumb config show`; snapshotted per connection so the hot path never reads config per call. Everything here is **advisory** (it never blocks a write), **byte-budgeted**, and **strictly per-workspace**.

**Tier 1 (`peer_awareness`, default on)** adds three signals, all verifiable observations derived from writes the daemon itself performed or watched (never agent claims): (1) **topology-annotated `recent_writes`** in `workspace_sessions` — each entry gains its enclosing package/symbol from the topology index (best-effort, `source=topology`); (2) a **peer-activity hint** appended to a path-bearing tool response when another *currently-active* session recently wrote that file (`[Peer: session … edited this file N min ago — consider file_status before editing.]`, recency window `min(idle threshold, 30 min)`); (3) a **`session_start` peer digest** naming active peers and the areas they recently touched.

**Tier 2 (`intents` + `mailbox`, default off)** adds two agent-authored, opt-in write tools — `share_intent` and `leave_note` — whose content is always rendered as an **unverified claim**, distinct from tier-1's observed facts. Intents (one live per session; cleared when the session ends) drive an intent-aware write hint (`[Peer intent (claim, unverified): …]`) and a listing in `workspace_sessions`; notes are delivered at a peer's `session_start` (a `next` note is consumed on first delivery; an addressed note persists until its TTL). Bodies are secret-scrubbed (`internal/redact`) before storage. Rows live in `<workspace>/.plumb/collab.db` (WAL, auto-gitignored like `topology.db`), created **lazily on first use** — a workspace whose `intents` and `mailbox` both stay off never gets a `collab.db` — and pruned on the daemon session-reaper tick (reads filter expired rows regardless). Delivery is polling + hint injection only; plumb cannot push. All injected blocks share `hint_budget_bytes`.

**Tier 3 (`knowledge_handoff`, default off)** adds the `share_findings` write tool — an on-demand flush of the episodic-memory pipeline. Instead of waiting for a session to go idle, an agent hands its findings to peers *now*: the tool writes a generated memory (secret-scrubbed via `internal/redact`, provenance-stamped `confidence=generated` with the authoring session + date, optional `paths:` globs for hint routing), indexes it in `memory.db`, and prunes to `[memory] generated_memory_keep` — the **same retention pool** as idle `episodic-*` summaries (the finding is named `finding-<timestamp>-<session>`, distinguishable but retention-shared). It is instantly discoverable through the ordinary channels — `search_memories`, `workspace_search`, `relevant_memories`, hint injection, the next `session_start` — with **no new storage or delivery mechanism**. The content is agent-authored generated content: lower-confidence than a user-written memory, and it never displaces one in a capped hint slot. Rule-based only — the agent supplies the text; no LLM summary.

### `[rastro]` — Rastro associative-memory integration

```toml
[rastro]
enabled = false     # off by default; nothing is looked up or executed while disabled
path    = "rastro"  # executable name resolved on PATH, or an absolute path
```

Project-overridable; no env override; both fields are `ReloadNextSession` in the field registry, so the TUI Settings screen marks them `²`. Surfaced in the TUI under a **Rastro** group (Enabled toggle, Path text) and written scope-aware like every other row — a workspace row lands in `<workspace>/.plumb/config.toml`, a global row in the global config.

`plumb doctor` grows an **Integrations** section that reports the integration's state: `disabled in config` when off; the resolved executable path (via `exec.LookPath`) when on and found; a **failure** naming the binary and how to fix it when on and absent. An unloadable config is reported there as a *warning*, not a second failure — the Configuration section already fails the run for that fault. `plumb` never executes the binary; it only resolves it.

### `[semantics]` — opt-in semantic re-rank for `topology_search`

```toml
[semantics]
enabled           = false                    # OFF by default — zero cost until enabled
provider          = "openai"                 # openai | voyage | jina | mistral | cohere | custom
model             = ""                        # "" → the provider preset's default model
base_url          = ""                        # "" → the preset's base URL; REQUIRED for custom
api_key           = ""                        # literal key — highest precedence (see below)
api_key_env       = ""                        # "" → the preset's default env var; key's source when api_key is empty
rerank_candidates = 50                        # how many FTS5 hits to re-rank
timeout           = "10s"                      # per embedding HTTP call
```

Project-overridable, hot-reloaded. When `enabled`, `topology_search` re-ranks its FTS5 candidates by embedding similarity (annotated `mode=fts+semantic`); FTS5 stays the authoritative spine and any error falls back to plain ranking (`mode=ranked`). **API / bring-your-own-endpoint only — plumb never bundles, downloads, or supervises a model** (a local-model spike found it does not beat FTS5). One OpenAI-compatible client (`internal/semantics`) covers `openai`, `voyage` (`voyage-code-3`), `jina`, `mistral`, and any self-run OpenAI-compatible server (Ollama / llama.cpp / LM Studio / TEI / vLLM) via `provider = custom` + `base_url`; `cohere` uses a small adapter. **Key precedence:** a literal `api_key` wins, else the key is read from `api_key_env` (or the preset default, e.g. `OPENAI_API_KEY`) — prefer the env var. Embeddings are cached lazily in `topology.db` (`topology_embeddings`, keyed by content hash).

### `[tasks.<lang>]` — per-language build/test commands (run by `run_task`)

```toml
[tasks.go]
build = "go build ./..."
lint  = "golangci-lint run"
test  = "go test ./..."        # may contain a {target} placeholder
e2e   = "go test -tags=integration ./..."
# verify is a COMPOSITE (build then test) — it stores no command of its own
```

Five optional command slots per language, keyed by the `[lsp.<lang>]` id, with shipped defaults (Go fully populated; a slot left empty over guessing an uninstalled tool). A command is a **single argv executed without a shell** (`config.ParseTaskCommand` rejects `&&`/`;`/`|`/`$(`/backtick/redirs); `verify` runs the build slot then the test slot in sequence. Surfaced through the `run_task` MCP tool and the `plumb build|lint|test|e2e|verify` CLI; output and runtime are bounded (100 KiB/200 lines, timeout). The only agent-supplied input that reaches the argv is a shell-safe `{target}` (`^[A-Za-z0-9._/:@-]+$`).

**Trust gate.** A task command the *project's* `.plumb/config.toml` supplies is **not run until trusted** — `plumb trust` records trust per workspace **root** in plumb's data dir (`config.DataDir()/trust.json`, never in the project, so a cloned repo cannot self-trust). Trust is bound to a hash of the trusted command set, so rewriting a trusted command re-prompts (closes the TOCTOU); a legacy boolean `trust.json` re-confirms once. `plumb trust` warns on any command matching the interpreter-with-inline-code pattern (`bash -c`, `python -c`, …). Default- and global-config commands always run.

### `agent_config_writes` — agent-writable config (opt-in)

```toml
agent_config_writes = false   # top-level; user-settable only, default off
```

When `true`, the `agent_config` tool may write a **small allowlist** of project-config keys on the user's behalf: the `[tasks.<lang>]` slots plus `log_level`, `ui.theme`, `ui.path_style`, `topology.exclude_patterns`, `quality.analysers`. The allowlist (`internal/config/fields_agent.go`) is the entire security model — every other key, **including `agent_config_writes` itself**, is never agent-writable (git tiers, workspace roots, `edits.strict`/`rate_limit`, `semantics.api_key`, session eviction, `log_file`, `lsp.*`). Writes go through the tool (never a raw `config.toml` edit): the whole batch is validated and applied atomically to project config, tagged `provenance=agent` in a `.plumb/config.provenance.json` sidecar (auto-gitignored), shown by `plumb config show`, and revertible with `plumb config unset <key>`. The enable knob is editable only by the user (TUI Settings); the agent cannot flip it.

### `[tools]` — client-aware tool profiles

```toml
[tools]
profile = "auto"            # auto | lean | full; PLUMB_TOOLS_PROFILE overrides

[tools.client_profiles]     # optional per-client override, keyed by clientInfo.name prefix
# claude-code = "full"
```

Controls **which tools appear in `tools/list`**, to spare a client that already has native filesystem tools from paying for the non-lean remainder (40 tools today, out of a 61-tool registry; the lean set keeps 21) as advertised schemas. `auto` (default) resolves to **lean** only for a client whose `internal/clientcaps` entry declares `ReliableDeferredToolDiscovery = true` — evidence-based, reviewed proof that its model can reliably discover and invoke a tool absent from its initial `tools/list` (a ToolSearch-style deferred mechanism), never an inference from native file/search/shell possession — and **full** for everything else: Claude Desktop, any unrecognised client, any schema-discovery-only client (Claude Code — see below), and, today, **Codex and Gemini CLI too** — their `internal/clientcaps` entries carry strong native file/search/shell access but leave `ReliableDeferredToolDiscovery` unset (`false`), pending integration coverage of their deferred-tool invocation behaviour. `lean`/`full` force the choice regardless of capability; project-overridable and `PLUMB_TOOLS_PROFILE`-overridable; resolution precedence is per-client override → `profile` → auto.

**The auto-mode decision and its reason are both inspectable.** `resolveToolProfile` (`internal/cli/conn_profile.go`) returns the resolved profile plus a stable, kebab-case reason, surfaced in both `session_start`'s orientation note and `daemon_info`: `client-override` (a `[tools.client_profiles]` entry fired), `explicit-config` (`[tools] profile` is set to `lean`/`full`), `unknown-deferred-discovery` (an unrecognised client — always full), `schema-discovery-only-client` (the `SchemaDiscoveryOnly` client — always full), `verified-deferred-discovery` (`ReliableDeferredToolDiscovery = true` — lean), and `unverified-deferred-discovery` (the flag is unset — full, the state every shipped client is in today). Promoting a client's capability entry to `ReliableDeferredToolDiscovery = true` is a reviewed, evidence-backed data change in `internal/clientcaps`, never a config toggle.

**Hidden ≠ unregistered.** A tool the lean profile hides is absent from `tools/list` but **stays callable by name** via `tools/call` — no capability is removed, and the resilient proxy never re-lists (it replays `initialize`). This escape hatch holds only for a client that can invoke a tool it was never advertised; a **schema-discovery-only** client (one that builds its tool list — and its tool-search/deferred list — solely from `tools/list`, e.g. Claude Code) cannot, so `auto` mode serves it **full** rather than lean (`SchemaDiscoveryOnly` in `internal/clientcaps`). The **lean set** (`internal/tools/profile.go` `LeanTools`, the single source of truth) keeps `session_start`, the read/edit/write/transaction file tools, `git`, `diagnostics`, the core LSP-semantic tools, the headline topology tools, `search_memories`, and `run_task`. The **mutation-lane rule** governs it: a read-only commodity tool may be hidden freely, but a mutation tool whose native fallback is unsafe (`mv`/`rm`/`sed` bypass plumb's per-path locks, the LSP notify, and the transaction WAL) stays lean; `read_file`/`read_symbol` stay lean too because the edit lane needs their mtime/sha headers. `run_task` is lean for the same reason: its only “native equivalent” is a raw shell `go test`/`zig build`, so hiding it just routes a recognised CLI client to the shell-build anti-pattern the profile exists to avoid. Under the lean profile `session_start` prints a one-line note with the hidden count and how to restore `full`. **Mid-session profile changes:** the server advertises the `tools.listChanged` capability and emits a `notifications/tools/list_changed` whenever a config reload changes the connection's resolved profile (e.g. a per-project `[tools]` override loaded at attach, or a hot-reloaded global setting). The resilient proxy forwards that server-initiated frame to the client unchanged, so a client that honours the notification re-lists and picks up the new profile mid-session; a client that lists only once still won't (its choice). The client identity that drives `auto` is known from the first list.

**The bootstrap set is the one exception no profile can hide.** `session_start`, `git`, `read_file`, and `edit_file` (`tools.BootstrapTools`, `internal/tools/profile.go`) are advertised in every connection's *initial* `tools/list` regardless of the resolved profile — the lean-hiding logic checks bootstrap membership before it ever consults `LeanTools`. `BootstrapTools` is a subset of `LeanTools` today (`TestBootstrapToolsAreLean` pins the containment), but the two sets answer different questions and are free to diverge: bootstrap is "what must always be visible for orientation", lean is "what a lean client keeps". Without this guarantee, a client whose first `tools/list` never advertised `session_start` would have no reliable way to discover the tool exists at all.

**Always-loaded (pinned) tools — Claude Code MCP tool search.** Claude Code defers MCP tool *schemas* by default (only tool names load at session start; the model must call `ToolSearch` to page a schema in before invoking it — otherwise it guesses parameter names and the call is rejected client-side, before it ever reaches plumb). plumb exempts its highest-frequency tools from that deferral by advertising them with `_meta["anthropic/alwaysLoad"] = true` in `tools/list` (`MetaAlwaysLoadKey`, `internal/mcp/server.go`; emitted in `handleToolsList` when `Server.AlwaysLoad` accepts the name, wired to `tools.IsLean` in `conn_register.go`). The pinned set is **exactly `LeanTools`** — one list serving double duty: the lean-profile visibility set *and* the never-deferred set. So the ~21 core tools load into context up front (no `ToolSearch` round-trip, no parameter guessing), while the long tail stays deferred to keep the context saving. Clients that predate the convention ignore the unknown `_meta` and are unaffected; no config knob is exposed (a per-machine override is `alwaysLoad: true` on the plumb server entry in the client's own MCP config).

## Client setup commands

`plumb setup` registers the current `plumb` binary as a stdio MCP server:

| Client | Command | Config target |
|---|---|---|
| Claude Desktop | `plumb setup claude-desktop` | Platform-specific Claude Desktop JSON config (macOS: `~/Library/Application Support/Claude/claude_desktop_config.json` — the one path Anthropic documents; also heuristically repoints any sibling `Claude*/claude_desktop_config.json` profile that already exists, e.g. `Claude-Personal`, the shape produced by the unofficial multi-account convention of a second `--user-data-dir` or a duplicated `.app` — not an Anthropic-documented mechanism, see `claudeDesktopExtraConfigPaths`) |
| Claude Code, user scope | `plumb setup claude-code` | `~/.claude.json` + `~/.claude/skills/` (skill files) |
| Claude Code, project scope | `plumb setup claude-code --project` | `.mcp.json` in the current directory + `~/.claude/skills/` |
| Codex | `plumb setup codex` | `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when unset |
| Gemini CLI | `plumb setup gemini` | `~/.gemini/settings.json` |
| Cursor | `plumb setup cursor` | `~/.cursor/mcp.json` (shared by the editor and the `cursor-agent` CLI) |
| Augment Code | `plumb setup augment` | `~/.augment/settings.json` (the `auggie` CLI) |
| Qwen Code | `plumb setup qwen` | `~/.qwen/settings.json` |
| Antigravity CLI | `plumb setup antigravity` | `~/.gemini/config/mcp_config.json` (the shared `{"mcpServers": {...}}` config Antigravity reads for both CLI and IDE; also repoints existing per-surface `~/.gemini/{antigravity-cli,antigravity-ide,antigravity}/mcp_config.json`) |
| Antigravity Desktop | `plumb setup antigravity-desktop` | same shared `~/.gemini/config/mcp_config.json` (Antigravity regenerates the per-server `mcp/` dirs from it) |
| OpenCode | `plumb setup opencode` | `~/.config/opencode/opencode.json` (`mcp` key; `type:"local"`, command array) |
| Crush | `plumb setup crush` | `~/.config/crush/crush.json` (`mcp` key; `type:"stdio"`) |
| Goose | `plumb setup goose` | `~/.config/goose/config.yaml` (`extensions` key; YAML) |
| Hermes | `plumb setup hermes` | `~/.hermes/config.yaml` (`mcp_servers` key; YAML) |

Setup helpers preserve existing MCP servers, back up config first, and resolve locations via OS/user-home helpers — no hardcoded paths. All clients funnel through one format-agnostic merge (`mergeServerEntry`) backed by JSON, TOML, or YAML serialisers; the trio Cursor/Augment/Qwen reuse the plain `mcpServers` shape, the rest carry a client-specific key/entry. (Aider is intentionally absent — it has no native MCP **client**, only third-party servers that wrap it.)

Two bulk flags on the bare `plumb setup` command (`runSetupAll`, `internal/cli/setup_clients.go`): `--all` **repoints** every already-registered client at the current binary — the idempotent repair `plumb doctor` recommends after the binary moves or is rebuilt; it never adds plumb to a client that lacked it. `--install-missing` additionally **registers** plumb in installed-but-unregistered clients (config file present, no plumb entry) — the one-shot first-time setup — but never fabricates a config for a client with no config file (an absent config is indistinguishable from an uninstalled client). Either flag triggers the bulk run; `refreshClientAt` classifies each config path as `not installed` / `not registered` / `already current` / `registered` / `updated`, and a bare `--all` that finds unregistered clients prints a hint pointing at `--install-missing`.

`plumb setup claude-code` also installs three idempotent user-scoped skills into `~/.claude/skills/`: `plumb-explore` (navigation), `plumb-refactor` (semantic rename, atomic cross-file edits), and `plumb-minimal-change` (prove reuse and minimality with plumb evidence before writing non-trivial code); `--no-skill` skips.

## Workspace detection

`workspacePool.Detect(dir)` walks up from `dir`:

1. **`.plumb/` marker** — explicit workspace. Returns `(dir, language)` if an LSP language is detectable here or in an ancestor; otherwise `(dir, "none")` (filesystem tools, stats, project config still work; LSP tools fail until a language attaches).
2. **A strong language root marker** (`go.mod`, `Cargo.toml`, `Package.swift`, `pyproject.toml`, …) at `dir` or any ancestor — returns `(dir, language)`.
3. **A `.git/` directory** — an unambiguous project boundary. Returns `(dir, "none")` so a multi-language repo with no language marker still resolves. `$HOME` is excluded; nearest-wins, so a `.plumb/` or language marker beats a `.git/` further up. **Content sniff (last resort):** before returning `"none"` at a `.git/` boundary (or resolving a `.plumb/` marker root), plumb scans that root — bounded, up to 2 levels deep, noise dirs pruned — for source files of an **active** language and, if one dominates, resolves that language instead (`extLangAt`, `pool_detect.go`). So a git repo full of `.py` files with **no** `pyproject.toml`/`setup.py` attaches Python when pyright is installed — matching the "install → on" philosophy for ecosystems with no mandatory manifest. It fires only after all strong/weak markers fail, is confined to the confirmed boundary (never ascends, never `$HOME`), and is gated on the language server being installed.

Walks to the parent otherwise; errors only after passing the filesystem root.

**Child-marker discovery (multi-language monorepo).** Detection only walks *up*. A `.plumb/` root that carries no language marker of its own — a Ghostty-style monorepo where the languages live one level down (`core/build.zig` + `app/Package.swift` under a bare `.plumb/` root) — would otherwise resolve as `LanguageNone` with nothing attached. So on a `LanguageNone` attach the daemon additionally descends up to `[workspace] child_scan_depth` levels (default 2) for **strong** language root markers in subdirectories (`discoverChildLanguages`, `pool_detect.go`; prunes dotdirs/`node_modules`/build outputs, stops at a matched root, never scans `$HOME`). Each discovered child language attaches its own server **rooted at the subdirectory** (`core/` for zig, `app/` for swift) via the existing multi-LSP-per-root machinery; one is elected the connection **primary** (go-first, then alphabetical), the rest attach lazily on first file. All discovered languages are listed in the `session_start` identity line (e.g. `Language: Swift, Zig`) and `workspace_sessions`, and `workspace_symbols` **fans out** across them. Discovery runs only when the root has no language of its own — a root with its own `go.mod` is untouched, with its child languages attaching lazily as before.

**Strong vs weak root markers.** Promiscuous markers — `package.json` (typescript), `index.html` (html) — are **weak** (`weak_root_markers`): they name the language only of the directory they sit in directly (the resolution dir, or a `.git/`/`.plumb/` boundary), **never** an ancestor. So a stray tooling `package.json` up the tree (a docs build, or a global `~/package.json`) cannot hijack a Go/Swift/Rust workspace as TypeScript; a real JS/TS project — `package.json` at its own root or `.git` boundary — still resolves. Strong markers always beat weak ones at the same directory. The ancestor walks are additionally bounded at `$HOME`: a stray marker in the home directory never captures a workspace beneath it.

**Automatic enablement (install → on).** Every language is `enabled = true` by default; the *effective* set is gated on the server binary being installed (`exec.LookPath`, cross-platform — honours `PATHEXT` on Windows). So installing `rust-analyzer` activates Rust for every Cargo project with no config; a language whose server is absent stays dormant at zero cost and its markers never enter detection. Set `[lsp.<lang>] enabled = false` to exclude a language even when its server is installed. `plumb config show` prints an `active` row per language (`yes (installed)` / `no (… not installed)` / `no (disabled in config)`); `plumb doctor` reports the same.

**Detection uses global config, not project config.** `Detect`/`detectLanguageAt` consult the daemon's resolved **global** language set, *before* any `<root>/.plumb/config.toml` loads. So a language enabled **only** in a subfolder's project config (e.g. `[lsp.html] enabled = true` in `site/.plumb/config.toml`) does **not** make that subfolder resolve as that language — enable it in **global** config. With multi-LSP-per-root (0.9.0) this rarely bites: enable the secondary globally and per-file routing sends each extension to the right server within one workspace, no subfolder pin needed.

`LanguageNone` (`"none"`) keeps non-Go/non-Python projects fully attached minus LSP; the `.git/` fallback extends this to any git repo, so a repo without a language marker resolves on the first path-bearing tool call. **Auto-attach** (opt-in, `[workspace].auto_attach`) covers the residual case — a seed dir with *no* `.git/` above — via `SynthesiseRoot`; synthetic sessions are marked `(auto)`, and `auto_attach_persist` creates `<root>/.plumb/` on first attach.

Cold-start resolution in `session_start`: the daemon's already-attached root → explicit `workspace` arg → `roots/list` query → the serve-proxy cwd hint → otherwise an honest "pass `workspace`" error. There is **no daemon-side `os.Getwd()` fallback** (the daemon is a singleton shared across connections) — but the per-conversation `plumb serve` proxy *is* cwd-aware and transports its working directory in the initialize `_meta` (`dev.plumbkit/workspace`) as an **advisory attach hint**: consulted only after client roots and the persisted pin, always validated through `Detect` (marker required, `$HOME` excluded), and never persisted as the sticky pin. So a client that reports no folder (Claude Desktop sends no `roots`) attaches automatically when its `plumb serve` was launched in the project directory; otherwise pin the project with an absolute `workspace`. Run `plumb init` to create a `.plumb/` marker.

**Forcing the primary language.** `session_start` also takes an optional `language` arg (the `[lsp.<lang>]` key) that forces the primary language server when detection cannot infer it — now rarely needed for Xcode since `*.xcodeproj`/`*.xcworkspace` are root markers; still useful when even those are absent (e.g. a loose `.swift` directory), where the workspace would otherwise be `LanguageNone` (per-file routing still attaches secondaries, but `workspace_symbols` and the hierarchies need a *primary*). It re-pins the current workspace (or pins alongside an explicit `workspace`) to that server and shows it in the identity line. The language must be active (installed + enabled); an unknown/uninstalled/disabled value is ignored and normal detection applies.

**Single-workspace-per-connection contract.** Once a connection has attached a workspace, every path-bearing tool refuses paths outside the connection's allowed roots with a `workspace boundary violation` error (the allowed set is the workspace plus `extra_roots` read-write and `read_roots` / Go dependency roots read-only). `rename_symbol` also boundary-checks each output URI before applying. To switch projects, call `session_start` with an explicit `workspace`: a deliberate `workspace` arg re-pins the connection (re-attaching LSP/topology/quality/config) rather than being refused — clients may reuse one `plumb serve` across chats, so a fresh chat is not a fresh connection. A connection that hits a violation is marked `Health: blocked` for the TUI. `git`'s `repo` arg defaults to the pinned workspace when omitted.

## Adapter validation status

> **Enablement note:** the `[lsp.<lang>] enabled = true` annotations in the rows below are historical — every language is now enabled by default and activates automatically when its server binary is on PATH (see *Automatic enablement* under Workspace detection). The knob you actually reach for is the opposite: `enabled = false` to exclude a language.

Real-binary validation has been exercised on macOS only; Linux integration runs in CI and is being hardened pre-v1; Windows is not yet supported. Each **Experimental** adapter has mock-transport unit tests passing and a real-binary integration test gated `//go:build integration`. The earlier "these servers fail `TestIntegration_DidChangeWatchedFiles` because they use pull diagnostics" hypothesis was **wrong**: the real cause was that plumb never advertised the `textDocument.publishDiagnostics` client capability, so a server that gates on it (typescript-language-server) published nothing. Declaring it (in `DefaultClientCapabilities`) fixed typescript-language-server 5.3.0 — its `TestIntegration_DidChangeWatchedFiles` now passes and the adapter is **validated**. The capability is shared by every adapter: a 2026-06-17 real-binary retest confirmed **zls** (0.16) now passes **both** integration tests too — it is **validated**. **kotlin-language-server** was also retested and still fails `TestIntegration_DidChangeWatchedFiles` (it needs a real Gradle/Maven project to publish diagnostics, not a bare temp workspace), so it stays experimental. (typescript-language-server does **not** implement pull diagnostics: `textDocument/diagnostic` returns -32601 and it advertises no `diagnosticProvider`.) The LSP 3.17 **pull** path (`textDocument/diagnostic`) is a first-class, per-language **negotiated** mode: `[lsp.<lang>] diagnostics = "pull"` advertises the pull client capability (`protocol.ClientCapabilitiesFor(true)`) for that connection, and `gopls` additionally gets its experimental `pullDiagnostics: true` initialization option (needed for it to answer pulls at all). `DefaultClientCapabilities` — what every connection gets under the default `auto`/`push` — still deliberately advertises **no** pull capability; pull is opt-in per connection, never the default. A real-binary retest under forced pull (2026-07-15, macOS arm64): **gopls v0.23.0** advertises `diagnosticProvider`, answers `textDocument/diagnostic` correctly, and keeps pushing `publishDiagnostics` too (resolves to **hybrid**) — single-document pull is roughly two orders of magnitude faster than waiting for a push (median ~3ms vs ~1s) — but it does not implement `workspace/diagnostic`, so a workspace-wide query still needs push. **typescript-language-server** (5.3.0) and **zls** (0.16) do not advertise `diagnosticProvider` under forced pull (zls answers with an empty report rather than erroring; typescript-language-server returns `-32601`), so requesting `pull` for either resolves to `pull-requested-but-unavailable` and behaves as push. `auto` (the default) resolves to **push** for every adapter; moving an adapter's auto policy to pull needs further real-binary evidence. See `docs/configuration.md`'s *Diagnostics mode* section for the full negotiation model, the four resolved-mode vocabulary strings (`push`/`pull`/`hybrid`/`pull-requested-but-unavailable`), and where each is surfaced (`plumb doctor`, `lsp-status`, `daemon_info`, `session_start`).

**Multiple servers per root.** A workspace root may bind more than one language server at once (e.g. Go + HTML for a web app). The pool is keyed by `(root, language)`; each file routes to the server that owns its extension (`langsupport.ByPath`). The **primary** language (the one `Detect` resolves from root markers — `go.mod` beats `index.html`, see Workspace detection) is pinned on attach; **secondaries** start lazily on the first file of their language and live to daemon shutdown. `diagnostics` aggregates across a root's servers; the call/type hierarchies are URI-bearing and route per-file. `workspace_symbols` consults the primary for a single-language root, but **fans out** across all servers for a multi-language monorepo root (the child-marker discovery case above), merging and deduplicating results — warming any lazily-attached child on its first such query. The TUI sessions view and `workspace_sessions` list every active adapter.

| Adapter | Status |
|---|---|
| `gopls` | **Validated** — mock-transport unit tests + real-binary integration; `client/registerCapability` answered, `workspace/didChangeWatchedFiles` confirmed. |
| `pyright` | **Validated** — same coverage as gopls, against the real pyright-langserver binary. |
| `jdtls` | **Validated** (experimental; `[lsp.java] enabled = true`; needs Java 21+ and jdtls on PATH). Sends string request IDs, so the conn uses `json.RawMessage` for IDs. **Gotcha:** needs *both* `DidChangeWatchedFiles` and `DidOpen` (sent after the server's `ServiceReady`) for reliable diagnostics after external writes. |
| `rust-analyzer` | **Validated** — `[lsp.rust] enabled = true`. **Cold-start warning:** loads the sysroot + runs `cargo metadata` on first attach — minutes on a large workspace; the topology fallback keeps Rust symbol queries working while it warms. |
| `sourcekit-lsp` | **Validated** — ships with the Swift toolchain / Xcode; `[lsp.swift] enabled = true`; root markers `Package.swift`, `*.xcodeproj`, `*.xcworkspace`. SwiftPM supplies its own build plan. Bare Xcode roots may use default-off `[xcode] auto_build_server = true`: after mandatory workspace trust, Plumb safely validates one marker/scheme, generates `buildServer.json`, and restarts only the root's Swift pool entry. Configuration, build data, warming, and semantic proof are reported separately; Plumb never builds the project automatically. Real-Xcode integration covers cold configuration, an explicit test-only build, restart, symbols, definitions, references, and concurrent-attach singleflight. Structural Swift (`file_outline`, topology) remains independent. |
| `zls` | **Validated** — `[lsp.zig] enabled = true`; root markers `build.zig`/`build.zig.zon`. zls + tree-sitter-zig track the Zig language version (pre-1.0). Real-binary retest (2026-06-17, zls 0.16): passes **both** `TestIntegration_DocumentSymbols` and `TestIntegration_DidChangeWatchedFiles` (push diagnostics arrive once the `publishDiagnostics` client capability is advertised — the earlier pull-diagnostics hypothesis was wrong). |
| `typescript-language-server` | **Validated** — `[lsp.typescript] enabled = true`; root markers `tsconfig.json`/`jsconfig.json`/`package.json`. Serves **both** TypeScript and JavaScript. Real-binary integration (5.3.0) passes both `TestIntegration_DocumentSymbols` and `TestIntegration_DidChangeWatchedFiles`. **Gotcha:** publishes nothing unless the client advertises `textDocument.publishDiagnostics` (now in `DefaultClientCapabilities`); it does not implement pull diagnostics. |
| `kotlin-language-server` | **Experimental** — `[lsp.kotlin] enabled = true`; root markers `settings.gradle.kts`/`build.gradle.kts` (overlaps Java's — with both enabled, alphabetical detect order makes Java win). Real-binary retest (2026-06-10): passes `TestIntegration_DocumentSymbols`, fails `TestIntegration_DidChangeWatchedFiles` (needs a real Gradle/Maven project to publish diagnostics). |
| `vscode-html-language-server` | **Experimental** — `[lsp.html] enabled = true`; root marker `index.html` (only consulted while html is enabled). Serves document symbols, hover, completion, embedded-CSS/JS validation; does **not** implement workspace/symbol, call hierarchy, or type hierarchy. |

## Contributor recipes

Step-by-step procedures live as project skills in `.claude/skills/` (plain markdown, readable from any client): `add-lsp-adapter` (adapter checklist + promotion rule; full guide in `docs/adding-an-lsp.md`) and `add-mcp-tool` (tool checklist incl. the thin-orchestrator `Execute()` pattern and the lean-profile decision).

## Available tools (61)

Concise index only. Full behaviour, schemas, and per-tool steering live in each tool's MCP description (`tools/list`); sources are `internal/tools/<name>.go`.

- **Bootstrap:** `session_start` is the first call; it returns workspace, language, branch, recent context, tool stats, diagnostics, git policy, memories, and client-specific guidance.
- **LSP queries:** `find_symbol`, `workspace_symbols`, `get_definition`, `explain_symbol`, `list_symbols`, `file_outline`, `find_references`, `call_hierarchy`, `type_hierarchy`, `diagnostics`.
- **LSP edits:** `rename_symbol`, `replace_symbol_body`, `insert_before_symbol`, `insert_after_symbol`, `safe_delete_symbol`, `move_symbol` (relocate a whole declaration between two files in the same directory/package, atomically — refuses cross-package moves it cannot rewrite references for); these are semantic operations, distinct from file moves/copies, and support `include_doc_comment` where relevant.
- **Filesystem reads:** `read_file`, `read_symbol`, `read_multiple_files`, `list_directory`, `list_files`, `find_files`, `search_in_files`, `file_status`. Reads are bounded, binary-safe, `.gitignore`-aware where applicable, and return mtime/sha headers for optimistic edits. `file_status` is a content-free probe reporting per-path `git_dirty` / `changed_since_plumb_wrote` / `last_writer` / mtime / size.
- **Filesystem writes:** `write_file`, `edit_file`, `delete_file`, `rename_file`, `copy_file`, `transaction_apply`, `undo_edit`. Writes take `WriteDeps`, hold per-path locks, respect dirty-file checks, notify LSP, invalidate caches, and consume the write-rate budget. `undo_edit` safely reverts plumb's most recent write to a file (its own change only, refusing if the file changed since), the safe alternative to a whole-file `git checkout`.
- **Search/replace and git:** `find_replace` is dry-run by default; prefer `rename_symbol` for identifiers. `git` is tiered by policy (read/write/destructive/network), with typed `add`/`commit` and confirmation for dangerous tiers.
- **Other utilities:** `git_init`, `file_diff`, `version`, `daemon_info`, `rename_session`, `workspace_sessions`.
- **Cross-agent sharing (`[collab]`, opt-in):** `share_intent` broadcasts what you are working on (optionally scoped to `path_globs`); `leave_note` leaves a message for a named peer session or for `next` (whoever attaches next); `share_findings` hands off what you have learned as a durable generated memory on demand (riding the episodic pipeline: redact → provenance → `.plumb/memories/` → FTS index → `generated_memory_keep` retention), instantly discoverable by peers via `search_memories`/`workspace_search`/`relevant_memories`/hints/`session_start`. All three are advisory (never block a write), secret-scrubbed, and per-workspace; `share_intent`/`leave_note` render to peers as unverified claims (delivered by polling only plus an intent-aware write hint), `share_findings` writes lower-confidence agent-generated content. Gated on `[collab] intents` / `mailbox` / `knowledge_handoff` (default off); intents/notes expire per `intent_ttl_minutes` in `<workspace>/.plumb/collab.db` (gitignored). See the `[collab]` config section.
- **Tasks, commands & config:** `run_task` runs a stored `[tasks.<lang>]` command (build/lint/test/e2e/verify; verify = build then test) — no shell, bounded, with a per-workspace trust gate (`plumb trust`) for project-supplied commands. `run_command` runs a named entry from the `[[command]]` allow-list (fixed argv + one `{target}`) under an OS sandbox — the safe, injection-proof way to run workspace commands; a project entry needs `plumb trust`. `execute_shell_command` runs an ad-hoc `sh -c` command (pipes/redirects work) and is **disabled by default** — enable it with `[commands] allow_shell` (global, or project + `plumb trust`); it too runs under the sandbox, which is integrity-only (it confines writes, not reads/env) and **denies the network by default** (`[commands] deny_network`, so the reply reports `network=off` — flip it if a command needs egress). See the `[[command]]` / `[commands]` config section. `agent_config` reads (`describe`) and, only when the user enabled `[agent_config_writes]`, writes (`set`) a small allowlist of config keys — validated atomically, `provenance=agent`, revertible via `plumb config unset`. Guardrails (git tiers, roots, strict mode, API keys, the enable knob) are never agent-writable. See the `[tasks.<lang>]` and `agent_config_writes` config sections above.
- **Topology:** `topology_status`, `topology_search`, `topology_explore`, `topology_impact`, `topology_affected`, `topology_routes`, `structural_query` use the SQLite/FTS5 index at `<workspace>/.plumb/topology.db`.
- **Ranked discovery:** `workspace_search` is the broker over the indexed corpora — code and docs via topology FTS, memories via the memory FTS index — interleaved by per-corpus rank and labelled with `corpus`/`source`/`field`/`score`/`why` plus per-corpus index freshness; `exact_match=false` always (it is discovery, never proof of absence — the exact lane stays `search_in_files`). Discovery ladder: `workspace_search` → topology/LSP → `search_in_files` → bounded `read_file`.
- **Memory:** `list_memories`, `read_memory`, `write_memory`, `delete_memory`, `search_memories`, `relevant_memories` operate on per-workspace markdown memories under `<workspace>/.plumb/memories/`. `search_memories` is FTS5-ranked when the index is fresh (grep fallback otherwise; `mode` = auto/fts/grep); `read_memory` shows a provenance footer for generated memories; writes/deletes keep the index current. See `[memory]` config.

## TUI conventions (Bubble Tea v2)

- Import paths are **v2 only**: `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`. Never mix in the v1 packages — type/API incompatibilities.
- `Model` is exported; `NewModel(logPath)` constructs, `Run(logPath)` is the entry point. `View()` returns `tea.View` (`v.AltScreen = true`). Keys: `tea.KeyPressMsg`, match via `msg.String()`.
- Sections (opened with `/`): `Dashboard`, `Sessions`, `Memory`, `Logs`, `Settings` (0–4). Settings (`internal/tui/model_settings.go`) is a two-pane editor: a left **Scope** column (Global + each workspace) and a right rows pane with **General**/**LSP** tabs; `tab`/`shift+tab` cycle focus, `[`/`]` resize.
- Settings persistence is scope-aware: Global rows write global config (`config.Save`), workspace rows write a sparse override to `.plumb/config.toml`. Each row carries a reload-tier numeral (`¹` live / `²` next session / `³` restart) and a `⁴` override / `⁵` inherited mark; only **Theme** and **Log level** apply live. `ctrl+t` opens the theme picker.
- List and `[lsp.<lang>]` rows open shared pop-up editors (`model_settings_listeditor.go`, `model_settings_texteditor.go`), auto-saving on close; overlays dim via `dimLines()` + `spliceOverlay()`.
- **Rebindable keys:** the twelve navigation/action keys are configurable via the global-only `[ui.keys]` table (unknown actions and key conflicts are warned at startup with deterministic resolution; overlay/popup keys and the vim aliases stay fixed). The help overlay and Sessions footer render the live bindings. See the `[ui.keys]` config section.
- **Theme system:** `ActiveTheme`/`ActiveThemeName` are globals in `internal/tui/theme.go`; lipgloss style vars are rebuilt by `RebuildStyles()` after any mutation. Adding a `Theme` field means updating every theme literal — `TestTheme_AllFieldsSet` catches omissions.

## Code style rules

- **Australian English** in all prose: docs, comments, log messages, error strings. Use -ise/-isation, behaviour, colour, honour, favour. **Exception:** identifiers from external specs keep their canonical spelling — LSP method names (`initialize`, `publishDiagnostics`), MCP protocol fields, Go standard library names.
- **`gofumpt`** on save. `golangci-lint` v2.12.2 before every commit; CI enforces.
- **`log/slog`** exclusively. Never `log` package or `fmt.Println` for logging.
- **Errors wrap context:** `fmt.Errorf("loading config: %w", err)`.
- **Context everywhere:** every blocking/I/O operation takes `context.Context` first.
- **Concurrency contract** stated in doc comments on every type.
- **No `init()` doing real work.** Wire dependencies in constructors.
- **No globals** except package-level style vars in `internal/tui/styles.go` and the `pathLocks` map in `internal/tools/file_write_helpers.go` (process-global by design).
- **Max ~600 lines per file.** Split if it grows. Exception allowlist: `internal/lsp/protocol/types.go`. No other file qualifies without explicit justification added here.
- **Comments only when the WHY is non-obvious.** No what-comments.
- **Gocyclo-15 contract.** No first-party non-test function may exceed cyclomatic complexity 15. CI enforces.

## Testing requirements

- Tests live next to the code (`_test.go` in the same package); table-driven where the shape fits.
- `internal/lsp/`, `internal/cache/`, `internal/tools/` require meaningful coverage. For write tools, `WriteDeps{}` is the zero-value setup. Per-session isolation tests belong in the package they test.
- The MCP parameter-alias engine (`internal/mcp/argguard.go`) resolves alias names only at the dispatch boundary, so an internal tool→tool `Execute` call (e.g. `read_multiple_files` composing args for `read_file`) must use the target's canonical parameter names — guarded by `internal/tools/inprocess_call_guard_test.go`.
- Do not chase TUI coverage.
- Integration tests requiring external binaries (gopls, pyright) must be gated with `//go:build integration`.

## Versioning

Version is injected at build time: `-X .../internal/cli.Version=<version>` (defaults to `"dev"`); the Makefile resolves it from the exact git tag → `VERSION` file → short commit hash. To bump during development, edit `VERSION`; do not tag every iteration.

The daemon writes its build version to `~/Library/Caches/plumb/plumb.version`; `plumb serve` warns on mismatch. **If you've just rebuilt, restart the daemon** — new code never activates against the old process. `plumb restart` brings a fresh daemon straight back up (the resilient proxy reconnects clients); `--force` skips the confirmation prompt.

## Commit conventions

```
<type>(<scope>): <short summary>

[optional body: why, not what]
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`. Prefer one commit per discrete change with a `CHANGELOG.md` entry — bisectable history > squashed PRs.

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
make install-clients     # install the MCP client CLIs (gemini, codex, qwen, …) the clientsmoke harness drives
make clients-test        # on-demand: each installed client CLI completes an MCP handshake with plumb (no API keys)
make clients-test-auth   # on-demand: drive each client headless to force a real plumb tool call (needs API keys)
```

The two `clients-test*` targets are on-demand (own build tags, never in `make verify` beyond a compile check) and drive real client CLIs non-interactively.

**`make install-hooks` is required after every fresh clone** — the pre-commit hook runs `golangci-lint run --fix ./...`. **Formatting note:** apply formatting via `golangci-lint run --fix ./...`, never the standalone `gofumpt -w` binary — the two can pin different versions and produce phantom lint failures.

## Known limitations and pending work

Take particular care before adding a feature that touches concurrency, the rate limiter, the read tracker, or the stats schema — these areas carry subtle invariants. Land each discrete change with its own `CHANGELOG.md` entry.

## Quick reference for agents

- **First call:** `session_start({})` for orientation, live git policy, diagnostics, memories, and client-specific guidance.
- **Stay in the plumb lane:** after a plumb read, edit with plumb `edit_file`/`write_file`, not a native client edit tool; read-state is tracked separately.
- **Read before edit:** use `read_file`, then pass its `mtime` or `sha256` header to `edit_file.expected_mtime`/`expected_sha`. Required in strict mode, recommended always.
- **Common file ops:** `write_file({file_path, content})`, `edit_file({file_path, edits: [{old_string, new_string}], expected_mtime})`, `transaction_apply({operations: [...]})`, `rename_file`/`copy_file` (`{from, to}`), `delete_file` (`allow_dir: true` only for empty directories).
- **Common rejections:** "has not been read" means strict mode or native/plumb lane mixing; "uncommitted changes" means the file was dirty before this session, so commit it or pass `dirty_ok: true`; throttling means wait or adjust `PLUMB_WRITE_RATE_LIMIT`.
- **Useful checks:** `git({subcommand: "status"})`, `git({subcommand: "log", args: ["-10", "--oneline"]})`, `plumb log-level warn/reset`, and `plumb config show --workspace .`.
