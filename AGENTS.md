# Plumb — Agent Instructions

> Source of truth: edit `AGENTS.md` only.
>
> `CLAUDE.md`, `GEMINI.md`, and `CHATGPT.md` are repository symlinks to this file for client compatibility. Do not replace, unlink, rewrite, or edit those symlink paths directly. If an instruction change is needed, update `AGENTS.md`; the linked files will reflect it automatically.
>
> These agent-context files are tracked in git to ensure a consistent, high-quality experience for AI assistants.

This file is the canonical brief for AI agents working in the plumb codebase. Keep it accurate; it ages fast.

> **CRITICAL — tool priority:** Always use plumb MCP tools for all tasks when plumb is present and the required capability is available through plumb. Do not fall back to native tools (Read, Edit, Bash, shell commands, etc.) for file reads, writes, edits, searches, symbol lookups, or git queries when the equivalent plumb tool exists. Plumb tools are LSP-aware, concurrency-safe, and session-tracked; native tools bypass all of that. The only exceptions are tasks plumb explicitly does not cover (e.g. running tests, compiling, interacting with external services).

> **Per-tool detail lives in the tool's own MCP description.** Each tool registers its full description and input schema (`tools/list`), and `session_start` emits client-specific tool guidance at runtime. This file is orientation, not the authoritative tool reference — when a tool's behaviour matters, read its description.

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
| `internal/lsp/adapters/{gopls,pyright,jdtls,rust,swift,zig,typescript,kotlin,html}/` | Go, Python, Java, Rust, Swift (validated), Zig + TypeScript/JS + Kotlin + HTML (experimental — unit-tested, integration pending binary install). All non-Go/Python adapters are opt-in via `[lsp.<lang>] enabled = true`. |
| `internal/topology/` | SQLite/FTS5 semantic graph; background indexer; Go AST, tree-sitter Python/JavaScript/TypeScript/Rust/Zig/Kotlin/Swift/Java/Bash/HCL/SQL/Dockerfile/TOML/YAML/Markdown/HTML, and a TSX/JSX (`.tsx`/`.jsx`) regex extractor; search + BFS explore/impact/affected/routes |
| `internal/topology/extractors/golang/` | Go extractor (`go/parser`+`go/ast`; no CGo) |
| `internal/topology/extractors/treesitter/` | gotreesitter extractors (pure-Go, no CGo): **Python, JavaScript, TypeScript, Rust, Zig, Kotlin, Swift, Java, Bash, HCL, SQL, Dockerfile, TOML, YAML, Markdown live** (the config/IaC/markup grammars extract their named declarations — Bash functions, HCL/SQL/Dockerfile/TOML/YAML config, Markdown headings; TOML/YAML/Markdown index the nesting tree via containment edges; Dockerfile matches by basename via the extensionless-file matcher in `findExtractor`). **JavaScript (`.js`/`.mjs`/`.cjs`) and TypeScript (`.ts`)** are on tree-sitter; the TS lex-states gap is closed by `ts_lex_states.go` (the missing `typescriptExternalLexStates` table regenerated via gotreesitter's `ts2go` and supplied through the exported `grammars.RegisterExternalLexStates` — no fork). **TSX/JSX (`.tsx`/`.jsx`) stay on the regex extractor**: gotreesitter v0.19.1's TSX grammar still cascades on typed arrow params even with the regenerated TSX lex-states (the table is generated + registered, ready for an upstream fix). Embeds the `grammars` package (206 blobs + runtime, ~+26 MB; ~46 MB binary). See `docs/internal/treesitter-plan.md`. |
| `internal/topology/extractors/typescript/` | TSX/JSX (`.tsx`/`.jsx`) regex extractor; no CGo. TypeScript (`.ts`) and JavaScript moved to tree-sitter. |
| `internal/langsupport/` | Per-language capability registry (structural engine + LSP adapter, keyed by language). Single source of truth for `buildExtractors` (`internal/cli/topology_pool.go`); the seam for moving a language onto tree-sitter. Pure data. |
| `internal/tools/` | MCP tool implementations; `WriteDeps` bundles write-tool deps; `txlog` subpackage is the transaction rollback WAL |
| `internal/quality/` | Offline post-write code analysers (golangci-lint, …) against changed files; findings appended to write responses |
| `internal/cache/` | Session-scoped symbol cache + LSP-driven invalidator |
| `internal/config/` | TOML config, XDG paths, project-config merging |
| `internal/session/` | Session-file registration + client identity tracking |
| `internal/stats/` | Global SQLite tool-call statistics, row-scoped by workspace and session (WAL, P95, client-aware, `user_version` 7) |
| `internal/memory/` | Per-workspace markdown memory store; exposed as MCP resources |
| `internal/tui/` | Bubble Tea v2 TUI — live session + stats dashboard, recent-edits panel |
| `internal/render/` | Shared, pure CLI/TUI presentation helpers (stdlib + rendering libs only) |
| `internal/fsguard/` | Guards filesystem walks against macOS TCC false-positive prompts on protected dirs |
| `internal/monitor/` | Process resource-usage snapshots (CPU %, memory) plus the daemon start time, feeding the TUI daemon metrics and its uptime baseline |
| `internal/cli/` | Cobra subcommands; daemon, proxy, pool, workspace detection, `config show` |

## Daemon architecture

`plumb serve` is a resilient stdio proxy. The real server is `plumb daemon`:

```
Claude Desktop / Claude Code / Codex / Gemini CLI
  └── plumb serve  (per conversation — dials Unix socket, frame-aware proxy)
        └── ~/Library/Caches/plumb/plumb.sock  (macOS; os.UserCacheDir())
              └── plumb daemon  (one process, shared across all conversations)
                    ├── workspacePool  (one gopls per workspace root; refcounted — torn down after idle grace once last session leaves)
                    │     └── poolEntry{proxy, inv, cache, refs, graceTimer} per root
                    └── handleConn()  (per-connection MCP session)
                          ├── readTracker        (per-connection strict-mode state)
                          ├── writeLimiter       (per-connection limit + shared budget parent, keyed by (client, workspace))
                          ├── editsCfg + strictFn (resolved per-project [edits])
                          ├── gitCfg + gitPolicyFn (resolved per-project [git])
                          ├── sessionCache       (per-connection symbol cache)
                          └── lsRefRoot          (workspace root for this session's pinned LSP reference)
```

On daemon start the binary writes these files under `os.UserCacheDir()/plumb` (e.g. `~/Library/Caches/plumb/` on macOS, `~/.cache/plumb/` on Linux):

| File | Purpose |
|---|---|
| `plumb.sock` | Unix socket — MCP wire |
| `plumb.pid` | PID for `plumb stop` |
| `plumb.version` | Build version; `plumb serve` warns on mismatch |
| `plumb.spawn.lock` | `flock`'d briefly by `plumb serve` to serialise daemon spawn decisions |
| `plumb.daemon.lock` | `flock`'d by `plumb daemon` for its lifetime; a second daemon sees `EWOULDBLOCK` and exits |
| `plumb.ctrl.sock` | Admin socket; line-based `set-level <level>` commands from `plumb log-level` |
| `daemon.log` | All daemon logs |

Stats live in one global DB at `config.DataDir()/stats.db` (e.g. `~/Library/Application Support/plumb/stats.db` on macOS). Every row carries `workspace` and `session_id`; project/session views filter on those.

**Singleton enforcement** (two advisory `flock`s in `internal/cli/lock.go`): `plumb.spawn.lock` serialises `plumb serve`'s spawn decision (re-dialling inside the critical section); `plumb.daemon.lock` is held by `plumb daemon` for its lifetime so a second daemon exits on `EWOULDBLOCK`. Both release on process exit; the lock files persist as zero-byte rendezvous points and are never cleaned up.

**Resilient proxy** (`internal/cli/serve_proxy*.go`, 0.8.0+): `plumb serve` is not a byte pump — it is a frame-aware reconnecting proxy that survives a daemon crash or hang without the client noticing. On a daemon failure it keeps the client's stdio open, dial-or-spawns a fresh daemon, and **replays the captured MCP handshake** (the client only sends `initialize` once, so the proxy stashes and resends it, swallowing the replayed response). In-flight requests get a synthesised retryable error (`code -32000`) instead of hanging; non-idempotent writes are never auto-replayed. A *hung* daemon (alive but silent) is caught by an idle `ping` heartbeat — the MCP server dispatches concurrently, so a missed pong is a real signal — then `SIGTERM`→`SIGKILL`'d via `plumb.pid` and respawned. Reconnects are bounded (exponential backoff); on give-up the proxy exits like the legacy path. Knobs: `PLUMB_PROXY_RECONNECT` (default on; off ⇒ legacy `io.Copy` proxy), `PLUMB_PROXY_HEARTBEAT` (duration; `0` disables hang detection), and `plumb serve --no-reconnect`.

## Configuration layers

Built in four layers, each overriding the prior; `plumb config show` prints the resolved config with provenance.

1. **Compiled defaults** in `internal/config/config.go` `defaults`.
2. **Global config** at `$XDG_CONFIG_HOME/plumb/config.toml` (falls back to `~/.config/plumb/config.toml`). Held in a live `config.Store` (`internal/config/store.go`) and hot-reloaded — an fsnotify watch on the file, the `reload-config` control-socket command, and `plumb config reload` each trigger a re-read that propagates to every live session. Settings the daemon cannot apply live (LSP servers, cache, log format) are flagged by `plumb config show` and `daemon_info` as restart-needed.
3. **Project config** at `<workspace>/.plumb/config.toml`. Merged onto global per connection — only fields the project sets are overridden.
4. **Environment variables** — highest precedence.

Env vars are noted inline below as comments; values shown are defaults.

### `[edits]` — write-tool safety

```toml
[edits]
strict = false                # PLUMB_STRICT_EDITS — require read_file (matching mtime) before edit_file. Per-session via ReadTracker
rate_limit_per_minute = 120   # PLUMB_WRITE_RATE_LIMIT — sliding-window cap per session; 0 disables. A shared parent budget (keyed by (client identity, workspace)) additionally caps the combined rate across all connections from the same client to one project — but two clients on different workspaces never share a budget (cross-workspace isolation).
show_write_diff = true        # PLUMB_SHOW_WRITE_DIFF — append a unified diff to edit_file/write_file responses
post_write_diagnostics_ms = 300 # PLUMB_POST_WRITE_DIAG_MS — ceiling on how long write/edit tools wait for the LSP to re-publish diagnostics after a write; the effective wait adapts down to the server's observed latency. 0 disables the wait entirely
```

### `[workspace]` — root detection fallback + path-access roots

```toml
[workspace]
auto_attach = false           # PLUMB_AUTO_ATTACH — fall back to SynthesiseRoot (nearest .git/ or seed) when no marker is found; LSP unavailable
auto_attach_persist = false   # PLUMB_AUTO_ATTACH_PERSIST — create .plumb/ at the synthetic root on first attach (implies auto_attach)
allow_dependency_reads = true # let read/search tools reach the Go module cache (go env GOMODCACHE) + GOROOT read-only; writes there always refused
extra_roots = []              # additional read-WRITE directories, additive to the workspace ($VAR-expanded)
read_roots = []               # additional read-ONLY directories (vendored deps, shared libs), additive ($VAR-expanded)
```

The workspace boundary (below) is enforced through a per-connection **`PathPolicy`** (`internal/tools/pathpolicy.go`): an allowlist of roots, each tagged read-only or read-write. The detected workspace is always read-write; `extra_roots` add more read-write roots; `read_roots` and — for a Go session with `allow_dependency_reads` — the module cache + `GOROOT` add read-only roots. Read/search tools admit any allowed root (out-of-workspace reads are annotated `# plumb-note: read-only — outside the workspace (<root>); not editable`); write tools demand read-write, so **a write outside the workspace is refused by construction**. (Setting the allowlist from MCP-client launch args — `serve --allow-dir`/`PLUMB_ALLOWED_DIRS` — is a planned follow-up; today the roots come from project/global config.)

### `[git]` — tiered git tool gating

```toml
[git]
allow_writes = true                     # PLUMB_GIT_ALLOW_WRITES — add, commit, switch, branch/tag create, stash
allow_destructive = false               # PLUMB_GIT_ALLOW_DESTRUCTIVE — reset, clean, checkout, restore, rebase, …  (each call also needs confirm:true)
allow_push = false                      # PLUMB_GIT_ALLOW_PUSH — push/fetch/pull (each call also needs confirm:true)
protected_branches = ["main", "master"] # never force-pushable, even with allow_push + confirm
```

Layered like everything else and hot-reloaded (`gitPolicyFn`, `internal/cli/conn.go`). Classification is *safe-biased* (`classifyGit`, `internal/tools/git.go`): ambiguous subcommands round **up** a tier (`checkout -b` is a write, any other `checkout` is destructive; bare `git stash` is a write; `restore --staged` write vs `--worktree` destructive). `add`/`commit` are typed, not pass-through — `commit` only runs `commit -m <message>`, `add` only `add -- <files>`; pre-commit hooks always run. The subcommand always leads argv and a denylist rejects `-c`/`-C`/`--git-dir`/`--work-tree`/`--namespace`/`--exec-path`/etc., so global flags can't reconfigure git; there is no shell. Output is capped (200 lines for `log`/`blame`, 100 KiB overall).

### `[ui]` — TUI theme (global config only)

```toml
[ui]
theme = "nordico"   # built-in theme name; must match a key in tui.AvailableThemes
```

Built-ins: `nordico`, `darcula`, `dracula`, `gruvbox` (dark); `github-light`, `solarized-light` (light) — each maps to a chroma style for `plumb config show`. Written live by the theme picker, read at startup by `internal/cli/root.go`. `config.Save` does a full-file rewrite (load → mutate → re-encode); user-added TOML comments are lost on first save — known v1 limitation. Project config ignores `[ui]`.

### `[lsp_query]` — LSP tool-call timeout

```toml
[lsp_query]
timeout = "30s"   # PLUMB_LSP_QUERY_TIMEOUT — cap on a single LSP tool call; "0s" disables
```

Top-level section (distinct from per-language `[lsp.<lang>]` tables). Applied at the tool layer (`withLSPDeadline`, `internal/tools/lsp_deadline.go`) and a no-op when the caller's context already carries a deadline, so the cold-start `initialize`/`initialized` handshake is never shortened. Independently, `jsonrpc.Conn.Call` logs any request slower than 2 s at WARN.

**LSP → topology fallback:** on LSP error/timeout, `find_symbol`, `workspace_symbols`, and `list_symbols` fall back to the topology index (when `[topology]` is enabled), returning approximate results annotated `source=topology, mode=indexed-approximate`. It runs under the original request context and is a no-op when topology is disabled or has no match (the authoritative LSP error still surfaces). Position/semantic tools (`get_definition`, `find_references`, `explain_symbol`, hierarchies, `rename_symbol`) have no equivalent and surface the error unchanged.

### `[topology]` — semantic index

```toml
[topology]
enabled                 = true    # on by default; opt out with enabled = false. SQLite/FTS5 index at <workspace>/.plumb/topology.db
resync_on_attach        = false   # full resync each time the workspace attaches
exclude_patterns        = []      # path globs to skip during indexing
max_file_size_bytes     = 524288  # 512 KiB cap per file; 0 = default
resync_batch            = 100     # files per pause during a full resync; 0 disables pacing
resync_pause_ms         = 25      # pause after each batch, ms; 0 disables pacing
resync_interval_minutes = 60      # periodic full resync FALLBACK; suppressed while the watcher is live; 0 disables
watch                   = true    # OS-level file watching (fswatcher): re-index on change, whoever made it
```

Enabled by default (opt out with `[topology] enabled = false`, per-project or global). On first attach to a workspace the index is created at `<workspace>/.plumb/topology.db` — this is the one case where plumb materialises `.plumb/` for a project that did not have it. Only the full resync walk is paced — write-triggered upserts are never delayed. Exposed through six `topology_*` tools and backs the LSP fallback above; `plumb doctor` reports its health and the TUI Sessions panel shows a topology row when an index exists. `topology.db` (+ `-wal`/`-shm`) is auto-added to `<workspace>/.plumb/.gitignore`. See the [Topology guide](docs/topology.md).

**Live indexing (`watch = true`, default on).** An OS-level file watcher ([`fswatcher`](https://github.com/sgtdi/fswatcher): FSEvents/inotify/kqueue) re-indexes a file the moment it changes on disk — regardless of who changed it (this agent, another agent, or your editor), which a write-hook alone cannot see. It feeds the same bounded indexer queue, so a mass change (`git checkout`, a formatter rewriting everything) coalesces and, on queue/OS overflow, collapses to a single paced full resync rather than a storm. While the watcher is live the periodic `resync_interval_minutes` poll is **suppressed** (freshness is event-driven; a full resync still runs at startup and on any dropped/overflow signal). `resync_interval_minutes` remains the fallback when `watch = false` or the platform watcher cannot start (logged, degrades gracefully). `.plumb/` is excluded from watching so the index's own writes never self-trigger.

**Per-project tuning (0.8.34):** the merged per-project `[topology]` config (`config.LoadProject`) is passed into `topologyPool.Acquire`, so per-project tuning (`resync_interval_minutes`, `resync_batch`, `exclude_patterns`, `max_file_size_bytes`, …) — not just enable/disable — is honoured on first attach and re-applied when a session re-acquires after a config reload. (A root with no live session keeps whatever tuning a *global* reload applied until its next attach re-tunes it — a benign edge: an orphaned store has no consumer.)

### `[session]` — idle detection & eviction

```toml
[session]
idle_threshold_minutes = 30   # mark a session idle in the TUI after this long with no tool call
eviction_ttl_minutes   = 60   # daemon force-closes a connection idle this long; 0 disables eviction
```

Global or per-project; no env override. `idle_threshold_minutes` is cosmetic — the TUI Sessions panel shows a `~` marker on a session with no tool call for longer than the threshold. `eviction_ttl_minutes` has teeth: a daemon-side reaper (fixed 5-min tick) cancels a connection whose last tool call was longer ago than the TTL, so a `plumb serve` whose agent silently disconnected (its stdio pipe held open by a parent process) is reclaimed instead of lingering. The TTL is read live (hot-reloaded); set `eviction_ttl_minutes = 0` to disable eviction entirely. A tool call is the activity signal — `LastSeenAt` is the session file's mtime, advanced by `session.Touch` after every call.

## Client setup commands

`plumb setup` registers the current `plumb` binary as a stdio MCP server:

| Client | Command | Config target |
|---|---|---|
| Claude Desktop | `plumb setup claude-desktop` | Platform-specific Claude Desktop JSON config |
| Claude Code, user scope | `plumb setup claude-code` | `~/.claude.json` + `~/.claude/skills/` (skill files) |
| Claude Code, project scope | `plumb setup claude-code --project` | `.mcp.json` in the current directory + `~/.claude/skills/` (skill files) |
| Codex | `plumb setup codex` | `$CODEX_HOME/config.toml`, or `~/.codex/config.toml` when unset |
| Gemini CLI | `plumb setup gemini` | `~/.gemini/settings.json` |

Setup helpers preserve existing MCP servers, back up config before modifying it, and resolve config locations via OS/user-home helpers or client env vars — no hardcoded absolute user paths.

`plumb setup claude-code` also installs two user-scoped skill files into `~/.claude/skills/` (created if absent): `plumb-explore` (topology/LSP-first navigation) and `plumb-refactor` (semantic rename, atomic cross-file edits, read→mtime→edit discipline). Skills are installed on every run and are idempotent — unchanged content is a no-op, changed content is backed up then updated. Pass `--no-skill` to skip skill installation.

## Workspace detection

`workspacePool.Detect(dir)` walks up from `dir`:

1. **`.plumb/` marker** — explicit workspace. Returns `(dir, language)` if an LSP language is detectable here or in an ancestor; otherwise `(dir, "none")` — filesystem tools, stats, and project config still work, LSP tools fail until a language attaches.
2. **A language root marker** (`go.mod`, `pyproject.toml`, `setup.py`, …) — returns `(dir, language)`.
3. **A `.git/` directory** — an unambiguous project boundary. Returns `(dir, "none")` so a scripts / multi-language repo with no language marker still resolves. `$HOME` is excluded (a dotfiles repo there must not capture the whole home directory). Priority is per-directory, nearest-wins: a `.plumb/` or language marker always beats a `.git/` further up, and `.git/` only wins when nothing more specific is found.

Walks to the parent otherwise; errors only after passing the filesystem root.

`LanguageNone` (`"none"`, 0.5.26+) keeps non-Go/non-Python projects fully attached minus LSP (fixed the "TUI stuck on resolving…" symptom — the TUI renders such a session's language as `?`). The `.git/` fallback (0.7.20+) extends this to any git repo, so the common case (a repo without a language marker, default config) now resolves on the first path-bearing tool call instead of staying on "resolving…" forever. **Auto-attach** (opt-in, `[workspace].auto_attach`, 0.6.4+) still covers the residual case — a seed dir with *no* `.git/` anywhere above — falling back to `SynthesiseRoot` when `Detect` fails; synthetic sessions are marked `(auto)` in the TUI, and `auto_attach_persist` creates `<root>/.plumb/` on first attach.

Cold-start resolution in `session_start`: the daemon's already-attached root (authoritative — `onBeforeTool` attaches the workspace before the tool body runs, including from this call's own `workspace` arg) → explicit `workspace` arg → `roots/list` query → otherwise an honest "pass `workspace`" error. There is **no `os.Getwd()` fallback** (removed 0.7.20): the daemon is a singleton shared across connections, so its working directory is never a per-session signal, and guessing it reported the wrong project. Clients that don't report a folder (Claude Desktop identifies as `claude-ai` and sends no `roots`) must pin the project by passing an absolute `workspace` to `session_start` (or any path-bearing tool). Run `plumb init` in any project root to create a `.plumb/` marker (holds `context.md`, the `memories/` store, and `topology.db` when enabled).

**Single-workspace-per-connection contract (0.8.19+).** Once a connection has attached a workspace, every path-bearing tool — file/list/search/write/git/memory/LSP-URI/topology-status — refuses paths outside the connection's allowed roots with a `workspace boundary violation` error. The allowed set is the workspace (read-write) plus any configured `extra_roots` (read-write) and `read_roots` / Go dependency roots (read-only) — see `[workspace]` above; writes are only ever permitted under read-write roots. `rename_symbol` also boundary-checks each output URI in the language server's `WorkspaceEdit` before applying. To work on a different project, call `session_start` with an explicit `workspace` pointing at the other project's root: a deliberate `workspace` argument re-pins the connection (tearing down and re-attaching LSP/topology/quality/config routing to the new root) rather than being refused (0.8.26+). This matters because Claude Desktop and similar clients may reuse one `plumb serve` across chats, so a fresh chat does **not** guarantee a fresh connection — without in-session re-pin the connection would stay welded to the first project it ever touched. The boundary still makes reuse safe rather than relying on client behaviour: only the deliberate `session_start` call switches projects; ad-hoc path tools never silently wander across the boundary. A connection that hits a violation has its session marked `Health: blocked` for the TUI dashboard, but legitimate in-workspace calls keep working — the per-call rejection is the contract, the sticky health flag is observability. `git`'s `repo` argument defaults to the pinned workspace when omitted (0.8.20+), so a bare `git status` runs against the session's project rather than the daemon's cwd.

## Adapter validation status

| Adapter | Status |
|---|---|
| `gopls` | **Validated** — mock-transport unit tests + real-binary integration; `client/registerCapability` answered, `workspace/didChangeWatchedFiles` confirmed. |
| `pyright` | **Validated** — same coverage as gopls, against the real pyright-langserver binary. |
| `jdtls` | **Validated** (experimental; enable with `[lsp.java] enabled = true`; needs Java 21+ and jdtls on PATH). jdtls sends string request IDs, so the conn uses `json.RawMessage` for IDs. **Gotcha:** unlike gopls/pyright it needs *both* `DidChangeWatchedFiles` (project model) and `DidOpen` (triggers reconcile, sent after the server's `ServiceReady` notification) for reliable diagnostics after external writes. |
| `rust-analyzer` | **Validated** — mock-transport unit tests + real-binary integration (`rustup component add rust-analyzer`); enable with `[lsp.rust] enabled = true`. Pairs with the tree-sitter Rust Map. **Cold-start warning:** loads the sysroot + runs `cargo metadata` on first attach — can take minutes on a large workspace; this is exactly the unavailability case the structural layer covers while it warms (the topology fallback keeps Rust symbol queries working meanwhile). |
| `sourcekit-lsp` | **Validated** — mock-transport unit tests + real-binary integration (ships with the Swift toolchain / Xcode; `/usr/bin/sourcekit-lsp` on macOS); enable with `[lsp.swift] enabled = true`. Root marker `Package.swift`; derives per-file compiler args from the SwiftPM build plan. Pairs with the tree-sitter Swift Map. |
| `zls` | **Experimental** — mock-transport unit tests pass; the real-binary integration test (`testdata/zig-fixture/`) is written and gated `//go:build integration` but **skips until `zls` is on PATH** (not installed on the validation machine). Enable with `[lsp.zig] enabled = true`; root markers `build.zig`/`build.zig.zon`. Pairs with the tree-sitter Zig Map. zls + tree-sitter-zig track the Zig language version (pre-1.0; ongoing maintenance surface). |
| `typescript-language-server` | **Experimental** — mock-transport unit tests pass; the real-binary integration test (`testdata/typescript-fixture/`) is written and gated `//go:build integration` but **skips until `typescript-language-server` is on PATH** (`npm install -g typescript-language-server typescript`). Enable with `[lsp.typescript] enabled = true`; root markers `tsconfig.json`/`jsconfig.json`/`package.json`. Serves **both** TypeScript and JavaScript (the `typescript` and `javascript` `langsupport` rows both name it). Pairs with the regex TS Map and the tree-sitter JS Map. |
| `kotlin-language-server` | **Experimental** — mock-transport unit tests pass; the real-binary integration test (`testdata/kotlin-fixture/`) is written and gated `//go:build integration` but **skips until `kotlin-language-server` is on PATH** (`brew install kotlin-language-server`). Enable with `[lsp.kotlin] enabled = true`; root markers `settings.gradle.kts`/`build.gradle.kts` (the `build.gradle.kts` marker overlaps Java's — with both enabled, alphabetical detect order makes Java win; both are opt-in). Pairs with the tree-sitter Kotlin Map. |
| `vscode-html-language-server` | **Experimental** — mock-transport unit tests pass; the real-binary integration test (`testdata/html-fixture/`) is written and gated `//go:build integration` but **skips until `vscode-html-language-server` is on PATH** (`npm install -g vscode-langservers-extracted`). Enable with `[lsp.html] enabled = true`; root marker `index.html` (only consulted while html is enabled, so it never fragments a non-HTML repo by default). Serves document symbols, hover, completion, and embedded-CSS/JS validation; does **not** implement workspace/symbol, call hierarchy, or type hierarchy. Pairs with the tree-sitter HTML Map. |

Real-binary validation has been exercised on macOS only; Linux/Windows is pre-v1 hardening work.

## How to add an LSP adapter

Pyright is the worked example.

1. Create `internal/lsp/adapters/<name>/` with a `doc.go` stating validation status.
2. Implement every method of the `LSPClient` interface (`internal/lsp/client.go`), including `DidChangeWatchedFiles` — the LSP-correct primitive for external file changes. No per-adapter extension methods.
3. Register `conn.SetRequestHandler(a.handleServerRequest)` to answer `client/registerCapability` / `client/unregisterCapability`; without it the server may stall.
4. Implement initialisation: capability negotiation (base on `protocol.DefaultClientCapabilities()` — it declares `workspace.didChangeWatchedFiles.dynamicRegistration: true`), workspace model, init options.
5. Unit-test with `internal/lsp/jsonrpc/mock.go`; cover the `DidChangeWatchedFiles` wire format (gopls and pyright have explicit tests).
6. Document in `docs/adding-an-lsp.md`.

## How to add an MCP tool

1. Create `internal/tools/<name>.go`.
2. Implement the `Tool` interface from `internal/mcp/tools.go` (`Name`, `Description`, `InputSchema`, `Execute`). The `Description` is the authoritative per-tool reference clients read — make it carry the steering.
3. For write/edit tools, take a single `WriteDeps` parameter — don't grow the constructor with positional params; add a `WriteDeps` field for a new cross-cutting concern.
4. Register the tool in `handleConn` (`internal/cli/daemon.go`). Write tools use the shared `writeDeps`.
5. Unit-test in `internal/tools/<name>_test.go`; `WriteDeps{}` is the nil-safe setup.
6. Document in `docs/tools.md` and update the tool table below.

## Available tools (49)

Concise index — each tool's full behaviour, inputs, and steering live in its MCP description (`tools/list`). Source files follow the `internal/tools/<name>.go` convention.

**Bootstrap**

- `session_start` — **Call FIRST.** Orientation packet (workspace, language, branch, recent commits + files, memories, top-5 tool stats, the live git tool policy, active diagnostics) plus a client-specific tool-guidance section. Cold-start chain: explicit → daemon-resolved → `roots/list` → cwd walk.

**LSP queries**

| Tool | LSP method | Notes |
|---|---|---|
| `find_symbol` | `documentSymbol` | Single-file; `uri` required. |
| `workspace_symbols` | `workspace/symbol` | Workspace-wide name search; stdlib/deps filtered. |
| `get_definition` | `definition` | Definition location by name or position. |
| `explain_symbol` | `hover` | Docs + type info. |
| `list_symbols` | `documentSymbol` | Full hierarchy with line ranges. |
| `file_outline` | `documentSymbol` | Token-cheap skeleton (signatures, bodies collapsed); tree-sitter topology fallback when the server is cold/absent. |
| `find_references` | `references` | All call sites + source line. |
| `call_hierarchy` | `prepareCallHierarchy` | Incoming + outgoing. |
| `type_hierarchy` | `prepareTypeHierarchy` | Supertypes + subtypes. |
| `diagnostics` | notification subscriber | Errors, warnings, hints. |

**LSP edits** — `rename_symbol` (workspace-wide semantic rename; distinct from `rename_file`), `replace_symbol_body`, `insert_before_symbol`, `insert_after_symbol`, `safe_delete_symbol` (refuses if external refs exist). The symbol-edit tools take an optional `include_doc_comment`.

**Filesystem reads**

| Tool | Notes |
|---|---|
| `read_file` | Path or `file://`; 200 KiB cap; binary detection. Emits a `# plumb-read mtime=… sha256=…` header — pass the value to `edit_file.expected_mtime` (or `expected_sha`). |
| `read_symbol` | Source body of a named symbol in one call (plain name or `ReceiverType.Method`). Emits the same mtime header. |
| `read_multiple_files` | Up to 20 paths, parallel; per-file errors inline. |
| `list_directory` | Immediate children with `[FILE]`/`[DIR]`, sizes, mtimes; glob + sort. |
| `list_files` | Recursive; glob filter; depth control; respects `.gitignore`. |
| `find_files` | Glob/regex finder; honours `.gitignore`. |
| `search_in_files` | Exact content scan; **literal text by default** (`use_regex:false`); set `use_regex:true` for Go RE2 regex. Smart-case; `.gitignore`-aware; `exclude` globs; `include_enclosing_symbol` annotates each match with the deepest LSP symbol. |

**Filesystem writes** — all take `WriteDeps`, hold per-path locks, check git dirty state (`dirty_ok` to override), notify the LSP via `didChangeWatchedFiles`, invalidate the symbol cache, and consume one rate-limit slot.

| Tool | Notes |
|---|---|
| `write_file` | Atomic create/overwrite (tmpdir + rename, EXDEV fallback); symlink-aware; permissions preserved. |
| `edit_file` | Two modes per edit item: str_replace (`old_string` must appear exactly once) or range (`start_line`/`end_line` to replace/delete a block; `start_line: -1` appends at EOF; `end_line: -1` extends to last line). CRLF-tolerant; all-or-nothing; optional `expected_mtime` concurrency check; `apply_partial` applies each edit independently. |
| `delete_file` | Refuses directories by default; `allow_dir: true` deletes an empty directory (non-empty always rejected). |
| `rename_file` | **Primary move tool.** Atomic; refuses overwrite without `overwrite=true`. Distinct from `rename_symbol`. |
| `copy_file` | Duplicate preserving permissions; cross-device safe; refuses overwrite without `overwrite=true`. |
| `transaction_apply` | Multi-file atomic edits (≤50 ops): validate in memory → write under locks → roll back on partial failure. For cross-file refactors. |

**Other**

| Tool | Notes |
|---|---|
| `find_replace` | Text/regex find-and-replace across files; dry-run by default; `format_after` runs the workspace formatter. Prefer `rename_symbol` for identifiers. |
| `git` | Tiered git tool — read (always) / write (`allow_writes`) / destructive (`allow_destructive` + `confirm`) / network (`allow_push` + `confirm`). Subcommand leads argv; unknown subcommands rejected. |
| `git_init` | Initialise a repo; `init_plumb: true` also creates `.plumb/context.md`. |
| `file_diff` | Unified diff between two arbitrary files (`diff -U`). |
| `version` | Server version, Go runtime, OS/arch. |
| `daemon_info` | Session name + ID, daemon version, start time, uptime; live config state (generation, last reload, restart-needed); plus this session's tool-call count and its slowest calls. |
| `rename_session` | Rename the current MCP session (letters/digits/`-`, ≤25 chars). |
| `workspace_sessions` | Same-workspace peer awareness: lists other active sessions on this workspace (name, client, idle status) and the recent write/edit operations any session made here. Call before editing a file a peer recently touched. `recent_limit` caps the write feed (default 10, max 50). Read-only; workspace-boundary-guarded. |

**Topology** — SQLite/FTS5 index at `<workspace>/.plumb/topology.db`; on by default (opt out with `[topology] enabled = false`).

| Tool | Notes |
|---|---|
| `topology_status` | Index health: file/entity counts, DB size, languages, last sync/error. |
| `topology_search` | FTS5 ranked symbol/file search; `kinds`/`language` filters. |
| `topology_explore` | BFS neighbourhood around a named symbol; `depth`/`max_nodes`/`include_source`/`edge_kinds`. |
| `topology_impact` | Bidirectional blast radius (depends-on + depended-by) around a symbol. |
| `topology_affected` | Likely affected files + tests for changed files/symbols; run after writing. |
| `topology_routes` | Framework-aware entry-point scanner (`go`/`python`/`swift`/`cobra`/`vapor`/`argument-parser`); heuristic, confidence-annotated. |

**Memory** — per-workspace markdown at `<workspace>/.plumb/memories/`, also exposed as MCP resources: `list_memories`, `read_memory`, `write_memory`, `delete_memory`, `search_memories` (pattern search), `relevant_memories` (path-based relevance).

## TUI conventions (Bubble Tea v2)

- Import paths are **v2 only**: `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`. Never add the v1 packages — mixing Charm v1 and v2 causes type/API incompatibilities.
- `Model` is exported; `NewModel(logPath string)` constructs, `Run(logPath string)` is the entry point. `View()` returns `tea.View` (`tea.NewView(content)`, `v.AltScreen = true`). Key handling: `tea.KeyPressMsg`, match via `msg.String()`.
- Sections (opened with `/`): `Dashboard`, `Sessions`, `Memory`, `Logs`, `Settings` (indices 0–4). Sessions (index 1, default) is a two-panel layout; Logs (index 3) tails `daemon.log`; Settings (index 4) is a scrollable grouped editor (`internal/tui/model_settings.go`).
- Settings rows persist to global config via `config.Save` and apply on next daemon start (marked `*`); only **Theme** and **Log level** apply live (the latter via the control socket in `m.ctrlPath`). `ctrl+t` opens the theme picker from any section.
- Overlays: dim the background with `dimLines()`, splice the box via `spliceOverlay()`.
- **Theme system:** `ActiveTheme`/`ActiveThemeName` are package globals in `internal/tui/theme.go`; all lipgloss style vars are rebuilt by `RebuildStyles()` after any `ActiveTheme` mutation. `AvailableThemes` is the catalogue; adding a `Theme` field means updating every theme literal — `TestTheme_AllFieldsSet` catches omissions.

## Code style rules

- **Australian English** in all prose: docs, comments, log messages, error strings. Use -ise/-isation (initialise, serialise, synchronise, organise, recognise). Use behaviour, colour, honour, favour. **Exception:** identifiers defined by external specifications keep their canonical spelling — LSP method names (`initialize`, `publishDiagnostics`), MCP protocol fields, and Go standard library names are never changed.
- **`gofumpt`** on save. `golangci-lint` v2.12.2 before every commit; CI enforces.
- **`log/slog`** exclusively. Never `log` package or `fmt.Println` for logging.
- **Errors wrap context:** `fmt.Errorf("loading config: %w", err)`.
- **Context everywhere:** every blocking/I/O operation takes `context.Context` first.
- **Concurrency contract** stated in doc comments on every type.
- **No `init()` doing real work.** Wire dependencies in constructors.
- **No globals** except package-level style vars in `internal/tui/styles.go` (rebuilt, not stateful) and the `pathLocks` map in `internal/tools/file_write_helpers.go` (process-global by design).
- **Max ~600 lines per file.** Split if it grows. Exception allowlist: `internal/lsp/protocol/types.go` (protocol type catalogue mirroring the LSP spec). No other file qualifies without explicit justification added here.
- **Comments only when the WHY is non-obvious.** No what-comments.
- **Gocyclo-15 contract.** No first-party non-test function may exceed cyclomatic complexity 15. Decompose before merging; CI enforces.

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

Version is injected at build time: `-X github.com/golimpio/plumb/internal/cli.Version=<version>` (defaults to `"dev"`). The Makefile resolves it from the exact git tag → the `VERSION` file → the short commit hash. To bump during development, edit `VERSION`; do not tag every iteration.

The daemon writes its build version to `~/Library/Caches/plumb/plumb.version`; `plumb serve` warns on mismatch. **If you've just rebuilt, restart the daemon** — new code never activates against the old process. `plumb restart` stops the daemon and brings a fresh one straight back up (use it after a rebuild); since the resilient proxy reconnects connected clients automatically, it is transparent to active conversations. `plumb stop --force` / `plumb restart --force` skip the confirmation prompt (scripts, Makefiles).

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
```

**`make install-hooks` is required after every fresh clone** — the pre-commit hook runs `golangci-lint run --fix ./...`, catching formatting/lint issues before commit. **Formatting note:** apply formatting via `golangci-lint run --fix ./...`, never the standalone `gofumpt -w` binary — the two can pin different versions and produce phantom lint failures.

## Known limitations and pending work

Outstanding items, footguns, and "subtle things to be aware of" live in [`docs/internal/todo.md`](docs/internal/todo.md). Always check it before adding a feature that touches concurrency, the rate limiter, the read tracker, or the stats schema. When you complete a TODO item, delete its section from `docs/internal/todo.md` *in the same commit* that adds the `CHANGELOG.md` entry.

## Quick reference for agents

You are likely an AI agent reading this through plumb. Most common patterns:

> **Parameter names follow Claude Code's native tools** — `file_path`, `old_string`, `new_string` on the file-content tools (`read_file`/`write_file`/`edit_file`/`delete_file`/`transaction_apply`); `path`/`pattern` on the search/dir tools (`list_directory`/`find_files`/`search_in_files`). Common alternative names from other agents and earlier plumb versions (`path`, `old_str`, `new_str`, `filename`, `dir`, `symbol`→`name`, …) are accepted as **aliases**: the call still succeeds, and the response carries a one-line note nudging you toward the canonical name. Genuinely unknown parameters are rejected with the valid list and a "did you mean" suggestion. The LSP query/edit tools take `uri`, which accepts a plain absolute path as readily as a `file://` URI (and `path`/`file_path` alias to it). `read_file` natively accepts Claude Code's `offset` (first line) + `limit` (line count) window alongside plumb's `start_line`/`end_line`.

- **First call:** `session_start({})`. Returns the orientation packet.
- **Read before edit:** call `read_file`, copy its `mtime` header value, pass it as `expected_mtime` to `edit_file`. Mandatory under strict mode; recommended always.
- **Common mistake — mixing plumb reads with the native Edit tool:** after a plumb `read_file`, always edit with plumb `edit_file` — **not** Claude Code's native `Edit`/`Write`. plumb and the Claude Code harness track file read-state *separately*: a plumb `read_file` does not satisfy the harness's read-before-edit rule, so a native `Edit` after a plumb `read_file` fails with "File has not been read yet" (and "File has been modified since read" after any external change). These look like plumb errors but come from mixing the two toolsets. Stay in one lane: `read_file` → `edit_file`.
- **One-shot file create:** `write_file({file_path, content})`.
- **Targeted edit:** `edit_file({file_path, edits: [{old_string, new_string}], expected_mtime})`. `old_string` must appear exactly once. CRLF differences are tolerated automatically.
- **Cross-file refactor:** `transaction_apply({operations: [{file_path, edits, expected_mtime}]})`. All-or-nothing.
- **Delete:** `delete_file({file_path})`. Refuses directories by default; `allow_dir: true` removes an empty directory.
- **Move/rename:** `rename_file({from, to})` — primary move tool. Distinct from `rename_symbol` (LSP-semantic identifier rename).
- **Copy:** `copy_file({from, to})`. Preserves permissions; cross-device safe. Use when you want to keep the source.
- **Discover what changed:** `git({subcommand: "status"})` or `git({subcommand: "log", args: ["-10", "--oneline"]})`.
- **See your own activity:** `plumb` TUI's right panel shows "Recent Edits" for the selected session.
- **Throttled?** You hit the rate limit (default 120/min). Wait or set `PLUMB_WRITE_RATE_LIMIT=0`.
- **Rejected for "has not been read"?** Two different sources: plumb's *strict mode* (`[edits].strict = true`) requires a prior plumb `read_file` — call it first. Claude Code's harness "File has not been read yet" instead means you used the *native* `Edit` after a plumb `read_file`; switch to plumb `edit_file` (see the common-mistake note above).
- **Rejected for "uncommitted changes"?** The target file was dirty in git *before* plumb touched it this session — re-editing a file plumb already wrote this session is never blocked (the dirty-guard is session-aware). Review and commit the pre-existing changes, or pass `dirty_ok: true` to overwrite anyway.
- **Too much log noise from the daemon?** `plumb log-level warn` raises the floor instantly; `plumb log-level reset` restores the config-file default.

When in doubt about the resolved config, `plumb config show --workspace .` from the project directory.
