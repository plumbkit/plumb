# Plumb — Agent Instructions

> Source of truth: edit `AGENTS.md` only.
>
> `CLAUDE.md` and `GEMINI.md` are repository symlinks to this file for client compatibility; Codex and ChatGPT read `AGENTS.md` directly. Do not replace, unlink, rewrite, or edit those symlink paths directly. If an instruction change is needed, update `AGENTS.md`; the linked files will reflect it automatically.
>
> These agent-context files are tracked in git to ensure a consistent, high-quality experience for AI assistants.

This file is the canonical brief for AI agents working in the plumb codebase. Keep it accurate; it ages fast.

> **CRITICAL — tool priority:** Always use plumb MCP tools for all tasks when plumb is present and the required capability is available through plumb. Do not fall back to native tools (Read, Edit, Bash, shell commands, etc.) for file reads, writes, edits, searches, symbol lookups, or git queries when the equivalent plumb tool exists. Plumb tools are LSP-aware, concurrency-safe, and session-tracked; native tools bypass all of that. The only exceptions are tasks plumb explicitly does not cover (e.g. running tests, compiling, interacting with external services).

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
| `internal/stats/` | Global SQLite tool-call statistics, row-scoped by workspace and session (WAL, P95, client-aware). Writes funnel through one batched-transaction `Writer` (single-writer goroutine; non-blocking enqueue, never on the response path); reads use a process-cached `SharedReadOnly` handle. Also holds the `episodic_memories` table (schema v8) for idle-session summaries |
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

**Memory bounds & introspection.** At startup the daemon applies a *soft* heap limit via `debug.SetMemoryLimit` (`internal/cli/memlimit.go`): `PLUMB_MEMORY_LIMIT` (a byte size like `1500MiB`, or `0`/`off`/`unlimited` to disable) overrides a generous 4 GiB anti-OOM backstop default — Go GCs harder near the limit and never hard-fails, so a transient spike is bounded. The active limit is logged. Three admin commands over `plumb.ctrl.sock` expose live state: `plumb debug mem` prints a `runtime.ReadMemStats` snapshot (`HeapAlloc`/`HeapInuse`/`HeapSys`/`HeapReleased`/`NumGC`/`Goroutines`), `plumb debug heap` forces a GC and writes a `runtime/pprof` heap profile to the cache dir (`plumb.heap.<ns>.pprof`) for `go tool pprof`, and `plumb debug stacks` writes a full goroutine stack dump (`plumb.stacks.<ns>.txt`, the pprof `goroutine` profile at `debug=2` — the non-destructive `SIGQUIT` equivalent) for diagnosing a live hang. A full topology resync ends with `debug.FreeOSMemory()` so the large transient working set returns to the OS rather than lingering as idle heap spans. Note: the TUI daemon widget's RSS row is the *current* sample, not a peak.

**Singleton enforcement** (`internal/cli/lock.go`): the two `flock`s above serialise `plumb serve`'s spawn decision and keep `plumb daemon` a singleton (a second daemon exits on `EWOULDBLOCK`); both release on process exit.

