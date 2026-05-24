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
   `~/.config/plumb/config.toml`. Loaded once at daemon start.
3. **Project config** — `<workspace>/.plumb/config.toml`. Loaded when a
   connection's workspace resolves and merged onto the global config. A project
   file that sets one field inherits the rest.
4. **Environment variables** — highest precedence; useful for one-off overrides
   without editing files.

The `[edits]`, `[walk]`, and `[git]` sections are **hot-reloaded**: the daemon
polls the project `config.toml` every 30 seconds and re-applies changes without
a reconnect. `[ui]`, `[lsp_query]`, and the `[lsp.*]` servers are **global-only**
(read once at daemon start).

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
| `log_file` | string | `""` | `PLUMB_LOG_FILE` | Empty writes to the daemon log under the cache dir. |

## `[ui]` — TUI presentation (global only)

| Field | Type | Default | Effect |
|---|---|---|---|
| `theme` | string | `"nordico"` | Active colour theme. Set interactively via the TUI **Settings** picker, which persists it here. |

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
| `post_write_diagnostics_ms` | int | `300` | `PLUMB_POST_WRITE_DIAG_MS` | How long to wait for the LSP server to re-publish diagnostics after a write. `0` disables the wait. |
| `concurrent_write_skew_ms` | int | `100` | `PLUMB_CONCURRENT_WRITE_SKEW_MS` | Clock-skew allowance for `edit_file`'s concurrent-write detector. Raise on slow/network filesystems. |
| `show_write_diff` | bool | `true` | `PLUMB_SHOW_WRITE_DIFF` | Append a unified diff to `edit_file`/`write_file` responses. Set false to return only metadata. |

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
| `enabled` | bool | `false` | Turn on the persistent SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`. See the [Topology guide](topology.md). |
| `resync_on_attach` | bool | `false` | Force a full resync each time the workspace attaches. |
| `exclude_patterns` | []string | `[]` | Path glob patterns to skip during indexing. |
| `max_file_size_bytes` | int64 | `524288` (512 KiB) | Largest file considered for extraction. `0` uses the default. |
| `resync_batch` | int | `100` | Files the full resync extracts before pausing, to throttle CPU. `0` disables pacing. |
| `resync_pause_ms` | int | `25` | Pause (milliseconds) after each `resync_batch` files. `0` disables pacing. |
| `resync_interval_minutes` | int | `60` | Interval between full resyncs (for enabled workspaces). `0` disables periodic resync. |

## `[lsp_query]` — LSP operation timeout (global only)

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `timeout` | duration | `"30s"` | `PLUMB_LSP_QUERY_TIMEOUT` | Caps a single LSP tool operation when the caller's context carries no deadline, so a wedged language server can't hang a request. `0` disables. |

## `[lsp.<language>]` — language servers

A map keyed by language name. Go is enabled by default; **Python and Java are
disabled by default**.

> **Important:** installing a language-server binary is not enough. Plumb only
> recognises a language and starts its server when that language's
> `enabled = true`. To use Python, install `pyright` **and** set
> `[lsp.python] enabled = true`. Likewise for Java (`jdtls` + Java 21+).

| Field | Type | Effect |
|---|---|---|
| `command` | string | Executable to launch (must be on `PATH`). Required when `enabled`. |
| `args` | []string | Arguments passed to the server. |
| `root_markers` | []string | Files whose presence identifies a workspace of this language. |
| `env` | map | Extra environment variables for the server process. |
| `enabled` | bool | Whether plumb starts this server and detects this language. |

Built-in defaults:

| Language | `command` | `args` | `root_markers` | `enabled` |
|---|---|---|---|---|
| `go` | `gopls` | `[]` | `go.mod` | **`true`** |
| `python` | `pyright-langserver` | `--stdio` | `pyproject.toml`, `setup.py`, `pyrightconfig.json` | `false` |
| `java` | `jdtls` | `[]` (plumb appends `-data <dir>`) | `pom.xml`, `build.gradle`, `build.gradle.kts`, `.classpath` | `false` |

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
| `PLUMB_CONCURRENT_WRITE_SKEW_MS` | `edits.concurrent_write_skew_ms` |
| `PLUMB_SHOW_WRITE_DIFF` | `edits.show_write_diff` |
| `PLUMB_REFUSE_HOME_ROOTS` | `walk.refuse_home_roots` |
| `PLUMB_GIT_ALLOW_WRITES` | `git.allow_writes` |
| `PLUMB_GIT_ALLOW_DESTRUCTIVE` | `git.allow_destructive` |
| `PLUMB_GIT_ALLOW_PUSH` | `git.allow_push` |
| `PLUMB_AUTO_ATTACH` | `workspace.auto_attach` |
| `PLUMB_AUTO_ATTACH_PERSIST` | `workspace.auto_attach_persist` |
| `PLUMB_LSP_QUERY_TIMEOUT` | `lsp_query.timeout` |

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
log_file   = ""          # empty = daemon log under the cache dir

[ui]
theme = "nordico"        # global only; set via the TUI Settings picker

[cache]
ttl      = "5m"
max_size = 1000

[edits]
strict                    = false   # require read_file before edit_file
rate_limit_per_minute     = 120     # 0 disables
post_write_diagnostics_ms = 300     # wait for LSP diagnostics after a write; 0 disables
concurrent_write_skew_ms  = 100     # clock-skew allowance for concurrent-write detection
show_write_diff           = true    # append a unified diff to write/edit responses

[walk]
refuse_home_roots = true            # macOS TCC guard; no-op elsewhere

[workspace]
auto_attach         = false         # synthetic-root fallback when no marker found
auto_attach_persist = false         # create .plumb/ at the synthetic root (implies auto_attach)

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
enabled                 = false             # persistent semantic index
resync_on_attach        = false
exclude_patterns        = []
max_file_size_bytes     = 524288            # 512 KiB
resync_batch            = 100               # files per pause during a full resync (0 disables)
resync_pause_ms         = 25                # pause after each batch, ms (0 disables)
resync_interval_minutes = 60                # periodic full resync; 0 disables

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
enabled      = false     # install pyright AND set true to activate Python

[lsp.java]
command      = "jdtls"
args         = []
root_markers = ["pom.xml", "build.gradle", "build.gradle.kts", ".classpath"]
enabled      = false     # install jdtls + Java 21+ AND set true to activate Java
```
