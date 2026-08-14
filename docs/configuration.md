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
`[memory]`, `[collab]`, and `[semantics]` apply live. `[xcode]` is evaluated
once per workspace on the next session, because enabling it may launch trusted
project-sensitive tooling. The restart-bound exceptions are the `[lsp.*]`
servers, `[cache]`, and `log_format`; `plumb config show` and `daemon_info` flag
those as restart-needed.

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
| `path_style` | string | `"compact"` | How workspace folder paths are abbreviated in the Sessions sidebar: `compact`, `truncate-middle`, or `full`. |
| `keys` | table | `{}` | Rebinds TUI keyboard shortcuts — an action-name → key-string map. See below. |

Built-in themes: `nordico`, `darcula`, `dracula`, `gruvbox`, `plumb` (dark);
`github-light`, `solarized-light`, `plumb-light` (light). The `plumb`/
`plumb-light` pair is derived from the project website's own terracotta/sage
palette (`site/index.html`). The theme picker writes the setting live via a
sparse `SetGlobalValue(["ui", "theme"])` that rewrites only the `[ui].theme` key
(preserving the rest of the file and never baking in `PLUMB_*` env overrides);
the whole-file `config.Save` path that other global Settings writes use still
rewrites the file, so user TOML comments are lost on first save there. The
palette catalogue lives in the UI-agnostic `internal/theme` package (hex
strings, no bubbletea import), consumed by both the TUI and the web UI.

### `[ui.keys]` — keyboard shortcuts

Rebind TUI actions by mapping a stable **action name** to a **key string**.
Global config only, like the rest of `[ui]`; project config is ignored. Any
action left unset keeps its default, so an empty or absent table changes
nothing.

```toml
[ui.keys]
quit         = "ctrl+w"   # default ctrl+q
section_menu = "s"        # default /
refresh      = "R"        # default a
nav_up       = "w"        # default up  (the vim alias k stays active)
nav_down     = "s"        # default down (the vim alias j stays active)
```

**Rebindable actions** (the full set):

| Action | Default | What it does |
|---|---|---|
| `quit` | `ctrl+q` | Quit immediately |
| `nav_up` | `up` | Move selection / scroll up |
| `nav_down` | `down` | Move selection / scroll down |
| `page_up` | `pgup` | Page up |
| `page_down` | `pgdown` | Page down |
| `section_menu` | `/` | Open the section selector |
| `panel_next` | `tab` | Cycle focus / tab forward |
| `panel_prev` | `shift+tab` | Cycle focus / tab backward |
| `refresh` | `a` | Refresh sessions and stats |
| `help` | `ctrl+h` | Open the help overlay |
| `rename` | `r` | Rename the selected session |
| `filter` | `f` | Filter the memories list |

**Fixed keys** (not rebindable — a binding onto one is reported and ignored):
`ctrl+c` (interrupt / quit confirmation), `esc`, `enter`, `space`, the vim
aliases `j`/`k`, the section shortcuts `ctrl+1`–`ctrl+5` / `alt+1`–`alt+5`,
`ctrl+t` (theme picker), `c` (copy), `G` (jump to latest log), `[` / `]`
(resize), and `backspace` (filter editing). Rebinding the arrow/named default of
`nav_up`/`nav_down` moves that key but leaves the `k`/`j` vim aliases active.

**Conflict handling** (all reported to stderr at startup and ignored, defaults
left intact):

- An **unknown action name** is warned about (listing the valid actions) and skipped.
- A key that **collides with a fixed key** is warned about (naming the fixed behaviour) and skipped.
- When **two actions request the same key**, the warning names both and the later one — deterministically, the alphabetically-greater action name — is dropped; the earlier keeps the key.
- When a binding **shadows another action's default** (e.g. `refresh = "up"` takes the arrow from `nav_up`), the explicit binding wins, the shadowed action is left with no key, and the displacement is reported. The displaced default then dispatches nothing.