**Resilient proxy** (`internal/cli/serve_proxy*.go`): `plumb serve` is a frame-aware reconnecting proxy that survives a daemon crash or hang without the client noticing. On a daemon failure it keeps the client's stdio open, dial-or-spawns a fresh daemon, and **replays the captured MCP handshake** (the client only sends `initialize` once). In-flight requests get a synthesised retryable error (`code -32000`) instead of hanging; non-idempotent writes are never auto-replayed. A *hung* daemon is caught by an idle `ping` heartbeat, then `SIGTERM`→`SIGKILL`'d and respawned. Reconnects are bounded. Knobs: `PLUMB_PROXY_RECONNECT` (default on; off ⇒ legacy `io.Copy` proxy), `PLUMB_PROXY_HEARTBEAT` (`0` disables hang detection), `plumb serve --no-reconnect`.

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
strict = false                # require read_file (matching mtime) before edit_file; per-session
rate_limit_per_minute = 120   # sliding-window cap per session; 0 disables. A shared parent budget (keyed by (client, workspace)) caps combined rate across connections from the same client to one project
show_write_diff = true        # append a unified diff to edit_file/write_file responses
post_write_diagnostics_ms = 300 # ceiling on the wait for the LSP to re-publish diagnostics after a write; adapts down to observed latency; 0 disables
```

Env: `PLUMB_STRICT_EDITS`, `PLUMB_WRITE_RATE_LIMIT`, `PLUMB_SHOW_WRITE_DIFF`, `PLUMB_POST_WRITE_DIAG_MS`.

### `[workspace]` — root detection fallback + path-access roots

```toml
[workspace]
auto_attach = false           # fall back to SynthesiseRoot (nearest .git/ or seed) when no marker found; LSP unavailable
auto_attach_persist = false   # create .plumb/ at the synthetic root on first attach (implies auto_attach)
allow_dependency_reads = true # read/search may reach the Go module cache + GOROOT read-only; writes there refused
extra_roots = []              # additional read-WRITE dirs, additive ($VAR-expanded)
read_roots = []               # additional read-ONLY dirs, additive ($VAR-expanded)
```

The workspace boundary is enforced per-connection by a **`PathPolicy`** (`internal/tools/pathpolicy.go`): an allowlist of roots tagged read-only or read-write. The detected workspace is always read-write; `extra_roots` add read-write roots; `read_roots` (and, for a Go session with `allow_dependency_reads`, the module cache + `GOROOT`) add read-only roots. Read/search tools admit any allowed root; write tools demand read-write, so a write outside the workspace is refused by construction.

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

Built-ins: `nordico`, `darcula`, `dracula`, `gruvbox`, `plumb` (dark); `github-light`, `solarized-light`, `plumb-light` (light). The `plumb`/`plumb-light` pair is derived from the project website's own terracotta/sage palette (`site/index.html`). Written live by the theme picker, read at startup; `config.Save` rewrites the whole file, so user TOML comments are lost on first save. Project config ignores `[ui]`.

### `[lsp_query]` — LSP tool-call timeout

```toml
[lsp_query]
timeout = "30s"   # PLUMB_LSP_QUERY_TIMEOUT — cap on a single LSP tool call; "0s" disables
```

Top-level section (distinct from per-language `[lsp.<lang>]` tables). Applied at the tool layer (`withLSPDeadline`) and a no-op when the context already carries a deadline, so the cold-start handshake is never shortened.

**LSP → topology fallback:** on LSP error/timeout, `find_symbol`, `workspace_symbols`, and `list_symbols` fall back to the topology index (when enabled), annotated `source=topology, mode=indexed-approximate`; a no-op when topology is disabled or has no match. Position/semantic tools (`get_definition`, `find_references`, hierarchies, `rename_symbol`) have no equivalent and surface the error unchanged.

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
idle_threshold_minutes = 30   # mark a session idle in the TUI after this long with no tool call
eviction_ttl_minutes   = 60   # daemon force-closes a connection idle this long; 0 disables eviction
```

Global or per-project; no env override. `idle_threshold_minutes` is cosmetic (a `~` marker in the TUI). `eviction_ttl_minutes` has teeth: a daemon-side reaper (5-min tick) cancels a connection whose last tool call was longer ago than the TTL, reclaiming a `plumb serve` whose agent silently disconnected. Read live; `0` disables. The activity signal is a tool call (`LastSeenAt` = session file mtime).

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

## Client setup commands

`plumb setup` registers the current `plumb` binary as a stdio MCP server:

