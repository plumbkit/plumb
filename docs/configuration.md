# Configuration Reference

Plumb is configured through TOML files and environment variables. Every setting
has a compiled-in default, so plumb works with no config at all — you only set
what you want to change.

To see the configuration plumb actually resolved (and where each value came
from), run:

```sh
plumb config show --workspace .   # values + source provenance
plumb config print                # the resolved config as plain TOML
```

## How configuration resolves

Configuration is built in layers. Each layer overrides only the fields it sets;
everything else is inherited from the layer below.

1. **Compiled defaults** — baked into the binary (`internal/config/config.go`).
2. **Global config** — `$XDG_CONFIG_HOME/plumb/config.toml`, falling back to
   `~/.config/plumb/config.toml`. Held in a live `config.Store` and
   hot-reloaded: an fsnotify watch on the file, the `reload-config`
   control-socket command, and `plumb config reload` each trigger a re-read
   that propagates to every live session. Settings the daemon cannot apply
   live (LSP servers, cache, log format) are flagged as restart-needed by
   `plumb config show` and the `daemon_info` tool.
3. **Project config** — `<workspace>/.plumb/config.toml`. Loaded when a
   connection's workspace resolves and merged onto the global config. A project
   file that sets one field inherits the rest.
4. **Environment variables** — highest precedence; useful for one-off overrides
   without editing files.

Most sections are **hot-reloaded** without a reconnect: an `fsnotify` watch on
the global `config.toml` (plus the `reload-config` control command and
`plumb config reload`) re-reads the file and re-merges every live session's
project view. `[edits]`, `[walk]`, `[git]`, `[topology]`, `[session]`,
`[memory]`, `[collab]`, and `[semantics]` apply live. The restart-bound exceptions are the
`[lsp.*]` servers, `[cache]`, and `log_format`; `plumb config show` and
`daemon_info` flag those as restart-needed.

## File locations

| Path | Scope | Notes |
|---|---|---|
| `~/.config/plumb/config.toml` | Global | Honours `$XDG_CONFIG_HOME`. Optional — defaults apply if absent. |
| `<workspace>/.plumb/config.toml` | Project | Created next to `.plumb/`; overrides the global layer. |

---