The help overlay (`ctrl+h`) and the Sessions footer render the live keys, so
they always reflect your bindings. Editing `[ui.keys]` from the Settings screen
is not yet supported — edit the global `config.toml` directly for now.

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
| `strict` | bool | `false` | `PLUMB_STRICT_EDITS` | Require every content-authoring write target — `edit_file` and the four symbol-edit tools (`replace_symbol_body`, `insert_before_symbol`, `insert_after_symbol`, `safe_delete_symbol`) — to have been read via `read_file` this session, with a matching mtime. `rename_symbol` is exempt (see below). |
| `rate_limit_per_minute` | int | `120` | `PLUMB_WRITE_RATE_LIMIT` | Sliding-window cap on writes per session. `0` disables. A shared parent budget (keyed by (client, workspace)) caps the combined rate across connections from the same client to one project. |
| `post_write_diagnostics_ms` | int | `300` | `PLUMB_POST_WRITE_DIAG_MS` | Ceiling on how long to wait for the LSP server to re-publish diagnostics after a write; the effective wait adapts down to the server's observed latency. `0` disables. |
| `post_write_cross_file` | bool | `true` | `PLUMB_POST_WRITE_CROSS_FILE` | After a write, compare workspace diagnostics against a pre-write baseline and flag NEW errors the edit introduced in OTHER files (the "edit A silently breaks B" case). The edited file's own diagnostics block keeps priority. |
| `post_write_cross_file_settle_ms` | int | `200` | `PLUMB_POST_WRITE_CROSS_FILE_SETTLE_MS` | Bounded grace the cross-file sweep waits, after the edited file's own diagnostics land, for dependent-file re-publishes before comparing. `0` compares immediately. |
| `concurrent_write_skew_ms` | int | `100` | `PLUMB_CONCURRENT_WRITE_SKEW_MS` | Clock-skew allowance for `edit_file`'s concurrent-write detector. Raise on slow/network filesystems. |
| `show_write_diff` | bool | `true` | `PLUMB_SHOW_WRITE_DIFF` | Append a unified diff to `edit_file`/`write_file` responses. Set false to return only metadata. |
| `block_dirty_writes` | bool | `true` | `PLUMB_BLOCK_DIRTY_WRITES` | Refuse a destructive write (`write_file`, `edit_file`, `delete_file`, `find_replace`, `rename_file`, `copy_file`, `transaction_apply`) to a file with uncommitted git changes that plumb did not write this session, unless `dirty_ok: true`. Set false to disable the guard — for a workflow that iterates on uncommitted WIP. Re-editing a file plumb wrote this session is never blocked either way. |
| `fsync` | bool | `true` | `PLUMB_FSYNC` | Fsync-before-ack: fsync the staged temp file before the atomic rename and the parent directory after it, so an acknowledged write (and plumb's own state files) survive a hard crash or power cut. Set false to skip both fsyncs — restores the old behaviour for benchmarks and exotic filesystems that refuse fsync. **Daemon-global** (the only `[edits]` key that is): it gates write primitives shared by every session, so it is resolved from global config (or `PLUMB_FSYNC`) once at daemon start and re-read live on a global reload; a per-project `.plumb/config.toml` override is ignored, because honouring it would let the last workspace to attach set the durability contract for every other live session. |

### Cross-file diagnostics honesty

The cross-file sweep is honest by construction: it only reports a file whose error count ROSE versus the pre-write baseline AND that the language server re-published after the write, so pre-existing errors and untouched files are never mis-attributed; the edited file's own block is unaffected and returned first, and the heads-up hedges the mid-series case rather than claiming "the build is broken".

### Strict mode and `rename_symbol`

`strict = true` asks a single question before every write: *has this session read
the file it is about to author content into?* It therefore covers `edit_file` and
the four symbol-edit tools, all of which write agent-authored text into one named
file.

`rename_symbol` is **deliberately exempt**. It authors no content — the new text
is a single identifier — and the file set is computed by the language server, not
chosen by the agent. Requiring a prior `read_file` of every file in a forty-file
rename would make the tool unusable under strict mode, and requiring it of the
anchor file alone would guarantee nothing, since the rename's ranges are resolved
server-side regardless of what the agent read. The dirty guard
(`block_dirty_writes`) is what protects unreviewed work from a rename.

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
| `allow_dependency_reads` | bool | `true` | — | Allow read/search (never write) to reach the session language's toolchain stdlib + dependency cache read-only (Go: GOMODCACHE/GOROOT; Zig: stdlib + cache; Rust: rust-src + cargo registry; Python: stdlib + site-packages; Swift: SDK; JVM: Gradle/Maven caches). TypeScript is intentionally excluded (node_modules is in-workspace). |
| `extra_roots` | []string | `[]` | — | Additional read-**write** directories, additive to the workspace (`$VAR`-expanded). Honoured from **global** config only (see below). |
| `read_roots` | []string | `[]` | — | Additional read-**only** directories — vendored deps, shared libs (`$VAR`-expanded). Honoured from **global** config only (see below). |
| `child_scan_depth` | int | `2` | — | Levels below a markerless `.plumb/` root to scan for language markers in subdirectories (multi-language monorepo). `0` disables. See [Architecture → Workspace detection](architecture.md#workspace-detection). |

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

The workspace boundary itself is enforced per-connection by a **`PathPolicy`**
(`internal/tools/pathpolicy.go`): an allowlist of roots tagged read-only or
read-write. The detected workspace is always read-write; `extra_roots` add
read-write roots; `read_roots` (and, with `allow_dependency_reads`, the session
language's toolchain stdlib + dependency cache) add read-only roots. Read/search
tools admit any allowed root; write tools demand read-write, so a write outside
the workspace is refused by construction.

## `[git]` — tiered git-tool gating

The `git` tool's read tier always runs. Higher tiers are gated here; destructive
and network calls additionally require `confirm: true` per call.

> **Per-workspace trust required.** `[git]` is a safety policy, so a project's
> `.plumb/config.toml` may set it only for a workspace you have approved with
> `plumb trust` — a cloned repository would otherwise grant itself history
> destruction and pushes with your credentials the moment a session attaches.
> Until then the whole block falls back to your global config, and `plumb doctor`,
> `plumb config show` and `session_start`'s git-policy section all report which
> keys are being ignored. See [Project-config trust](#project-config-trust).

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `allow_writes` | bool | `true` | `PLUMB_GIT_ALLOW_WRITES` | Safe-write tier: `add`, `commit`, `switch`, `branch`/`tag` create, `stash` push/pop. |
| `allow_destructive` | bool | `false` | `PLUMB_GIT_ALLOW_DESTRUCTIVE` | Destructive tier: `reset`, `clean`, `checkout`, `restore`, `rebase`, `revert`, `cherry-pick`, branch/tag delete, `stash` drop. Also needs `confirm:true`. |
| `allow_push` | bool | `false` | `PLUMB_GIT_ALLOW_PUSH` | Network tier: `push`, `fetch`, `pull`. Also needs `confirm:true`. |
| `protected_branches` | []string | `["main", "master"]` | — | Branch names that may never be force-pushed, even with `allow_push` + `confirm`. |
| `commit_trailer` | bool | `false` | `PLUMB_GIT_COMMIT_TRAILER` | Stamp each plumb-mediated commit with a `Plumb-Session: <session-name>` trailer, attributing it to the authoring agent session. **Requires git ≥ 2.32** — `git commit --trailer` does not exist on older git, and plumb runs no version probe, so enabling this against an older binary fails every commit issued through the tool. Attribution is queryable without it — `workspace_sessions` lists recent commits per session either way. |
| `env` | table | `{}` | — | Environment variables set on the git child process. See [The git child's environment](#the-git-childs-environment) below. |
| `write_timeout` | duration | `"10m"` | `PLUMB_GIT_WRITE_TIMEOUT` | How long plumb waits for an index/ref-mutating git child before killing it. See [When plumb stops waiting](#when-plumb-stops-waiting) below. |

### When plumb stops waiting

A write- or destructive-tier git child is decoupled from request and daemon
cancellation, so that a shutdown mid-commit lets git finish rather than
stranding a half-written index. Something still has to bound it, and
`write_timeout` is that bound.

The bound was previously a hardcoded two minutes, described in the source as
"generous enough for a slow pre-commit hook". That is false on any machine where
several agents share one toolchain: a hook running `golangci-lint` queues behind
a peer's run on the same shared cache, and those waits have been observed to
exceed ten minutes on plumb's own repository.

**A wrong bound is expensive in both directions**, which is why this is a knob
rather than a better constant. Too short kills a hook that was going to succeed.
Too long leaves a wedged child holding the per-repo lock, so every other git
operation on that repository queues behind it. There is deliberately **no value
that disables the bound** — an unbounded child is the failure this exists to
prevent.

**When it fires, plumb says so.** A killed child exits signalled, which git
reports as exit code `-1`; plumb used to render that as an ordinary git failure
under a remediation stating that no plumb setting changes the outcome. That
inverts the truth — plumb's own bound caused it, and this is the setting. The
timeout is now reported as plumb's, names the elapsed bound, and points here.

It is trust-gated for the same reason as the rest of `[git]`: shortening it is a
denial of service on every commit, and lengthening it lets a hostile hook hold
the repository lock. Neither is a choice a cloned repository's
`.plumb/config.toml` should make unasked.

### The git child's environment

The daemon is long-lived and its environment is whatever started it — a login
shell, an editor, a launchd job. Everything it spawns inherits that verbatim,
including the `git` process that runs **your repository's hooks**. When the
inherited environment is wrong for the repository, the hook is what breaks:

```toml
[git]
env = { GOWORK = "off" }
```

That example is the motivating one. Go discovers a workspace by walking *up*
from the process's working directory, and plumb runs git with `cwd` set to the
repository root — so a `go.work` above a git worktree or submodule silently
swallows the module, and a pre-commit hook running `go build ./...` fails on
every commit. Before this knob the only way out was to leave plumb and run
`GOWORK=off git commit` in a shell.

**It extends, it does not replace.** Entries are applied on top of the inherited
environment, so `PATH` (git finding its own subcommands), `HOME` (`~/.gitconfig`,
`known_hosts`) and `SSH_AUTH_SOCK` (authenticating a fetch or push) survive
untouched. An entry whose name is already present replaces that value — that is
the point, `GOWORK = "off"` has to beat an inherited `GOWORK`. There is no way to
*unset* an inherited variable; setting a name to `""` sets it to the empty
string. With no entries the child inherits exactly as it always did.

It applies to the git process plumb runs on your behalf — the one that runs
hooks and can open an editor. The auxiliary read queries around it (`ls-files`,
`log -1`, `rev-parse`, `diff --cached`) are plumbing whose output plumb parses,
and deliberately keep inheriting.

**A project's entries compose with your global ones**, the way every other
setting in this file does: the project's value wins for the names it sets, and a
global entry it does not mention survives. Any of the three TOML spellings gives
the same result — `env = { X = "y" }` under `[git]`, a `[git.env]` sub-table, or
a `git.env.X = "y"` dotted key. (TOML's own unmarshalling does not: an inline
table replaces a map where the other two merge into it. plumb normalises that,
so a project cannot drop one of your global entries by choosing a spelling.)

> **This is a capability, not a preference, and it is gated as one.** A git
> child's environment can name commands git will run — `GIT_SSH_COMMAND`,
> `GIT_EXTERNAL_DIFF`, `GIT_PROXY_COMMAND` and `GIT_PAGER` all do, and
> `GOFLAGS=-toolexec=…` reaches any `go` a hook invokes. It lives inside `[git]`
> so it inherits that block's per-workspace trust boundary exactly: a cloned
> repository's `.plumb/config.toml` cannot set it until you have approved that
> exact content with `plumb trust`, and `plumb trust` discloses the variables and
> their values. Note that no allowlist of "safe" variable names is offered as a
> substitute — the dangerous set is open-ended and reaches into other tools'
> variables entirely, so the trust boundary is the whole mechanism.

**Editors.** `git rebase -i` and `git tag -a` invoke `GIT_EDITOR`
unconditionally, and plumb passes it no terminal, so the editor blocks. plumb
does not set `GIT_EDITOR` for you — that would silently accept a default commit
message you never wrote. Set it yourself if you want those verbs to be
non-interactive:

```toml
[git]
env = { GIT_EDITOR = "true" }
```

Either way plumb no longer hangs on it: the child wait is bounded (5s past the
child's exit), and cancellation kills the whole process group rather than the
direct child alone. If a process the command started outlives git while still
holding its output pipes, plumb stops waiting and says so — quoting git's own
output, so you can see whether the operation completed.

Ambiguous subcommands (`checkout`, `switch`, `restore`, `branch`, `tag`,
`stash`) are classified by their arguments and biased towards the higher tier —
e.g. `checkout -b` is a write but any other `checkout` is destructive, and
`restore --staged` is a write but `restore --worktree` is destructive. `add` and
`commit` are typed (only `commit -m <message>` / `add -- <files>` ever run, so
`--amend`/`--no-verify`/globs are unreachable; pre-commit hooks always run). A
denylist rejects global flags that would reconfigure git (`-c`/`-C`/`--git-dir`/
`--work-tree`/etc.), and no shell is involved; output is capped (200 lines for
`log`/`blame`, 100 KiB overall). See
[Tools → `git`](tools.md#git) for the full behavioural contract.

## `[quality]` — post-write code analysis

| Field | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `false` | Run offline analysers against changed files; findings appended to write responses. |
| `mode` | string | `"background"` | `background` (findings on the next request) or `sync` (block up to `timeout_ms` and append inline). |
| `analysers` | []string | `["golangci-lint"]` | Which analysers to run. Unknown names are skipped. |
| `timeout_ms` | int | `2000` | Per-analyser run cap. |
| `max_findings_per_file` | int | `5` | Cap on findings appended per file. |

The `golangci-lint` analyser needs the binary itself. plumb looks for it on
`PATH` first, then in the Go tool bin directory — `$GOBIN`, else `$GOPATH/bin`,
else `~/go/bin` — because the daemon inherits the environment of whichever
`plumb serve` proxy started it, which frequently lacks `~/go/bin` even when your
shell has it. If it is found nowhere, writes still succeed, findings are simply
absent, and the daemon log says so **once** (`quality: golangci-lint not found`)
rather than leaving you to wonder. `plumb doctor`'s **Dev Tools** section
reports the resolved path, or warns with a fix hint when there is none.

## `[topology]` — semantic index

| Field | Type | Default | Effect |
|---|---|---|---|
| `enabled` | bool | `true` | The persistent SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`. On by default; set `false` to opt out (per-project or global). The index is created on first attach — the one case where plumb materialises `.plumb/`. See the [Topology guide](topology.md). |
| `resync_on_attach` | bool | `false` | Force a full resync each time the workspace attaches. |
| `exclude_patterns` | []string | `[]` | Path glob patterns to skip during indexing. |
| `max_file_size_bytes` | int64 | `524288` (512 KiB) | Largest file considered for extraction. `0` uses the default. |
| `extract_timeout_seconds` | int | `10` | Longest one file's parse may take before it is abandoned, recorded as a file error, and the indexer moves on. Bounded by a built-in 2-minute ceiling: this setting can LOWER the bound but not remove it, because an unbounded parse can wedge the single indexer worker permanently. `0` means "use the ceiling", not "unbounded". Size caps bound how much source a grammar sees, not how long it spends: a pathological error-recovery path can burn tens of seconds on a small file, and the indexer runs one worker, so that would stall the whole index. |
| `resync_batch` | int | `100` | Files the full resync extracts before pausing, to throttle CPU. `0` disables pacing. |
| `resync_pause_ms` | int | `25` | Pause (milliseconds) after each `resync_batch` files. `0` disables pacing. |
| `resync_interval_minutes` | int | `60` | Periodic full-resync **fallback**, used only when `watch = false` or the platform watcher cannot start; suppressed while the watcher is live. `0` disables. |
| `watch` | bool | `true` | OS-level file watching ([`fswatcher`](https://github.com/sgtdi/fswatcher)): re-index a file the instant it changes on disk, whoever changed it — this agent, another agent, or your editor. Replaces time-based polling; a mass change (e.g. `git checkout`) coalesces to a single paced resync via the bounded queue + overflow path. Set `false` to fall back to `resync_interval_minutes`. |

Note the two different meanings of `0` in this table: `max_file_size_bytes = 0` means *use the default*, and `extract_timeout_seconds = 0` means *use the built-in 2-minute ceiling* -- it no longer disables bounding entirely, since an unbounded parse can wedge the indexer.

`topology.db` (+ `-wal`/`-shm`) is auto-added to `<workspace>/.plumb/.gitignore`,
and `.plumb/` itself is excluded from watching. Per-project `[topology]` config
is honoured on attach and re-applied on reload. Only the full resync walk is
paced; write-triggered upserts are never delayed.

## `[session]` — idle detection & eviction

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `idle_threshold_minutes` | int | `30` | — | How long after the last tool call a session is shown idle (a `~` marker) in the TUI Sessions panel. Cosmetic. |
| `eviction_ttl_minutes` | int | `60` | — | How long after the last tool call the daemon force-closes an idle connection — reclaiming a `plumb serve` whose agent silently disconnected but kept its stdio pipe open. A reaper checks every 5 min (fixed). `0` disables eviction. Read live (hot-reloaded). |
| `persist_state` | bool | `true` | `PLUMB_PERSIST_SESSION_STATE` | Persist a connection's session state (pinned workspace, strict-mode read-tracking, session name) to disk so it survives a daemon restart/upgrade transparently, instead of resetting on reconnect. |
| `persist_state_ttl_minutes` | int | `1440` | — | How long persisted session state is honoured on restart before it's treated as stale and discarded. |

Global or per-project; no environment override except `persist_state`. Activity is a tool call: the session file's mtime is advanced after each call (`session.Touch`) and read back as the last-seen time.

`persist_state` (default on; env `PLUMB_PERSIST_SESSION_STATE`) makes a **daemon restart transparent to a connected agent**: strict-mode read-tracking, the pinned workspace, and the session name are written to `session_state.db` (in the data dir, beside `stats.db`), keyed by a stable proxy session ID that `plumb serve` injects into the `initialize` handshake `_meta` and replays on every reconnect. On reconnect the fresh daemon rehydrates that state, so a strict-mode `edit_file` of a file read before the restart is not refused, a client that reports no roots (e.g. Claude Desktop) comes back pinned without an explicit `session_start`, and the connection keeps its session name and its **mailbox identity**. Both matter for messages: a note is addressed by name, so a fresh random name on every reconnect would orphan it, and a note to a peer that was live when it was sent is *bound* to that peer's session ID, so the reconnected connection — which registers under a new ID — also inherits its predecessor's ID in order to collect mail written before the restart. That inheritance is granted **only** by presenting the proxy session ID, never by answering to a name; a session that merely takes a free name inherits nothing, which is what keeps `addressee_id` a real boundary. The chain is bounded at one predecessor: each reconnect records its own ID, so a message unread across two restarts expires rather than being inherited indefinitely. Rehydration is **safe by construction**: a restored read still passes `checkStrictRead`'s on-disk `os.Stat`+mtime comparison, so it can only satisfy an unchanged file, never bypass a dirty-file check. Read-tracking is scoped by `(proxy session, workspace)`, so a re-pin to a different project never resurrects the old project's reads. `persist_state_ttl_minutes` (config-only, default 24h; `0` disables pruning) bounds how long state left by a serve proxy that died without reconnecting lingers; it is independent of `eviction_ttl_minutes` (eviction must not delete state a reconnect may rehydrate).

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

Generated and episodic memories are always redaction-scrubbed and clearly
lower-confidence than user-authored ones. Hint injection reads only frontmatter
(never bodies) on the hot path via a per-connection snapshot of the resolved
`[memory]` config (no per-call config read); when user-authored and generated
memories compete for the capped hint slots, user-authored ones always win.
Hybrid memory v1 (0.9.16): `write_memory` accepts `paths` globs (stored as
frontmatter, driving `relevant_memories` and hints), and an idle session that
wrote workspace files also leaves a durable `episodic-*` markdown memory —
redacted, provenance-stamped, indexed, and pruned to the newest
`generated_memory_keep`.

Three behaviours worth knowing: **per-project `generated_summaries` is one-way**
— a project may *disable* episodic summaries under a global opt-in, but may no
longer *enable* them under a global opt-out. Summaries are written to disk from
session content, so a repository turning them on for you produces an artefact
you did not ask for; turning them off costs nothing. (This tightened the 0.9.16
behaviour, which honoured the field in both directions — see
`docs/project-config-trust.md`.) Only the idle *threshold* is global-resolved
(`idle_summary_minutes` → `[session] idle_threshold_minutes`), and a session is
always summarised before it is evicted, even when `eviction_ttl_minutes` is
shorter than the threshold.
**`search_memories` auto mode greps when FTS finds nothing** — a fresh index
that returns zero FTS5 hits (the tokeniser is whole-token, so a substring like
`essio` inside `UserSession` won't match) falls through to substring grep;
`case_sensitive: true` always uses grep (FTS5 is case-insensitive); a literal
`mode: fts` keeps the empty FTS result. **Hint and episodic budgets are byte
caps** (`*_bytes`), enforced in bytes on a UTF-8 boundary, so a multi-byte
summary cannot overrun.

## `[collab]` — cross-agent sharing

Multiple agents (Claude Code, Codex, Gemini CLI, …) share one plumb daemon per
machine, and the daemon is the only process that observes every agent's activity
on a workspace. This layer surfaces that **advisorily** — nothing here ever
blocks a write. No env override; hot-reloaded; strictly per-workspace.

**Trust split.** The four channel switches — `intents`, `mailbox`,
`cross_project`, `knowledge_handoff` — are **gated on `plumb trust`**. A project
may ask for them, but the request is only honoured once you have approved that
exact config for that workspace; until then the global value stands, in both
directions.

The reason is that `.plumb/config.toml` is an untrusted surface — a cloned
repository ships it — and each of these opens a cross-agent channel. A channel a
repository can open for itself is one it can use. `cross_project` is the plainest
case: its contract is that receiving is the *recipient's* decision, which means
nothing while the recipient's own repo can flip it.

So "per workspace" is supported; "the repository decides" is not. The grant is
recorded in plumb's data dir (never in the project, so a clone cannot mark itself
trusted) and is bound to the config's exact content — editing it afterwards
lapses the grant. `plumb trust` prints each requested key as `key = value` with a
warning on the ones that open a channel.

Everything else in `[collab]` — `peer_awareness` and every budget and TTL — stays
freely project-overridable in both directions, because tuning cannot open
anything.

One consequence worth knowing: a trust grant binds to the workspace's *whole*
capability request, so changing any gated key — including flipping one of these
toggles in the TUI — lapses the grant for **all** of them, and the workspace
falls back to your global values until you run `plumb trust` again. That is what
makes the grant TOCTOU-proof, but it means a casual toggle can silently revert an
unrelated `[git]` or `[lsp.<lang>]` grant in the same file. A `[collab]` key plumb does not recognise is gated by default, so a
field added later fails closed rather than being silently free.

Three tiers, each behind its own flag. **Tier 1 (`peer_awareness`, default on)** is
passive and derived from writes the daemon itself performed or watched — verifiable
**observations**, never agent claims. **Tier 2 (`intents`, default off; `mailbox`,
default on)** adds agent-authored **claims**: `share_intent`, and the `leave_note`
/ `check_messages` mailbox (see
[Cross-agent sharing](tools.md#cross-agent-sharing-collab) in the tool reference).
Claims are always rendered distinctly from observations, secret-scrubbed
(`internal/redact`) before storage, byte-budgeted when injected, and stored in
`<workspace>/.plumb/collab.db` (WAL, auto-gitignored like `topology.db`), created
lazily on first **send** and pruned on the daemon session-reaper tick (reads filter
expired rows regardless). No read path ever creates the file, so a workspace whose
agents never send anything never gets a `collab.db`. **Tier 3
(`knowledge_handoff`, default off)** adds
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
| `hint_budget_bytes` | int | `512` | Byte cap (UTF-8 boundary) on any injected peer-signal block — the peer-activity hint, the `session_start` peer digest, the intent-aware write hint, and the git tool's repo-intent warning share it. Delivered message bodies use `chat_budget_bytes` instead. |
| `intents` | bool | `false` | **Gated on `plumb trust`.** Tier 2, opt-in: the `share_intent` tool, its listing in `workspace_sessions`, and the intent-aware peer write hint. |
| `mailbox` | bool | `true` | **Gated on `plumb trust`.** Agent-to-agent messaging between sessions **on this workspace**: `leave_note`, `check_messages`, message delivery on tool results and at `session_start`, and unread listing in `workspace_sessions`. On by default — same-project agents are working on shared code and the bodies never leave the project. |
| `cross_project` | bool | `false` | **Gated on `plumb trust`.** Opt-in: also **receive** messages from sessions pinned to a *different* workspace. Deliberately the recipient's decision, not the sender's — a session reads the daemon-level cross-project store only when its own project sets this, so another project can never inject text into this one's context uninvited. Sending across is always permitted; an un-opted-in recipient simply never reads it and the message expires unread. |
| `max_exchanges` | int | `10` | Messages allowed in one **conversation** before further replies are refused. A speed bump against two agents answering each other indefinitely, not an enforced ceiling: plumb cannot observe a human turn, so it counts total messages in a thread rather than consecutive agent replies, and opening a new thread starts a fresh budget. `0` uses the default. |
| `chat_budget_bytes` | int | `2048` | Byte cap (UTF-8 boundary) on a single delivered message body. Separate from `hint_budget_bytes` because a message is content the agent must act on, not a pointer to look up. `0` uses the default. |
| `max_wait_seconds` | int | `55` | Ceiling on how long `check_messages` will block waiting for a message. Kept below the client's own MCP call timeout so a wait expires cleanly rather than surfacing as a tool timeout. `0` uses the default. |
| `knowledge_handoff` | bool | `false` | **Gated on `plumb trust`.** Tier 3, opt-in: the `share_findings` tool — hand findings to peers now as a generated memory, instead of waiting for the idle episodic summary. |
| `intent_ttl_minutes` | int | `120` | Expiry applied to a new intent or note. Rows past expiry are pruned on the reaper tick and filtered from every read. `0` uses the default. |

A session holds at most **one live intent** — a new `share_intent` replaces it,
and it is cleared when the session ends. A `next` note is consumed on first
delivery; an addressed note persists until its TTL. Delivery is polling plus
hint injection only — plumb cannot push to a peer. `share_findings` writes its
memory as `finding-<timestamp>-<session>`, retention-shared with the idle
`episodic-*` summaries under `[memory] generated_memory_keep`, and it never
displaces a user-authored memory in a capped hint slot. Rule-based only — the
agent supplies the text; no LLM summary.

## `[rastro]` — Rastro associative-memory integration

```toml
[rastro]
enabled = false     # off by default; nothing is looked up or executed while disabled
path    = "rastro"  # executable name resolved on PATH, or an absolute path
```

Project-overridable; no env override; both fields are `ReloadNextSession` in the
field registry, so the TUI Settings screen marks them `²`. Surfaced in the TUI
under a **Rastro** group (Enabled toggle, Path text) and written scope-aware
like every other row — a workspace row lands in `<workspace>/.plumb/config.toml`,
a global row in the global config.

`plumb doctor`'s **Integrations** section reports the integration's state:
`disabled in config` when off; the resolved executable path (via
`exec.LookPath`) when on and found; a **failure** naming the binary and how to
fix it when on and absent. An unloadable config is reported there as a
*warning*, not a second failure — the Configuration section already fails the
run for that fault. `plumb` never executes the binary; it only resolves it.

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

One OpenAI-compatible client (`internal/semantics`) covers `openai`, `voyage`
(`voyage-code-3`), `jina`, `mistral`, and any self-run OpenAI-compatible server
(Ollama / llama.cpp / LM Studio / TEI / vLLM) via `provider = "custom"` +
`base_url`; `cohere` uses a small adapter. A local-model spike found a bundled
model does not beat FTS5, which is why plumb never bundles, downloads, or
supervises one. Embeddings are cached lazily in `topology.db`
(`topology_embeddings`, keyed by content hash).

## `[xcode]` — trusted Build Server Protocol setup

Bare `.xcodeproj` and `.xcworkspace` roots need `buildServer.json` plus Xcode
build data for complete SourceKit-LSP semantics. Automatic setup is deliberately
off by default and always requires per-workspace trust, even when enabled in the
global config:

```toml
[xcode]
auto_build_server = true
scheme = "MyApp"
timeout = "2m"
```

Review the workspace first, then run `plumb trust` from its root. On the next
session Plumb safely selects one project/workspace marker, validates the configured
scheme (or requires exactly one discovered scheme), and runs only bounded argv
commands for `xcodebuild -list -json` and `xcode-build-server config`. It never
uses a shell and never builds the project. `xcode-build-server` itself invokes
Xcode tooling and its current implementation may interpolate Xcode-derived values,
which is why trust is per workspace rather than implied by a global opt-in. After
generation Plumb restarts only that root's SourceKit-LSP entry through the shared
pool.

Configuration, build-data availability, restart/warm-up, and a proven non-empty
semantic result are separate states in logs, `plumb doctor`, `session_start`,
and empty-result guidance. If build data is absent, build the selected scheme once
in Xcode; Plumb will not do that automatically.

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `auto_build_server` | bool | `false` | `PLUMB_XCODE_AUTO_BUILD_SERVER` | Opt in to trusted attach-time generation. Per-workspace trust remains mandatory for global and project opt-ins. |
| `scheme` | string | `""` | — | Explicit scheme, validated against `xcodebuild -list -json`. Empty selects only when exactly one scheme exists. |
| `timeout` | duration | `"2m"` | — | Per-command bound for scheme discovery and build-server generation. Must be positive while automatic setup is enabled. |

## `[lsp_query]` — LSP operation timeout (global only)

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `timeout` | duration | `"30s"` | `PLUMB_LSP_QUERY_TIMEOUT` | Caps a single LSP tool operation when the caller's context carries no deadline, so a wedged language server can't hang a request. `0` disables. |

The timeout is applied at the tool layer (`withLSPDeadline`) and is a no-op when
the context already carries a deadline, so the cold-start handshake is never
shortened.

**LSP → topology fallback:** on LSP error/timeout, `workspace_symbols` and
`file_outline` fall back to the topology index (when enabled), annotated
`source=topology, mode=indexed-approximate`; a no-op when topology is disabled
or has no match. `get_definition` **by name** (`symbol_name`) also falls back to
the index when the server is unavailable — approximate (the declaration line
resolved by name, annotated `source=topology, mode=indexed-approximate`), since
the index has no position-level go-to-definition. The raw-position form of
`get_definition` and the other position/semantic tools (`find_references`, the
call/type hierarchies, `rename_symbol`) have no equivalent and surface the error
unchanged — they need a precise position or a whole-workspace reference graph
the index does not hold. **Empty-result fill:** `workspace_symbols` additionally
supplements an *empty-but-no-error* LSP answer from the index for **tree-sitter**
languages (annotated `topology fill … source=topology, mode=indexed-approximate`)
— lazy servers like zls only answer for files they have already analysed, so a
freshly-attached session would otherwise report "No symbols found" for a symbol
the Map knows. Native-AST languages (Go via gopls, which indexes eagerly) are
excluded so an authoritative empty answer is never supplanted.

## `[tools]` — tool advertisement profile

Governs which tools are *advertised* in `tools/list` — a hidden tool stays
callable by name via `tools/call` (hidden ≠ unregistered); this only trims the
advertised set so a client with its own native filesystem tools isn't billed for
the non-lean remainder (37 tools today). Project-overridable.

A *schema-discovery-only* client cannot use this knob at all — it can only
invoke what `tools/list` advertised, so hiding a tool removes the capability
rather than the schema. Kimi Code is one, and takes the same saving on the
**client side** instead, as do Codex and Gemini CLI (which have their own
allowlists but no verified deferred discovery, so `auto` still serves them
`full`): `plumb setup <client> --lean` writes plumb's lean tool names into the
client's own config — `enabledTools` in Kimi Code's `mcp.json`, `enabled_tools`
on `[mcp_servers.plumb]` in Codex's `config.toml`, `includeTools` on
`mcpServers.plumb` in Gemini CLI's `settings.json`. The list is a snapshot of
`tools.LeanToolNames()` (its only permitted source — a client-enforced filter
cannot be rescued by plumb's server-side bootstrap guarantee), and `plumb doctor`
grades it. See [CLI reference → `plumb setup`](cli-reference.md#plumb-setup).

plumb **cannot see** whether such an allowlist is in force: the client applies it
before a call is ever made, and the daemon is shared and long-lived, so its
environment is not reliably the connecting client's. `session_start` therefore
writes guidance for these three clients that is correct either way — it names
only lean-set tools, and its no-language-server fallbacks point at the client's
own file search rather than at `search_in_files`/`find_files`, which an allowlist
would have removed. The profile line still reports `full` for them, because that
is what plumb *advertised*; the filtering happens after.

**Auto resolution is capability-gated, not a config setting.** `auto` resolves
to **lean** only when the connecting client's entry in `internal/clientcaps`
declares `ReliableDeferredToolDiscovery = true` — reviewed, evidence-based proof
that its model reliably discovers and invokes a tool absent from its initial
`tools/list` (a ToolSearch-style deferred mechanism). That flag is compiled-in
registry data, not one of the config keys below and not something a project or
env var can flip; no shipped client (including Codex and Gemini CLI, despite
their strong native file/search/shell access) carries it yet, so `auto`
resolves to **full** for every client today. `session_start` and `daemon_info`
report which rule decided, via a stable reason string: `client-override`,
`explicit-config`, `unknown-deferred-discovery`, `schema-discovery-only-client`,
`verified-deferred-discovery`, or `unverified-deferred-discovery`. A fixed
four-tool bootstrap set (`session_start`, `git`, `read_file`, `edit_file`) is
always advertised regardless of the resolved profile.

| Field | Type | Default | Env | Effect |
|---|---|---|---|---|
| `profile` | string | `"auto"` | `PLUMB_TOOLS_PROFILE` | `auto` (capability-gated — lean only for a client with a verified deferred-discovery capability, full otherwise; see above) \| `lean` (non-bootstrap commodity tools hidden) \| `full` (every tool advertised). |
| `client_profiles` | map | `{}` | — | Per-client override, keyed by a case-insensitive `clientInfo.name` prefix (e.g. `"claude-code"`); each value is `auto`\|`lean`\|`full`. An empty or absent entry falls through to `profile`. |

**The lean set and the mutation-lane rule.** The lean set
(`internal/tools/profile.go` `LeanTools`, the single source of truth) keeps
`session_start`, the read/edit/write/transaction file tools, `git`,
`diagnostics`, the core LSP-semantic tools, the headline topology tools,
`search_memories`, and `run_task`. The **mutation-lane rule** governs it: a
read-only commodity tool may be hidden freely, but a mutation tool whose native
fallback is unsafe (`mv`/`rm`/`sed` bypass plumb's per-path locks, the LSP
notify, and the transaction WAL) stays lean; `read_file`/`read_symbol` stay lean
too because the edit lane needs their mtime/sha headers. `run_task` is lean for
the same reason: its only "native equivalent" is a raw shell `go test`/
`zig build`, so hiding it just routes a recognised CLI client to the shell-build
anti-pattern the profile exists to avoid. Under the lean profile `session_start`
prints a one-line note with the hidden count and how to restore `full`.

**Mid-session profile changes.** The server advertises the `tools.listChanged`
capability and emits a `notifications/tools/list_changed` whenever a config
reload changes the connection's resolved profile (e.g. a per-project `[tools]`
override loaded at attach, or a hot-reloaded global setting). The resilient
proxy forwards that server-initiated frame to the client unchanged, so a client
that honours the notification re-lists and picks up the new profile mid-session;
a client that lists only once still won't (its choice).

**Always-loaded (pinned) tools — Claude Code MCP tool search.** Claude Code
defers MCP tool *schemas* by default (only tool names load at session start; the
model must call `ToolSearch` to page a schema in before invoking it — otherwise
it guesses parameter names and the call is rejected client-side, before it ever
reaches plumb). plumb exempts its highest-frequency tools from that deferral by
advertising them with `_meta["anthropic/alwaysLoad"] = true` in `tools/list`
(`MetaAlwaysLoadKey`, `internal/mcp/server.go`; emitted in `handleToolsList`
when `Server.AlwaysLoad` accepts the name, wired in `conn_register.go` to
`tools.IsLean || tools.IsBootstrap || tools.IsMailbox`). The pinned set is
`LeanTools` plus two sets pinned for their own reasons:

- **`BootstrapTools`** — so a future profile change can never un-pin
  `session_start`/`git`/`read_file`/`edit_file`. It is a subset of `LeanTools`
  today, so it adds nothing yet; it is named to keep that guarantee independent
  of lean membership.
- **`MailboxTools`** (`leave_note`, `check_messages`) — disjoint from
  `LeanTools`, so this genuinely widens the pinned set by two. These are the
  only tools whose own output tells the agent to call the other one, and
  deferring one half of a half-finished exchange leaves the agent holding an
  instruction it cannot follow. They are pinned as a **pair**; a one-sided edit
  recreates the defect and fails `TestMailboxToolsArePairedAndNonLean`.

Pinning is not visibility: it changes only whether a schema is deferred, so it
is independent of both the `[tools]` profile and `[collab] mailbox`. Clients
that predate the convention ignore the unknown `_meta` and are unaffected; no
config knob is exposed (a per-machine override is `alwaysLoad: true` on the
plumb server entry in the client's own MCP config).

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

> **Per-workspace trust required for everything except four fields.** In a
> project's `.plumb/config.toml`, only `enabled`, `diagnostics`, `idle_timeout`
> and `max_workspaces` are honoured freely — none of them can change which process
> runs. **Every other key** in an `[lsp.<lang>]` table, recognised or not, is
> gated on `plumb trust`: `command`, `args`, `env`, `initialization_options`,
> `root_markers` and `weak_root_markers` decide which process plumb spawns and
> with what, and an unrecognised key is gated because plumb cannot prove it does
> not. (TOML keys reach a field case-insensitively, so `Command` is `command`;
> gating by exclusion is what stops a variant spelling slipping past.) A language
> your global config does not define is dropped from a project config either way —
> plumb has no adapter for it. See
> [Project-config trust](#project-config-trust).

### Project-config trust

A project config is an untrusted surface: cloning a repository ships one, and it
takes effect on attach with no prompt. Everything in it that grants a
**capability** rather than expressing a preference therefore needs your explicit
approval — the `[git]` tiers, and the `[lsp.<lang>]` fields above.

`plumb trust` (run from the workspace, or `plumb trust <dir>`) is that approval.
It prints every capability-granting key as `key = value`, then a block naming the
ones that are execution or a tier grant, then **asks**. Answer `y` to grant.
`--yes` skips the prompt; without a terminal and without `--yes` it refuses
rather than granting, so nothing acquires the grant as a side effect of running
in a script.

One `plumb trust` records **one grant per workspace**, covering the project's
task commands, its `[[command]]` allow-list, its `[commands]` shell policy, its
LSP config, and its git policy. The record lives in `DataDir/trust.json`, never
in the project, so a repository cannot mark itself trusted.

The grant is **bound to content**. Each part is hashed independently, so editing a
task command does not disturb the LSP grant — but rewriting a trusted `command`
does mean the new command is not honoured until you re-run `plumb trust`. An
unreadable or corrupt trust store fails closed.

Nothing about this is silent. An untrusted request is reported by `plumb doctor`
(a warning naming the keys and the fix), by `plumb config show` (the row's
provenance reads `global config — project asked, UNTRUSTED`, and the requested
values are printed in full below the table), by a daemon log line at attach, and
in the TUI settings editor by a `⁶` marker on the row — which shows the value
actually in force, not the one in the project file.

All of those address the **user**. An untrusted `[git]` request is additionally
reported to the **agent**, in `session_start`'s git-policy section, beneath the
resolved policy it annotates: it names the ignored keys, explains that plumb
takes the whole `[git]` table from the global config until the request is
approved, and gives both fixes (`plumb trust` here — `--yes` to skip the prompt —
or the global config / `PLUMB_GIT_*` everywhere). Without it an agent sees only
`Push/fetch/pull: off.`, cannot reconcile that with the `allow_push = true` in
the repository it is looking at, and concludes the tier is unimplemented — which
is an argument for shelling out to raw git, bypassing the policy the drop exists
to enforce.

Three details keep every line of that notice checkable against the policy printed
above it:

- **It names only keys that genuinely differ.** The requested value is compared
  against the resolved policy field by field, so a workspace that already grants
  the tiers globally and then clones a repository asking for them too gets
  nothing — claiming those keys were "NOT in force" would be false, with a
  `plumb trust` that would change nothing attached to it. A `[git]` field the
  notice does not recognise cannot be compared, so it is named rather than
  assumed satisfied.
- **The reason runs in both directions**, because `[git]` is forced back
  *wholesale*, not tier by tier: unapproved, a project can neither open the
  destructive or network tier and shorten the protected-branch list, nor turn
  `allow_writes` or `commit_trailer` off. All four `PLUMB_GIT_*` variables are
  named for the same reason.
- **The remediation is stated per route, because the three land at different
  moments.** `plumb trust` writes `DataDir/trust.json`, which nothing watches, so
  the grant lands when the workspace is next attached — a new session, `plumb
  restart`, or a re-pin — and until then both the policy and the notice keep
  saying exactly what they said before (they are one snapshot, captured together
  when the project config was applied). A **global config** edit, by contrast,
  applies *mid-session*: the daemon watches that file and re-applies it to every
  live session, and `plumb config reload` forces the same pass. A `PLUMB_GIT_*`
  variable needs *more* than a new session, not less — it is read from the daemon
  **process's** environment, so exporting it in your shell and re-running
  `session_start`, or re-pinning, changes nothing until the daemon itself is
  restarted with the variable set.

The notice is silent when the project sets no `[git]` key and when every
requested value already matches — which is the ordinary trusted session, since
the grant is what put those values in force.

Being trusted is not itself a reason for silence, and treating it as one
reintroduces the bug from the other side. Plumb applies `PLUMB_GIT_*` *after* the
project config (so that forcing an untrusted `[git]` back to base cannot discard
an override you set for the process), which means the environment outranks a
trust grant too: an approved `allow_push = true` plus `PLUMB_GIT_ALLOW_PUSH=0`
resolves to push **off**. That is the same unexplained `Push/fetch/pull: off.`
the notice exists to end, so it gets an `OVERRIDDEN` notice naming the
environment as the layer that beat the grant — `plumb trust` is already given and
recommending it would send the reader to re-approve something that is not the
obstacle. A trusted `[git]` field with no counterpart in the resolved policy
(`env`) is *not* reported there: the table was applied whole, so it is in force,
and naming it would invent an override that does not exist.

A `.plumb/config.toml` that **cannot be parsed** gets a third notice: it is
skipped whole, so its `[git]` block is as ignored as an untrusted one — and there
is no `plumb trust` that would help. That notice reports only what the *file*
contributed (nothing); it deliberately does **not** claim the policy above was
resolved without the file, because `applyProjectConfig` returns on a parse error
without reverting the session's git view. A config that parsed at attach and was
then broken in place leaves its already-applied values in force underneath the
notice — precisely the state you are in while editing the file. `plumb restart`
resolves the policy from scratch.

| Field | Type | Effect |
|---|---|---|
| `command` | string | Executable to launch (must be on `PATH`). Required when `enabled`. |
| `args` | []string | Arguments passed to the server. |
| `root_markers` | []string | Files whose presence identifies a workspace of this language. |
| `env` | map | Extra environment variables for the server process. |
| `enabled` | bool | Whether plumb starts this server and detects this language. |
| `idle_timeout` | duration | Hibernate the server (stop its process, keep the warm cache) after this long without a tool call; the next call restarts it. `0` disables. Default `0`, except `java` = `20m`. Restart-needed. |
| `max_workspaces` | int | Cap on concurrently-running servers of this language; the least-recently-used is hibernated before starting another. `0` = unlimited. Default `0`, except `java` = `2`. Restart-needed. |
| `diagnostics` | string | How plumb negotiates this language's diagnostics: `auto` (default; an absent/empty value is treated the same) defers to plumb's per-adapter policy — **push for every adapter today**; `push` consumes pushed `publishDiagnostics` only; `pull` advertises the LSP 3.17 `textDocument/diagnostic` client capability and negotiates the pull model when the server advertises `diagnosticProvider` (otherwise the connection degrades to `pull-requested-but-unavailable`). See *Diagnostics mode* below. Restart-needed. |
| `initialization_options` | table | Free-form table sent verbatim as the LSP `initializationOptions` at `initialize`. Absent/empty sends nothing (the default). Advanced escape hatch — keys are server-specific and unvalidated by plumb. See *Server initialization options* below. Restart-needed. |

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
| `kotlin` | `kotlin-lsp --stdio` (plumb appends `--system-path <dir>`) | `settings.gradle.kts`, `build.gradle.kts` |
| `html` | `vscode-html-language-server --stdio` | weak: `index.html` |

Go and Python are first-class; Java, Rust, Swift, Zig, and TypeScript/JavaScript
are validated; HTML is experimental (see the status table under
*Validation levels* in [Adding an LSP Adapter](adding-an-lsp.md#validation-levels)).

jdtls is heavyweight (~0.8–1.5 GB RSS); it defaults to `idle_timeout = "20m"` and
`max_workspaces = 2` so idle JVMs are hibernated and concurrent JVMs are capped.
If your `jdtls` launcher is not named `jdtls` on `PATH` (e.g. `jdtls.sh`,
`jdtls.bat`, or an absolute path), set `command` accordingly. Use
`plumb debug lsp` to see each server's state, PID, RSS, and idle time.

### Diagnostics mode: push vs pull (LSP 3.17)

By default plumb consumes diagnostics the way it always has: the language
server pushes `publishDiagnostics` notifications as it re-analyses files, and
plumb's cache holds the latest snapshot per file. Setting
`[lsp.<lang>] diagnostics = "pull"` additionally advertises the LSP 3.17
`textDocument/diagnostic` client capability, so a server that implements the
pull model answers on-demand diagnostic requests — result IDs, unchanged
reports, and related documents included — instead of (or, commonly, alongside)
pushing. Pull is additive: the push capability stays advertised too, so a
dual-mode server keeps pushing if it wants to.

`gopls` needs one extra step to answer pulls at all: pull mode also sets its
experimental `pullDiagnostics: true` initialization option.

plumb resolves each connection to exactly one of four states, and **never
infers the mode from cache contents** — it is always the outcome of what was
requested and what the server advertised:

| Resolved mode | Meaning |
|---|---|
| `push` | Requested `push` (or `auto`, which resolves to `push` for every adapter today), or a `pull`/`hybrid` connection that downgraded after a method-not-found response (see below). |
| `pull` | Requested `pull`, and the server advertised `diagnosticProvider`. |
| `hybrid` | Requested `pull`, negotiated pull, and the server also kept sending `publishDiagnostics` — e.g. gopls v0.23 forced into pull answers `textDocument/diagnostic` correctly and keeps pushing, so it resolves to hybrid. |
| `pull-requested-but-unavailable` | Requested `pull`, but the server never advertised `diagnosticProvider` (e.g. typescript-language-server, zls) — plumb logs one warning and the connection behaves as push. |

See [Troubleshooting → diagnostics mode](troubleshooting.md#how-do-i-read-the-resolved-diagnostics-mode)
for where the resolved mode is surfaced (`plumb doctor`, `lsp-status`,
`daemon_info`, `session_start`) and what the `-32601` downgrade looks like.

**Evidence-gated `auto` policy.** `auto` resolves to `push` for every adapter
today. Real-binary testing (gopls v0.23.0, macOS arm64) found that forcing
pull negotiates cleanly — gopls answers pulls correctly and keeps pushing
(hybrid) — with a large single-document latency win (median ~3ms per pull vs a
~1s push-arrival, gopls's own diagnostics debounce). It does not, however,
implement `workspace/diagnostic`, so a whole-workspace query under pull
covers only files already analysed or explicitly pulled, whereas push delivers
workspace-wide diagnostics as gopls analyses the module — so `auto` stays
`push`. `pull` remains available as an explicit per-language opt-in for the
low-latency single-file behaviour, with push continuing underneath on a
hybrid-capable server. typescript-language-server and zls do not currently
advertise `diagnosticProvider` under pull, so requesting `pull` for either
degrades to `pull-requested-but-unavailable` and behaves as push.

### Server initialization options (advanced)

`[lsp.<lang>].initialization_options` is a free-form table passed **verbatim** to
the language server as the LSP `initializationOptions` at `initialize`. plumb does
not validate or interpret the keys — they are whatever the specific server accepts.
When absent or empty, plumb sends no options, so the init handshake is byte-for-byte
unchanged from the default. It is a restart-bound, per-language escape hatch for
server-specific tuning plumb does not model with a dedicated field.

Interaction with `diagnostics = "pull"` (gopls): a configured options table
replaces gopls's typed defaults, but pull mode still injects the experimental
`pullDiagnostics: true` flag into the sent table unless you set that key
yourself — an explicit user value (either way) always wins.

The headline case is **Zig / zls**. By default zls reports only `ast-check` *syntax*
diagnostics; real compile and semantic errors require build-on-save, which is off
unless you turn it on:

```toml
[lsp.zig.initialization_options]
enable_build_on_save = true
build_on_save_step   = "check"   # a step defined in your build.zig
```

> **⚠ Cost & security — opt-in only.** `enable_build_on_save` makes zls run
> `zig build` on **every save**, which has a real CPU cost and **executes your
> project's own `build.zig` logic**. Because `[lsp.<lang>]` (like `command`/`args`)
> can be set in a workspace's `.plumb/config.toml`, enabling this for a repository
> you do not trust means opening it can run that repository's build script. plumb
> never turns build-on-save on for you; leave it unset for untrusted code.

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

`workspace_symbols` consults the primary for a single-language root but **fans
out** across every server for a multi-language monorepo root (the child-marker
discovery case — see [Architecture → Workspace
detection](architecture.md#workspace-detection)), merging and deduplicating
results; the call/type hierarchies are URI-bearing and route per-file.
`diagnostics` aggregates across every server bound to the root.

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
(`&&`, `;`, `|`, `$(`, backtick, redirects) are rejected (`config.ParseTaskCommand`).
The only agent-supplied input that reaches the argv is a shell-safe `{target}`
(`^[A-Za-z0-9._/:@-]+$`). Shipped defaults exist
for common languages (Go fully populated; a slot is left empty rather than guess
an uninstalled tool). Output and runtime are bounded (100 KiB/200 lines, timeout).

**Trust gate.** A task command supplied by a *project* `.plumb/config.toml` is
not run until the workspace is trusted with `plumb trust` (recorded per workspace
root in `DataDir/trust.json`, never in the project — a cloned repo cannot
self-trust). Trust is **bound to a hash of the trusted command set**: if any task
command is later added, removed, or changed, the grant no longer matches and the
command is refused until you re-run `plumb trust` — so an agent that rewrites a
trusted command cannot have the new command run without a re-prompt. A
`trust.json` written by an older plumb (the legacy boolean format) is treated as
untrusted and re-confirmed once on the next `plumb trust`. When it records trust,
`plumb trust` prints each command being trusted and flags any that invoke an
interpreter with inline code (`bash -c`, `sh -c`, `python -c`, `node -e`,
`perl -e`, `ruby -e`) — arbitrary code execution by design, so review it before
trusting. Default- and global-config commands always run.

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
| `PLUMB_FSYNC` | `edits.fsync` |
| `PLUMB_REFUSE_HOME_ROOTS` | `walk.refuse_home_roots` |
| `PLUMB_GIT_ALLOW_WRITES` | `git.allow_writes` |
| `PLUMB_GIT_ALLOW_DESTRUCTIVE` | `git.allow_destructive` |
| `PLUMB_GIT_ALLOW_PUSH` | `git.allow_push` |
| `PLUMB_GIT_COMMIT_TRAILER` | `git.commit_trailer` |
| `PLUMB_GIT_WRITE_TIMEOUT` | `git.write_timeout` |
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
strict                    = false   # require read_file before edit_file / symbol edits
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
commit_trailer     = false                  # stamp commits with a Plumb-Session: <name> trailer
env                = {}                     # extra env for the git child (hooks see it); trust-gated
write_timeout      = "10m"                  # bound on a mutating git child before plumb kills it; trust-gated

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
extract_timeout_seconds = 10                # abandon one file's parse after this long (0 = the 2 min ceiling)
resync_batch            = 100               # files per pause during a full resync (0 disables)
resync_pause_ms         = 25                # pause after each batch, ms (0 disables)
resync_interval_minutes = 60                # periodic full resync FALLBACK (suppressed while watch is on); 0 disables
watch                   = true              # OS-level file watching: re-index on change, whoever made it

[session]
idle_threshold_minutes    = 30              # TUI idle marker threshold (cosmetic)
eviction_ttl_minutes      = 60              # daemon force-closes a connection idle this long; 0 disables
persist_state             = true            # persist read-tracking + pinned workspace + session name across a daemon restart (env PLUMB_PERSIST_SESSION_STATE)
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