| Client | Command | Config target |
|---|---|---|
| Claude Desktop | `plumb setup claude-desktop` | Platform-specific Claude Desktop JSON config |
| Claude Code, user scope | `plumb setup claude-code` | `~/.claude.json` + `~/.claude/skills/` (skill files) |
| Claude Code, project scope | `plumb setup claude-code --project` | `.mcp.json` in the current directory + `~/.claude/skills/` |
| Codex | `plumb setup codex` | `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when unset |
| Gemini CLI | `plumb setup gemini` | `~/.gemini/settings.json` |
| Cursor | `plumb setup cursor` | `~/.cursor/mcp.json` (shared by the editor and the `cursor-agent` CLI) |
| Augment Code | `plumb setup augment` | `~/.augment/settings.json` (the `auggie` CLI) |
| Qwen Code | `plumb setup qwen` | `~/.qwen/settings.json` |
| OpenCode | `plumb setup opencode` | `~/.config/opencode/opencode.json` (`mcp` key; `type:"local"`, command array) |
| Crush | `plumb setup crush` | `~/.config/crush/crush.json` (`mcp` key; `type:"stdio"`) |
| Goose | `plumb setup goose` | `~/.config/goose/config.yaml` (`extensions` key; YAML) |
| Hermes | `plumb setup hermes` | `~/.hermes/config.yaml` (`mcp_servers` key; YAML) |

Setup helpers preserve existing MCP servers, back up config first, and resolve locations via OS/user-home helpers — no hardcoded paths. All clients funnel through one format-agnostic merge (`mergeServerEntry`) backed by JSON, TOML, or YAML serialisers; the trio Cursor/Augment/Qwen reuse the plain `mcpServers` shape, the rest carry a client-specific key/entry. (Aider is intentionally absent — it has no native MCP **client**, only third-party servers that wrap it.)

`plumb setup claude-code` also installs two idempotent user-scoped skills into `~/.claude/skills/`: `plumb-explore` (navigation) and `plumb-refactor` (semantic rename, atomic cross-file edits); `--no-skill` skips.

## Workspace detection

`workspacePool.Detect(dir)` walks up from `dir`:

1. **`.plumb/` marker** — explicit workspace. Returns `(dir, language)` if an LSP language is detectable here or in an ancestor; otherwise `(dir, "none")` (filesystem tools, stats, project config still work; LSP tools fail until a language attaches).
2. **A strong language root marker** (`go.mod`, `Cargo.toml`, `Package.swift`, `pyproject.toml`, …) at `dir` or any ancestor — returns `(dir, language)`.
3. **A `.git/` directory** — an unambiguous project boundary. Returns `(dir, "none")` so a multi-language repo with no language marker still resolves. `$HOME` is excluded; nearest-wins, so a `.plumb/` or language marker beats a `.git/` further up.

Walks to the parent otherwise; errors only after passing the filesystem root.

**Strong vs weak root markers.** Promiscuous markers — `package.json` (typescript), `index.html` (html) — are **weak** (`weak_root_markers`): they name the language only of the directory they sit in directly (the resolution dir, or a `.git/`/`.plumb/` boundary), **never** an ancestor. So a stray tooling `package.json` up the tree (a docs build, or a global `~/package.json`) cannot hijack a Go/Swift/Rust workspace as TypeScript; a real JS/TS project — `package.json` at its own root or `.git` boundary — still resolves. Strong markers always beat weak ones at the same directory. The ancestor walks are additionally bounded at `$HOME`: a stray marker in the home directory never captures a workspace beneath it.

**Automatic enablement (install → on).** Every language is `enabled = true` by default; the *effective* set is gated on the server binary being installed (`exec.LookPath`, cross-platform — honours `PATHEXT` on Windows). So installing `rust-analyzer` activates Rust for every Cargo project with no config; a language whose server is absent stays dormant at zero cost and its markers never enter detection. Set `[lsp.<lang>] enabled = false` to exclude a language even when its server is installed. `plumb config show` prints an `active` row per language (`yes (installed)` / `no (… not installed)` / `no (disabled in config)`); `plumb doctor` reports the same.

**Detection uses global config, not project config.** `Detect`/`detectLanguageAt` consult the daemon's resolved **global** language set, *before* any `<root>/.plumb/config.toml` loads. So a language enabled **only** in a subfolder's project config (e.g. `[lsp.html] enabled = true` in `site/.plumb/config.toml`) does **not** make that subfolder resolve as that language — enable it in **global** config. With multi-LSP-per-root (0.9.0) this rarely bites: enable the secondary globally and per-file routing sends each extension to the right server within one workspace, no subfolder pin needed.

`LanguageNone` (`"none"`) keeps non-Go/non-Python projects fully attached minus LSP; the `.git/` fallback extends this to any git repo, so a repo without a language marker resolves on the first path-bearing tool call. **Auto-attach** (opt-in, `[workspace].auto_attach`) covers the residual case — a seed dir with *no* `.git/` above — via `SynthesiseRoot`; synthetic sessions are marked `(auto)`, and `auto_attach_persist` creates `<root>/.plumb/` on first attach.

Cold-start resolution in `session_start`: the daemon's already-attached root → explicit `workspace` arg → `roots/list` query → otherwise an honest "pass `workspace`" error. There is **no `os.Getwd()` fallback** (the daemon is a singleton shared across connections). Clients that don't report a folder (Claude Desktop sends no `roots`) must pin the project with an absolute `workspace`. Run `plumb init` to create a `.plumb/` marker.

**Forcing the primary language.** `session_start` also takes an optional `language` arg (the `[lsp.<lang>]` key) that forces the primary language server when detection cannot infer it — now rarely needed for Xcode since `*.xcodeproj`/`*.xcworkspace` are root markers; still useful when even those are absent (e.g. a loose `.swift` directory), where the workspace would otherwise be `LanguageNone` (per-file routing still attaches secondaries, but `workspace_symbols` and the hierarchies need a *primary*). It re-pins the current workspace (or pins alongside an explicit `workspace`) to that server and shows it in the identity line. The language must be active (installed + enabled); an unknown/uninstalled/disabled value is ignored and normal detection applies.

**Single-workspace-per-connection contract.** Once a connection has attached a workspace, every path-bearing tool refuses paths outside the connection's allowed roots with a `workspace boundary violation` error (the allowed set is the workspace plus `extra_roots` read-write and `read_roots` / Go dependency roots read-only). `rename_symbol` also boundary-checks each output URI before applying. To switch projects, call `session_start` with an explicit `workspace`: a deliberate `workspace` arg re-pins the connection (re-attaching LSP/topology/quality/config) rather than being refused — clients may reuse one `plumb serve` across chats, so a fresh chat is not a fresh connection. A connection that hits a violation is marked `Health: blocked` for the TUI. `git`'s `repo` arg defaults to the pinned workspace when omitted.

## Adapter validation status

> **Enablement note:** the `[lsp.<lang>] enabled = true` annotations in the rows below are historical — every language is now enabled by default and activates automatically when its server binary is on PATH (see *Automatic enablement* under Workspace detection). The knob you actually reach for is the opposite: `enabled = false` to exclude a language.

Real-binary validation has been exercised on macOS only; Linux integration runs in CI and is being hardened pre-v1; Windows is not yet supported. Each **Experimental** adapter has mock-transport unit tests passing and a real-binary integration test gated `//go:build integration`. For zls, typescript-language-server, and kotlin-language-server those gated tests were re-run on 2026-06-10 with the binaries installed: `TestIntegration_DocumentSymbols` passes but `TestIntegration_DidChangeWatchedFiles` fails (no `publishDiagnostics` in a bare temp workspace; zls and typescript-language-server appear to use pull diagnostics, and kotlin-language-server needs a real Gradle/Maven project), so all three stay experimental until pull-diagnostics support lands.