## Logging (top level)

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `log_level` | string | `"info"` | `PLUMB_LOG_LEVEL` | One of `debug`, `info`, `warn`, `error`. Change a running daemon at runtime with [`plumb log-level`](cli-reference.md#plumb-log-level). |
| `log_format` | string | `"text"` | `PLUMB_LOG_FORMAT` | `text` or `json`. |
| `log_file` | string | `""` | `PLUMB_LOG_FILE` | Empty writes to the daemon log under the OS log dir (`~/Library/Logs/plumb/` on macOS). |

## `[ui]` — TUI presentation (global only)

| Field | Type | Default | Effect |
|---|---|---|---|
| `theme` | string | `"plumb"` | Active colour theme. Set interactively via the TUI **Settings** picker, which persists it here. |

## `[web]` — web UI (global only)

The opt-in, loopback-only web UI launched with `plumb web`. See [Web UI](web.md).

| Field | Type | Default | Effect |
|---|---|---|---|
| `port` | int | `8870` | Loopback TCP port for the web UI. The listener is always bound to `127.0.0.1` only. Applied on the next `plumb web`. |

Like `[ui]`, `[web]` is read from the global config only and ignored in project
config. `plumb web --port` overrides it for a single launch.

## `[cache]` — session symbol cache

| Field | Type | Default | Effect |
|---|---|---|---|
| `ttl` | duration | `"5m"` | Time-to-live for cached LSP query results. |
| `max_size` | int | `1000` | Maximum cache entries. Must be ≥ 0. |

## `[edits]` — write-tool safety

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `strict` | bool | `false` | `PLUMB_STRICT_EDITS` | Require every `edit_file` target to have been read via `read_file` this session, with a matching mtime. |
| `rate_limit_per_minute` | int | `120` | `PLUMB_WRITE_RATE_LIMIT` | Sliding-window cap on writes per session. `0` disables. |
| `post_write_diagnostics_ms` | int | `300` | `PLUMB_POST_WRITE_DIAG_MS` | Ceiling on how long to wait for the LSP server to re-publish diagnostics after a write; the effective wait adapts down to the server's observed latency. `0` disables. |
| `post_write_cross_file` | bool | `true` | `PLUMB_POST_WRITE_CROSS_FILE` | After a write, compare workspace diagnostics against a pre-write baseline and flag NEW errors the edit introduced in OTHER files (the "edit A silently breaks B" case). The edited file's own diagnostics block keeps priority. |
| `post_write_cross_file_settle_ms` | int | `200` | `PLUMB_POST_WRITE_CROSS_FILE_SETTLE_MS` | Bounded grace the cross-file sweep waits, after the edited file's own diagnostics land, for dependent-file re-publishes before comparing. `0` compares immediately. |
| `concurrent_write_skew_ms` | int | `100` | `PLUMB_CONCURRENT_WRITE_SKEW_MS` | Clock-skew allowance for `edit_file`'s concurrent-write detector. Raise on slow/network filesystems. |
| `show_write_diff` | bool | `true` | `PLUMB_SHOW_WRITE_DIFF` | Append a unified diff to `edit_file`/`write_file` responses. Set false to return only metadata. |
| `block_dirty_writes` | bool | `true` | `PLUMB_BLOCK_DIRTY_WRITES` | Refuse a destructive write (`write_file`, `edit_file`, `delete_file`, `find_replace`, `rename_file`, `copy_file`, `transaction_apply`) to a file with uncommitted git changes that plumb did not write this session, unless `dirty_ok: true`. Set false to disable the guard — for a workflow that iterates on uncommitted WIP. Re-editing a file plumb wrote this session is never blocked either way. |

## `[walk]` — filesystem-traversal safety

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `refuse_home_roots` | bool | `true` | `PLUMB_REFUSE_HOME_ROOTS` | Refuse walks rooted exactly at `$HOME` or a protected macOS directory (Desktop, Documents, …) to avoid spurious TCC consent prompts. Subpaths like `~/Documents/MyProject` are still walked. No-op off macOS. |

## `[workspace]` — root-detection fallback

Detection walks up looking for a `.plumb/` marker, a language root marker
(`go.mod`, `pyproject.toml`, …), or a `.git/` directory (since 0.7.20; `$HOME`
excluded). Because any git repo now resolves on its own, `auto_attach` only
comes into play for a directory that is *neither* a git repo *nor* a marked
project.

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `auto_attach` | bool | `false` | `PLUMB_AUTO_ATTACH` | When detection finds no marker at all (no `.plumb/`, language marker, or `.git/`), fall back to a synthetic root (the seed directory). Stats, TUI, and project config work; LSP is unavailable. |
| `auto_attach_persist` | bool | `false` | `PLUMB_AUTO_ATTACH_PERSIST` | Create `.plumb/` at the synthetic root on first attach so later sessions resolve normally. **Implies `auto_attach`.** |
| `allow_dependency_reads` | bool | `true` | — | For a Go session, allow read/search (never write) into the module cache (`GOMODCACHE`) + `GOROOT`. |
| `extra_roots` | []string | `[]` | — | Additional read-**write** directories, additive to the workspace (`$VAR`-expanded). Honoured from **global** config only (see below). |
| `read_roots` | []string | `[]` | — | Additional read-**only** directories — vendored deps, shared libs (`$VAR`-expanded). Honoured from **global** config only (see below). |

### Per-workspace roots (trusted grants)

`extra_roots` / `read_roots` set in a **project** `.plumb/config.toml` are ignored
— `LoadProject` forces them back to the global value, because a cloned repo is
an untrusted surface and must not be able to widen its own filesystem access the
moment a session attaches. To grant **one** workspace extra roots, add them
manually in the TUI Settings screen under that workspace's scope (Extra roots /
Read roots rows). Such a grant is recorded in plumb's own data dir
(`<DataDir>/workspace_roots.json`), keyed by the canonical workspace root —
**never** in the repo — so a cloned repository can neither write it nor change a
granted path after the fact (the VS Code "workspace trust" model). The grants are
additive to the global config roots and shown by `plumb config show --workspace
<dir>` with a `data-dir grant` source.

## `[git]` — tiered git-tool gating

The `git` tool's read tier always runs. Higher tiers are gated here; destructive
and network calls additionally require `confirm: true` per call.

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `allow_writes` | bool | `true` | `PLUMB_GIT_ALLOW_WRITES` | Safe-write tier: `add`, `commit`, `switch`, `branch`/`tag` create, `stash` push/pop. |
| `allow_destructive` | bool | `false` | `PLUMB_GIT_ALLOW_DESTRUCTIVE` | Destructive tier: `reset`, `clean`, `checkout`, `restore`, `rebase`, `revert`, branch/tag delete, `stash` drop. Also needs `confirm:true`. |
| `allow_push` | bool | `false` | `PLUMB_GIT_ALLOW_PUSH` | Network tier: `push`, `fetch`, `pull`. Also needs `confirm:true`. |
| `protected_branches` | []string | `["main", "master"]` | — | Branch names that may never be force-pushed, even with `allow_push` + `confirm`. |