**Multiple servers per root.** A workspace root may bind more than one language server at once (e.g. Go + HTML for a web app). The pool is keyed by `(root, language)`; each file routes to the server that owns its extension (`langsupport.ByPath`). The **primary** language (the one `Detect` resolves from root markers — `go.mod` beats `index.html`, see Workspace detection) is pinned on attach; **secondaries** start lazily on the first file of their language and live to daemon shutdown. `diagnostics` aggregates across a root's servers; `workspace_symbols` and the call/type hierarchies consult the primary only. The TUI sessions view and `workspace_sessions` list every active adapter.

| Adapter | Status |
|---|---|
| `gopls` | **Validated** — mock-transport unit tests + real-binary integration; `client/registerCapability` answered, `workspace/didChangeWatchedFiles` confirmed. |
| `pyright` | **Validated** — same coverage as gopls, against the real pyright-langserver binary. |
| `jdtls` | **Validated** (experimental; `[lsp.java] enabled = true`; needs Java 21+ and jdtls on PATH). Sends string request IDs, so the conn uses `json.RawMessage` for IDs. **Gotcha:** needs *both* `DidChangeWatchedFiles` and `DidOpen` (sent after the server's `ServiceReady`) for reliable diagnostics after external writes. |
| `rust-analyzer` | **Validated** — `[lsp.rust] enabled = true`. **Cold-start warning:** loads the sysroot + runs `cargo metadata` on first attach — minutes on a large workspace; the topology fallback keeps Rust symbol queries working while it warms. |
| `sourcekit-lsp` | **Validated** — ships with the Swift toolchain / Xcode; `[lsp.swift] enabled = true`; root markers `Package.swift`, `*.xcodeproj`, `*.xcworkspace` (the latter two glob-matched, so Xcode apps with no SwiftPM manifest detect as Swift). Derives per-file compiler args from the SwiftPM build plan; for a bare `.xcodeproj` it attaches but its cross-file index (`workspace_symbols`/`find_references`/`get_definition`) is empty until a Build Server Protocol config (`buildServer.json` via `xcode-build-server`) supplies compile args. Structural Swift (`file_outline`, topology) uses the canonical-grammar WASM extractor and is unaffected. |
| `zls` | **Experimental** — `[lsp.zig] enabled = true`; root markers `build.zig`/`build.zig.zon`. zls + tree-sitter-zig track the Zig language version (pre-1.0). Real-binary retest (2026-06-10): passes `TestIntegration_DocumentSymbols`, fails `TestIntegration_DidChangeWatchedFiles` (likely pull diagnostics). |
| `typescript-language-server` | **Experimental** — `[lsp.typescript] enabled = true`; root markers `tsconfig.json`/`jsconfig.json`/`package.json`. Serves **both** TypeScript and JavaScript. Real-binary retest (2026-06-10): passes `TestIntegration_DocumentSymbols`, fails `TestIntegration_DidChangeWatchedFiles` on 5.3.0 (likely pull diagnostics). |
| `kotlin-language-server` | **Experimental** — `[lsp.kotlin] enabled = true`; root markers `settings.gradle.kts`/`build.gradle.kts` (overlaps Java's — with both enabled, alphabetical detect order makes Java win). Real-binary retest (2026-06-10): passes `TestIntegration_DocumentSymbols`, fails `TestIntegration_DidChangeWatchedFiles` (needs a real Gradle/Maven project to publish diagnostics). |
| `vscode-html-language-server` | **Experimental** — `[lsp.html] enabled = true`; root marker `index.html` (only consulted while html is enabled). Serves document symbols, hover, completion, embedded-CSS/JS validation; does **not** implement workspace/symbol, call hierarchy, or type hierarchy. |

## How to add an LSP adapter

Pyright is the worked example; full guide in `docs/adding-an-lsp.md`.

1. Create `internal/lsp/adapters/<name>/` with a `doc.go` stating validation status.
2. Implement every `LSPClient` method (`internal/lsp/client.go`), including `DidChangeWatchedFiles` — the LSP-correct primitive for external file changes. No per-adapter extension methods.
3. Register `conn.SetRequestHandler(a.handleServerRequest)` to answer `client/registerCapability` / `client/unregisterCapability`; without it the server may stall.
4. Implement initialisation: capability negotiation (base on `protocol.DefaultClientCapabilities()`), workspace model, init options.
5. Unit-test with `internal/lsp/jsonrpc/mock.go`; cover the `DidChangeWatchedFiles` wire format (gopls and pyright have explicit tests).

## How to add an MCP tool

1. Create `internal/tools/<name>.go`.
2. Implement the `Tool` interface from `internal/mcp/tools.go` (`Name`, `Description`, `InputSchema`, `Execute`). The `Description` is the authoritative per-tool reference clients read — make it carry the steering.
3. For write/edit tools, take a single `WriteDeps` parameter — don't grow the constructor with positional params; add a `WriteDeps` field for a new cross-cutting concern.
4. Register the tool in `handleConn` (`internal/cli/daemon.go`); write tools use the shared `writeDeps`.
5. Unit-test in `internal/tools/<name>_test.go` (`WriteDeps{}` is the nil-safe setup); document in `docs/tools.md` and update the tool table below.

## Available tools (51)

Concise index only. Full behaviour, schemas, and per-tool steering live in each tool's MCP description (`tools/list`); sources are `internal/tools/<name>.go`.

- **Bootstrap:** `session_start` is the first call; it returns workspace, language, branch, recent context, tool stats, diagnostics, git policy, memories, and client-specific guidance.
- **LSP queries:** `find_symbol`, `workspace_symbols`, `get_definition`, `explain_symbol`, `list_symbols`, `file_outline`, `find_references`, `call_hierarchy`, `type_hierarchy`, `diagnostics`.
- **LSP edits:** `rename_symbol`, `replace_symbol_body`, `insert_before_symbol`, `insert_after_symbol`, `safe_delete_symbol`; these are semantic operations, distinct from file moves/copies, and support `include_doc_comment` where relevant.
- **Filesystem reads:** `read_file`, `read_symbol`, `read_multiple_files`, `list_directory`, `list_files`, `find_files`, `search_in_files`. Reads are bounded, binary-safe, `.gitignore`-aware where applicable, and return mtime/sha headers for optimistic edits.
- **Filesystem writes:** `write_file`, `edit_file`, `delete_file`, `rename_file`, `copy_file`, `transaction_apply`. Writes take `WriteDeps`, hold per-path locks, respect dirty-file checks, notify LSP, invalidate caches, and consume the write-rate budget.
- **Search/replace and git:** `find_replace` is dry-run by default; prefer `rename_symbol` for identifiers. `git` is tiered by policy (read/write/destructive/network), with typed `add`/`commit` and confirmation for dangerous tiers.
- **Other utilities:** `git_init`, `file_diff`, `version`, `daemon_info`, `rename_session`, `workspace_sessions`.
- **Topology:** `topology_status`, `topology_search`, `topology_explore`, `topology_impact`, `topology_affected`, `topology_routes`, `structural_query` use the SQLite/FTS5 index at `<workspace>/.plumb/topology.db`.
- **Ranked discovery:** `workspace_search` is the broker over the indexed corpora — code and docs via topology FTS, memories via the memory FTS index — interleaved by per-corpus rank and labelled with `corpus`/`source`/`field`/`score`/`why` plus per-corpus index freshness; `exact_match=false` always (it is discovery, never proof of absence — the exact lane stays `search_in_files`). Discovery ladder: `workspace_search` → topology/LSP → `search_in_files` → bounded `read_file`.
- **Memory:** `list_memories`, `read_memory`, `write_memory`, `delete_memory`, `search_memories`, `relevant_memories` operate on per-workspace markdown memories under `<workspace>/.plumb/memories/`. `search_memories` is FTS5-ranked when the index is fresh (grep fallback otherwise; `mode` = auto/fts/grep); `read_memory` shows a provenance footer for generated memories; writes/deletes keep the index current. See `[memory]` config.

## TUI conventions (Bubble Tea v2)

- Import paths are **v2 only**: `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`. Never mix in the v1 packages — type/API incompatibilities.
- `Model` is exported; `NewModel(logPath)` constructs, `Run(logPath)` is the entry point. `View()` returns `tea.View` (`v.AltScreen = true`). Keys: `tea.KeyPressMsg`, match via `msg.String()`.
- Sections (opened with `/`): `Dashboard`, `Sessions`, `Memory`, `Logs`, `Settings` (0–4). Settings (`internal/tui/model_settings.go`) is a two-pane editor: a left **Scope** column (Global + each workspace) and a right rows pane with **General**/**LSP** tabs; `tab`/`shift+tab` cycle focus, `[`/`]` resize.
- Settings persistence is scope-aware: Global rows write global config (`config.Save`), workspace rows write a sparse override to `.plumb/config.toml`. Each row carries a reload-tier numeral (`¹` live / `²` next session / `³` restart) and a `⁴` override / `⁵` inherited mark; only **Theme** and **Log level** apply live. `ctrl+t` opens the theme picker.
- List and `[lsp.<lang>]` rows open shared pop-up editors (`model_settings_listeditor.go`, `model_settings_texteditor.go`), auto-saving on close; overlays dim via `dimLines()` + `spliceOverlay()`.
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

## Tool implementation pattern

Every `Tool.Execute()` must be a thin orchestrator over named, individually-testable steps (parse/validate → domain logic → presentation). PRs that add a monolithic `Execute()` are non-conforming. Each inner function stays under gocyclo 15.

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

## Testing requirements

- Tests live next to the code (`_test.go` in the same package); table-driven where the shape fits.
- `internal/lsp/`, `internal/cache/`, `internal/tools/` require meaningful coverage. For write tools, `WriteDeps{}` is the zero-value setup. Per-session isolation tests belong in the package they test.
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