Ambiguous subcommands (`checkout`, `switch`, `restore`, `branch`, `tag`,
`stash`) are classified by their arguments and biased towards the higher tier —
e.g. `checkout -b` is a write but any other `checkout` is destructive, and
`restore --staged` is a write but `restore --worktree` is destructive. `add` and
`commit` are typed (only `commit -m <message>` / `add -- <files>` ever run, so
`--amend`/`--no-verify`/globs are unreachable; pre-commit hooks always run). See
[Tools → `git`](tools.md#git) for the full behavioural contract.

## `[quality]` — post-write code analysis

| Field | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `false` | Run offline analysers against changed files; findings appended to write responses. |
| `mode` | string | `"background"` | `background` (findings on the next request) or `sync` (block up to `timeout_ms` and append inline). |
| `analysers` | []string | `["golangci-lint"]` | Which analysers to run. Unknown names are skipped. |
| `timeout_ms` | int | `2000` | Per-analyser run cap. |
| `max_findings_per_file` | int | `5` | Cap on findings appended per file. |

## `[topology]` — semantic index

| Field | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `true` | The persistent SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`. On by default; set `false` to opt out (per-project or global). The index is created on first attach — the one case where plumb materialises `.plumb/`. See the [Topology guide](topology.md). |
| `resync_on_attach` | bool | `false` | Force a full resync each time the workspace attaches. |
| `exclude_patterns` | []string | `[]` | Path glob patterns to skip during indexing. |
| `max_file_size_bytes` | int64 | `524288` (512 KiB) | Largest file considered for extraction. `0` uses the default. |
| `resync_batch` | int | `100` | Files the full resync extracts before pausing, to throttle CPU. `0` disables pacing. |
| `resync_pause_ms` | int | `25` | Pause (milliseconds) after each `resync_batch` files. `0` disables pacing. |
| `resync_interval_minutes` | int | `60` | Periodic full-resync **fallback**, used only when `watch = false` or the platform watcher cannot start; suppressed while the watcher is live. `0` disables. |
| `watch` | bool | `true` | OS-level file watching ([`fswatcher`](https://github.com/sgtdi/fswatcher)): re-index a file the instant it changes on disk, whoever changed it — this agent, another agent, or your editor. Replaces time-based polling; a mass change (e.g. `git checkout`) coalesces to a single paced resync via the bounded queue + overflow path. Set `false` to fall back to `resync_interval_minutes`. |

## `[session]` — idle detection & eviction

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `idle_threshold_minutes` | int | `30` | — | How long after the last tool call a session is shown idle (a `~` marker) in the TUI Sessions panel. Cosmetic. |
| `eviction_ttl_minutes` | int | `60` | — | How long after the last tool call the daemon force-closes an idle connection — reclaiming a `plumb serve` whose agent silently disconnected but kept its stdio pipe open. A reaper checks every 5 min (fixed). `0` disables eviction. Read live (hot-reloaded). |
| `persist_state` | bool | `true` | `PLUMB_PERSIST_SESSION_STATE` | Persist a connection's session state (pinned workspace, strict-mode read-tracking) to disk so it survives a daemon restart/upgrade transparently, instead of resetting on reconnect. |
| `persist_state_ttl_minutes` | int | `1440` | — | How long persisted session state is honoured on restart before it's treated as stale and discarded. |

Global or per-project; no environment override except `persist_state`. Activity is a tool call: the session file's mtime is advanced after each call (`session.Touch`) and read back as the last-seen time.

## `[memory]` — per-workspace memory engine

Markdown memories under `<workspace>/.plumb/memories/` are the source of truth;
`memory.db` is a rebuildable FTS5 index. Project-overridable; no env override.

| Field | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `true` | The `memory.db` FTS5 index backing ranked `search_memories`. Off ⇒ memory tools use a grep fallback. |
| `generated_summaries` | bool | `true` | Write rule-based episodic summaries (no LLM, always redacted) when a session goes idle. |
| `inject_hints` | bool | `true` | Append a compact "[Hint: relevant memory …]" block to path-bearing tool responses. |
| `hint_budget_bytes` | int | `512` | Byte cap on an injected hint block. |
| `episodic_budget_bytes` | int | `1024` | Byte cap on the "last session" summary in `session_start`. |
| `max_hints` | int | `3` | Max memories hinted per response. |
| `idle_summary_minutes` | int | `0` | Idle threshold before an episodic summary; `0` falls back to `[session] idle_threshold_minutes`. |
| `generated_memory_keep` | int | `50` | Newest generated episodic memories retained per workspace; `0` disables pruning. |

## `[collab]` — cross-agent sharing

Multiple agents (Claude Code, Codex, Gemini CLI, …) share one plumb daemon per
machine, and the daemon is the only process that observes every agent's activity
on a workspace. This layer surfaces that **advisorily** — nothing here ever
blocks a write. Project-overridable in both directions (the `generated_summaries`
precedent); no env override; hot-reloaded; strictly per-workspace.

Three tiers, each behind its own flag. **Tier 1 (`peer_awareness`, default on)** is
passive and derived from writes the daemon itself performed or watched — verifiable
**observations**, never agent claims. **Tier 2 (`intents` + `mailbox`, default
off)** adds agent-authored **claims**: `share_intent` and `leave_note` (see
[Cross-agent sharing](tools.md#cross-agent-sharing-collab) in the tool reference).
Claims are always rendered distinctly from observations, secret-scrubbed
(`internal/redact`) before storage, byte-budgeted when injected, and stored in
`<workspace>/.plumb/collab.db` (WAL, auto-gitignored like `topology.db`), created
lazily on first use and pruned on the daemon session-reaper tick (reads filter
expired rows regardless). A workspace whose `intents` and `mailbox` both stay off
never gets a `collab.db`. **Tier 3 (`knowledge_handoff`, default off)** adds
`share_findings`: an on-demand flush of an agent's findings through the
generated-memory pipeline (redacted, provenance-stamped, FTS-indexed, retained
under `[memory] generated_memory_keep`), instantly discoverable by peers via the
ordinary memory channels — no new storage.

When `peer_awareness` is on it adds three signals:

- **Topology-annotated `recent_writes`** — each entry in `workspace_sessions`
  gains its enclosing package/symbol from the topology index (best-effort,
  `source=topology`), so a peer's activity reads as "edited `RateLimiter.Allow` in
  `internal/tools/ratelimit.go`" rather than a bare path.
- **Peer-activity hint** — a path-bearing tool response gains a bounded
  `[Peer: session … edited this file N min ago — consider file_status before
  editing.]` block when another currently-active session recently wrote that file.
  Recency window = `min(idle threshold, 30 min)`.
- **`session_start` peer digest** — when peers are active at attach time, the
  orientation packet gains a short "Active peers" block naming them and the areas
  (directories/packages) they recently touched.

| Field | Type | Default | Effect |
|---|---|---|---|
| `peer_awareness` | bool | `true` | Turn the three tier-1 signals on. Set `false` (globally or per project, either direction) to fall back to bare, unannotated output. |
| `hint_budget_bytes` | int | `512` | Byte cap (UTF-8 boundary) on any injected peer-signal block — the peer-activity hint, the `session_start` peer digest, the intent-aware write hint, and a delivered note body share it. |
| `intents` | bool | `false` | Tier 2, opt-in: the `share_intent` tool, its listing in `workspace_sessions`, and the intent-aware peer write hint. |
| `mailbox` | bool | `false` | Tier 2, opt-in: the `leave_note` tool, note delivery at `session_start`, and pending-note listing in `workspace_sessions`. |
| `knowledge_handoff` | bool | `false` | Tier 3, opt-in: the `share_findings` tool — hand findings to peers now as a generated memory, instead of waiting for the idle episodic summary. |
| `intent_ttl_minutes` | int | `120` | Expiry applied to a new intent or note. Rows past expiry are pruned on the reaper tick and filtered from every read. `0` uses the default. |

## `[semantics]` — opt-in semantic re-rank for `topology_search`

Off by default — zero cost until enabled. When on, `topology_search` re-ranks its
FTS5 candidates by embedding similarity (`mode=fts+semantic`); FTS5 stays the
authoritative spine and any error falls back to plain ranking. **API /
bring-your-own-endpoint only — plumb never bundles, downloads, or supervises a
model.** Project-overridable, hot-reloaded.

Semantic re-rank is **generally available** as of 0.10 — a supported, stable
capability, not an experiment. It stays opt-in (and off by default) only because
it needs an embedding endpoint you supply; nothing about it is provisional.

| Field | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `false` | Turn semantic re-rank on. |
| `provider` | string | `"openai"` | Preset: `openai` \| `voyage` \| `jina` \| `mistral` \| `cohere` \| `custom`. |
| `model` | string | `""` | Embedding model id; `""` uses the preset default. |
| `base_url` | string | `""` | Override the provider API base; **required** for `custom` (Ollama / llama.cpp / LM Studio / TEI / vLLM). |
| `api_key` | string | `""` | Literal key — highest precedence. Prefer `api_key_env`. |
| `api_key_env` | string | `""` | Env var holding the key, used when `api_key` is empty; `""` uses the preset default (e.g. `OPENAI_API_KEY`). |
| `rerank_candidates` | int | `50` | How many FTS5 hits to re-rank. |
| `timeout` | duration | `"10s"` | Per embedding HTTP call. |

## `[lsp_query]` — LSP operation timeout (global only)

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `timeout` | duration | `"30s"` | `PLUMB_LSP_QUERY_TIMEOUT` | Caps a single LSP tool operation when the caller's context carries no deadline, so a wedged language server can't hang a request. `0` disables. |

## `[tools]` — tool advertisement profile

Governs which tools are *advertised* in `tools/list` — a hidden tool stays
callable by name via `tools/call` (hidden ≠ unregistered); this only trims the
advertised set so a client with its own native filesystem tools isn't billed for
commodity duplicates. Project-overridable.

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `profile` | string | `"auto"` | `PLUMB_TOOLS_PROFILE` | `auto` (resolve from the client's native capabilities) \| `lean` (commodity tools hidden) \| `full` (every tool advertised). |
| `client_profiles` | map | `{}` | — | Per-client override, keyed by a case-insensitive `clientInfo.name` prefix (e.g. `"claude-code"`); each value is `auto`\|`lean`\|`full`. An empty or absent entry falls through to `profile`. |

## `[lsp.<language>]` — language servers

A map keyed by language name. **Every supported language is enabled by default**
and activates automatically when its server binary is on `PATH` (checked with
`exec.LookPath`). Installing `rust-analyzer` turns on Rust for every Cargo
project with no config; a language whose server is absent stays dormant at zero
cost and its markers never enter detection.

> **The knob is the opposite of "enable":** set `[lsp.<lang>] enabled = false` to
> *exclude* a language even when its server is installed. `plumb config show`
> prints an `active` row per language (`yes (installed)` /
> `no (… not installed)` / `no (disabled in config)`); `plumb doctor` reports the
> same.

| Field | Type | Effect |
|---|---|---|
| `command` | string | Executable to launch (must be on `PATH`). Required when `enabled`. |
| `args` | []string | Arguments passed to the server. |
| `root_markers` | []string | Files whose presence identifies a workspace of this language. |
| `env` | map | Extra environment variables for the server process. |
| `enabled` | bool | Whether plumb starts this server and detects this language. |
| `idle_timeout` | duration | Hibernate the server (stop its process, keep the warm cache) after this long without a tool call; the next call restarts it. `0` disables. Default `0`, except `java` = `20m`. Restart-needed. |
| `max_workspaces` | int | Cap on concurrently-running servers of this language; the least-recently-used is hibernated before starting another. `0` = unlimited. Default `0`, except `java` = `2`. Restart-needed. |

Built-in defaults (all `enabled = true`; the *effective* set is whichever of
these servers are installed):

| Language | `command` | `root_markers` |
|---|---|---|
| `go` | `gopls` | `go.mod` |
| `python` | `pyright-langserver --stdio` | `pyproject.toml`, `setup.py`, `pyrightconfig.json` |
| `rust` | `rust-analyzer` | `Cargo.toml` |
| `swift` | `sourcekit-lsp` | `Package.swift`, `*.xcodeproj`, `*.xcworkspace` |
| `typescript` | `typescript-language-server --stdio` | `tsconfig.json`, `jsconfig.json` (weak: `package.json`) |
| `java` | `jdtls` (plumb appends `-data <dir>`) | `pom.xml`, `build.gradle`, `build.gradle.kts`, `.classpath` |
| `zig` | `zls` | `build.zig`, `build.zig.zon` |
| `kotlin` | `kotlin-language-server` | `settings.gradle.kts`, `build.gradle.kts` |
| `html` | `vscode-html-language-server --stdio` | weak: `index.html` |

Go and Python are first-class; Java, Rust, Swift, Zig, and TypeScript/JavaScript
are validated; Kotlin and HTML are experimental (see the *Adapter validation
status* table in `AGENTS.md`).

jdtls is heavyweight (~0.8–1.5 GB RSS); it defaults to `idle_timeout = "20m"` and
`max_workspaces = 2` so idle JVMs are hibernated and concurrent JVMs are capped.
If your `jdtls` launcher is not named `jdtls` on `PATH` (e.g. `jdtls.sh`,
`jdtls.bat`, or an absolute path), set `command` accordingly. Use
`plumb debug lsp` to see each server's state, PID, RSS, and idle time.

### Multiple language servers in one project

Enabling more than one language binds them all to the same workspace: a single
root can run several servers at once (e.g. Go + HTML for a web app). Each file is
routed to the server that owns its extension. The **primary** language is the one
resolved from root markers — with both `go.mod` and `index.html` present, `go`
wins — and is started on attach; **secondary** servers start lazily the first
time a file of their language is opened, and the sessions view lists every active
server. So to add HTML support to a Go project:

```toml
[lsp.html]
enabled = true   # gopls stays primary; the HTML server handles .html files
```

`workspace_symbols` and the call/type hierarchies still consult the primary
language only; `diagnostics` aggregates across every server bound to the root.

---

## `[tasks.<language>]` — per-language build/test commands

Five optional command slots per language, keyed by the `[lsp.<lang>]` id, run by
the `run_task` tool and the `plumb build|lint|test|e2e|verify` CLI.

```toml
[tasks.go]
build = "go build ./..."
lint  = "golangci-lint run"
test  = "go test ./..."        # may contain a {target} placeholder
e2e   = "go test -tags=integration ./..."
# verify is a COMPOSITE (build then test); it stores no command of its own
```

A command is a **single argv executed without a shell** — shell metacharacters
(`&&`, `;`, `|`, `$(`, backtick, redirects) are rejected. Shipped defaults exist
for common languages (Go fully populated; a slot is left empty rather than guess
an uninstalled tool). Output and runtime are bounded (100 KiB/200 lines, timeout).

**Trust gate.** A task command supplied by a *project* `.plumb/config.toml` is
not run until the workspace is trusted with `plumb trust` (recorded per workspace
root in `DataDir/trust.json`, never in the project — a cloned repo cannot
self-trust). Default- and global-config commands always run.

## `[[command]]` / `[commands]` — safe command execution

Run workspace commands (build/test/lint/scripts) from within plumb, two ways.

**`run_command` — the safe default.** A named allow-list of **fixed-argv**
commands. The argv is never built from agent free-text, so it is injection-proof
by construction; the one exception is a single `{target}` token, bounded to one
shell-safe argument (`[A-Za-z0-9._/:@-]`).

```toml
[[command]]
name         = "test-one"
exec         = ["go", "test", "-run", "{target}", "./..."]  # fixed argv; optional {target}
working_dir  = "."          # relative to the workspace root; must not escape it
timeout      = "60s"        # default 60s
allow_writes = true         # sandbox: may write inside the workspace (default: only $TMPDIR/caches)
deny_network = false        # sandbox: cut network for this command (default: allowed)
```

**`execute_shell_command` — the opt-in escape hatch.** Runs an arbitrary command
through `sh -c` (pipes/redirects/globs work). It is the one place agent free-text
reaches a command line, so it is **disabled by default**.

```toml
[commands]
allow_shell     = false     # gate for execute_shell_command
require_sandbox = false     # if true, refuse to run (either tool) when no OS sandbox is active
deny_network    = true      # execute_shell_command network egress; default ON — false to allow (a [[command]] sets its own, default false)
```

**Trust gate.** A `[[command]]` entry — and a project raising `[commands]`
`allow_shell` — supplied by a *project* `.plumb/config.toml` is honoured only
after `plumb trust` (recorded per workspace root in `DataDir/trust.json`, never in
the project — a cloned repo cannot self-enable execution). Commands and policy in
your *global* config are user-authored and always honoured. Editing a command in
the TUI Settings **Commands** tab auto-trusts that workspace. A project that
declares its own `[[command]]` block **replaces** the global allow-list entirely
(global entries are shadowed while the project defines any) — to keep a global
command in a project, redefine it there.

**OS sandbox.** Both tools run under a best-effort write jail: reads and process
execution stay permissive (toolchains need them), writes are confined to a
temp/cache set plus the workspace (when `allow_writes`), and the network is cut
only when `deny_network`. macOS uses `sandbox-exec`, Linux uses `bwrap`; when the
sandbox binary is absent the command runs unsandboxed with a clear status note
(set `require_sandbox = true` to refuse instead). plumb's own runtime dir
(`<cache>/plumb`) is excluded from the writable set so a command cannot clobber
the daemon's socket/locks. Output and runtime are bounded (100 KiB/200 lines,
timeout).

**Two limits to understand.** (1) The sandbox is **integrity-only, not
confidentiality**: reads stay permissive and a command inherits the daemon's
environment, so an enabled+trusted `execute_shell_command` can *read* any file or
secret your user can (`~/.ssh`, API keys in the daemon env). To bound the damage,
the shell tier **denies the network by default** (`[commands] deny_network =
true`) so a read secret cannot be exfiltrated over the wire; set `deny_network =
false` (in global config, or a trusted project) only when a command genuinely
needs the network. When a command runs with the network off, the tool's reply
says `network=off` with a note, so the agent can tell you to flip it. Still: only
enable the shell tier for repositories you trust. (A `[[command]]` entry sets its
own per-command `deny_network`, default false, since those are deliberate.) (2) The writable set is tuned for **Go** (build
cache, module cache, `$TMPDIR`, the workspace). Other toolchains that write
outside those (e.g. `cargo`'s `~/.cargo/registry`, `npm`'s cache) may need
`allow_writes` and may fail under `require_sandbox = true`; only Go is validated.
Commands inherit the daemon's environment so `go`/`npm`/linters find their
toolchain.

## `agent_config_writes` — agent-writable config (top level)

```toml
agent_config_writes = false   # default off; user-settable only
```

When `true`, the `agent_config` tool may write a small allowlist of project
config keys: the `[tasks.<lang>]` slots plus `log_level`, `ui.theme`,
`ui.path_style`, `topology.exclude_patterns`, `quality.analysers`. Every other
key — including this knob itself and all safety guardrails — is never
agent-writable. Agent writes are validated and applied atomically, tagged
`provenance=agent` in a (gitignored) `.plumb/config.provenance.json` sidecar,
shown by `plumb config show`, and revertible with `plumb config unset <key>`.
The knob is editable only by the user (e.g. the TUI Settings screen).

---

## Environment variables

Environment variables are the highest-precedence layer. Booleans accept
`1`/`true`/`yes`; `PLUMB_SHOW_WRITE_DIFF` and `PLUMB_GIT_ALLOW_WRITES` instead
treat `0`/`false`/`no` as off (default on otherwise).

| Variable | Overrides |
|---|---|
| `PLUMB_LOG_LEVEL` | `log_level` |
| `PLUMB_LOG_FORMAT` | `log_format` |
| `PLUMB_LOG_FILE` | `log_file` |
| `PLUMB_STRICT_EDITS` | `edits.strict` |
| `PLUMB_WRITE_RATE_LIMIT` | `edits.rate_limit_per_minute` |
| `PLUMB_POST_WRITE_DIAG_MS` | `edits.post_write_diagnostics_ms` |
| `PLUMB_POST_WRITE_CROSS_FILE` | `edits.post_write_cross_file` |
| `PLUMB_POST_WRITE_CROSS_FILE_SETTLE_MS` | `edits.post_write_cross_file_settle_ms` |
| `PLUMB_CONCURRENT_WRITE_SKEW_MS` | `edits.concurrent_write_skew_ms` |
| `PLUMB_SHOW_WRITE_DIFF` | `edits.show_write_diff` |
| `PLUMB_BLOCK_DIRTY_WRITES` | `edits.block_dirty_writes` |
| `PLUMB_REFUSE_HOME_ROOTS` | `walk.refuse_home_roots` |
| `PLUMB_GIT_ALLOW_WRITES` | `git.allow_writes` |
| `PLUMB_GIT_ALLOW_DESTRUCTIVE` | `git.allow_destructive` |
| `PLUMB_GIT_ALLOW_PUSH` | `git.allow_push` |
| `PLUMB_AUTO_ATTACH` | `workspace.auto_attach` |
| `PLUMB_AUTO_ATTACH_PERSIST` | `workspace.auto_attach_persist` |
| `PLUMB_LSP_QUERY_TIMEOUT` | `lsp_query.timeout` |
| `PLUMB_TOOLS_PROFILE` | `tools.profile` |
| `PLUMB_PERSIST_SESSION_STATE` | `session.persist_state` |

---

## Validation rules

`plumb` refuses to start with an invalid config (and reports it via
`plumb doctor`):

- `log_level` ∈ {`debug`, `info`, `warn`, `error`}; `log_format` ∈ {`text`, `json`}.
- `cache.max_size`, all `edits.*_ms`, `edits.rate_limit_per_minute`,
  `quality.timeout_ms`, `quality.max_findings_per_file`, and `lsp_query.timeout`
  must be non-negative.
- `quality.mode` ∈ {`background`, `sync`} (empty allowed → default).
- An enabled `[lsp.<language>]` must set `command`.

---

## Annotated sample `config.toml`

Every value below is the compiled-in default — copy only the lines you want to
change.

```toml
log_level  = "info"      # debug | info | warn | error
log_format = "text"      # text | json
log_file   = ""          # empty = daemon log under the OS log dir (~/Library/Logs/plumb on macOS)

[ui]
theme = "plumb"          # global only; set via the TUI Settings picker

[cache]
ttl      = "5m"
max_size = 1000

[edits]
strict                    = false   # require read_file before edit_file
rate_limit_per_minute     = 120     # 0 disables
post_write_diagnostics_ms = 300     # ceiling; effective wait adapts down to observed latency; 0 disables
post_write_cross_file          = true  # flag NEW errors the edit introduced in OTHER files (edit A breaks B)
post_write_cross_file_settle_ms = 200  # bounded grace for dependent-file re-publishes; 0 compares immediately
concurrent_write_skew_ms  = 100     # clock-skew allowance for concurrent-write detection
show_write_diff           = true    # append a unified diff to write/edit responses

[walk]
refuse_home_roots = true            # macOS TCC guard; no-op elsewhere

[workspace]
auto_attach            = false      # synthetic-root fallback when no marker found
auto_attach_persist    = false      # create .plumb/ at the synthetic root (implies auto_attach)
allow_dependency_reads = true       # read/search the Go module cache (GOMODCACHE) + GOROOT read-only; writes there always refused
extra_roots            = []         # additional read-WRITE directories, additive to the workspace ($VAR-expanded)
read_roots             = []         # additional read-ONLY directories (vendored deps, shared libs), additive ($VAR-expanded)

[git]
allow_writes       = true                   # add, commit, switch, branch/tag create, stash
allow_destructive  = false                  # reset, clean, checkout… (also needs confirm:true)
allow_push         = false                  # push, fetch, pull (also needs confirm:true)
protected_branches = ["main", "master"]     # never force-pushable

[quality]
enabled               = false               # post-write offline analysers
mode                  = "background"         # background | sync
analysers             = ["golangci-lint"]
timeout_ms            = 2000
max_findings_per_file = 5

[topology]
enabled                 = true              # on by default; set false to opt out
resync_on_attach        = false
exclude_patterns        = []
max_file_size_bytes     = 524288            # 512 KiB
resync_batch            = 100               # files per pause during a full resync (0 disables)
resync_pause_ms         = 25                # pause after each batch, ms (0 disables)
resync_interval_minutes = 60                # periodic full resync FALLBACK (suppressed while watch is on); 0 disables
watch                   = true              # OS-level file watching: re-index on change, whoever made it

[session]
idle_threshold_minutes    = 30              # TUI idle marker threshold (cosmetic)
eviction_ttl_minutes      = 60              # daemon force-closes a connection idle this long; 0 disables
persist_state             = true            # persist read-tracking + pinned workspace across a daemon restart (env PLUMB_PERSIST_SESSION_STATE)
persist_state_ttl_minutes = 1440            # how long persisted per-connection state lingers before pruning; 0 disables pruning

[lsp_query]
timeout = "30s"          # per-operation cap; 0 disables; global only

[lsp.go]
command      = "gopls"
args         = []
root_markers = ["go.mod"]
enabled      = true

[lsp.python]
command      = "pyright-langserver"
args         = ["--stdio"]
root_markers = ["pyproject.toml", "setup.py", "pyrightconfig.json"]
enabled      = true      # auto-activates when pyright-langserver is on PATH; false excludes

[lsp.java]
command      = "jdtls"
args         = []
root_markers = ["pom.xml", "build.gradle", "build.gradle.kts", ".classpath"]
enabled      = true      # auto-activates when jdtls (+ Java 21+) is on PATH; false excludes

# rust, swift, typescript, zig, kotlin, and html share the same shape and are
# also enabled by default — each activates when its server binary is on PATH.
```
