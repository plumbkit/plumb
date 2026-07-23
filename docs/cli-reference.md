# CLI Reference

Every `plumb` command, argument, and flag. Run `plumb <command> --help` for the
same information at the terminal.

Plumb has **no global flags**. Daemon logging is controlled through
[configuration](configuration.md) (`log_level`, `log_format`, `log_file`), the
`PLUMB_LOG_*` environment variables, and — at runtime — the
[`log-level`](#plumb-log-level) command.

Many commands resolve the *workspace* by walking up from the given path (or the
current directory) until they find a `.plumb/` marker, a language root marker
(`go.mod`, `pyproject.toml`, …), or a `.git/` directory — the same way the
daemon does. A git repo with no language marker resolves to its git root with
language `none`.

## Command index

| Command | Summary |
|---|---|
| [`plumb`](#plumb-dashboard) | Launch the interactive TUI dashboard |
| [`plumb serve`](#plumb-serve) | Start the MCP server over stdio (the command MCP clients run) |
| [`plumb daemon`](#plumb-daemon) | Run the shared background daemon (usually automatic) |
| [`plumb stop`](#plumb-stop) | Stop the background daemon |
| [`plumb restart`](#plumb-restart) | Restart the daemon (stop + fresh spawn) |
| [`plumb init`](#plumb-init) | Create a `.plumb/` workspace marker |
| [`plumb setup`](#plumb-setup) | Register plumb as an MCP server for a client |
| [`plumb doctor`](#plumb-doctor) | Run health checks |
| [`plumb config`](#plumb-config) | Inspect resolved configuration |
| [`plumb sessions`](#plumb-sessions) | List active sessions |
| [`plumb stats`](#plumb-stats) | Show tool-call statistics (alias: `status`) |
| [`plumb diagnostics`](#plumb-diagnostics) | Print LSP diagnostics (alias: `diag`, `diags`) |
| [`plumb log-level`](#plumb-log-level) | Change the running daemon's log level |
| [`plumb enable-lsp`](#plumb-enable-lsp) | Enable a language server in the running daemon without a restart |
| [`plumb debug`](#plumb-debug) | Daemon introspection: memory, heap/stack dumps, LSP state |
| [`plumb version`](#plumb-version) | Print version information |

---

## `plumb` (dashboard)

```
plumb
```

Run with no subcommand, `plumb` launches the interactive **TUI dashboard** — a
read-only live monitor of the daemon built with Bubble Tea v2. Sections
(opened with `/`): **Dashboard**, **Sessions**, **Memory**, **Logs**,
**Settings**. The Settings section includes a live theme picker; the active
theme is read from `[ui].theme` in the global config.

In the **Sessions** section, press `r` to rename the selected session and `a`
to refresh; both are also listed in the right panel's footer and the in-app help
overlay (`ctrl+h`). Press `q` or `ctrl+c` to quit. See the
[TUI conventions in AGENTS.md](../AGENTS.md) for navigation details.

---

## `plumb serve`

```
plumb serve
```

Start the MCP server over stdio. **This is the command MCP clients invoke.**
`serve` is a resilient, frame-aware proxy: it dials the daemon's Unix socket —
spawning `plumb daemon` if none is running — and proxies MCP frames between the
client and the socket. On a daemon crash or hang it respawns the daemon and
replays the captured `initialize` handshake, so the client never notices. It
registers no tools and owns no language-server processes itself.

If the running daemon's build version differs from this binary's, `serve`
prints a warning to stderr suggesting `plumb restart` to refresh.

| Flag | Default | Effect |
|---|---|---|
| `--no-reconnect` | `false` | Disable the reconnecting proxy; fall back to a plain byte copy (legacy behaviour). |
| `--allow-dir <path>` | — | Grant an extra **read-write** root to this connection (repeatable). Additive to the detected workspace and config `extra_roots`; never replaces them. Also read from `PLUMB_ALLOWED_DIRS` (OS-list-separated). Each path is `$VAR`-expanded and made absolute, then canonicalised (symlink-aware) by the daemon. Requires the resilient proxy (the default); ignored under `--no-reconnect`. |

The `--allow-dir` grant is transported to the daemon inside the captured
`initialize` frame's `params._meta` (`dev.plumbkit/allow-dirs`), so it rides the
handshake replay automatically — a reconnected daemon re-applies it with no
separate message. The grant is per-connection: it never leaks into another
client's session, and it survives a workspace re-pin.

`serve` also transports its own working directory the same way
(`dev.plumbkit/workspace`) as an **advisory workspace attach hint** for clients
that report no MCP roots (e.g. Claude Desktop): if nothing stronger resolves
the workspace — no explicit `session_start` pin, no client root, no persisted
pin from an earlier reconnect — the daemon attaches from the serve cwd,
validated against project markers. The hint never overrides an explicit choice
and is never persisted as the sticky pin.

---

## `plumb daemon`

```
plumb daemon
```

Run the shared background daemon. **Usually started automatically by
`serve`** — you rarely run this by hand. The daemon owns the language-server
subprocesses (one per `(root, language)` — a single workspace root may host
several, e.g. `gopls` + an HTML server), the per-connection MCP sessions, the
stats database, and the topology pool.

It takes an exclusive `flock` on `plumb.daemon.lock` for its lifetime; a second
`plumb daemon` invocation sees the lock held and exits immediately, enforcing
the single-daemon invariant.

No flags.

---

## `plumb stop`

```
plumb stop [--force]
```

Stop the background daemon. The daemon is located in three stages: PID file →
`lsof` on the socket → `pgrep -f "plumb daemon"`. The `pgrep` fallback covers
binary upgrades that changed the socket/PID path.

| Flag | Default | Effect |
|---|---|---|
| `--force` | `false` | Stop without asking for confirmation. |

Use `plumb stop` (or `plumb restart`) after rebuilding the binary so the next
`serve` starts a daemon running your new code.

---

## `plumb restart`

```
plumb restart [--force]
```

Stop the running daemon and bring a fresh one straight back up — the resilient
proxy reconnects active clients. Use it after rebuilding so new code activates
without manually stopping and waiting for the next `serve`.

| Flag | Default | Effect |
|---|---|---|
| `--force` | `false` | Skip the confirmation prompt. |

---

## `plumb init`

```
plumb init [directory] [--discover]
```

Create a `.plumb/` workspace marker in the current directory (or `directory` if
given) and seed `.plumb/context.md` from a template. If `.plumb/` already
exists, the command reports its location and does nothing else.

| Argument | Description |
|---|---|
| `directory` | Optional. Directory to initialise. Defaults to the current directory. (Max 1.) |

| Flag | Default | Effect |
|---|---|---|
| `--discover` | `false` | Auto-detect project structure (languages, build systems, entry points, test layout) and seed `context.md` from the discovery instead of the blank template. |

`.plumb/` also holds the `memories/` store and — when `[topology]` is enabled —
`topology.db`. Commit it to share project context with your team, or add it to
`.gitignore` to keep it local.

---

## `plumb setup`

```
plumb setup <client>
```

Register the current `plumb` binary as a stdio MCP server in a client's config.
Setup helpers preserve any existing MCP servers (and any extra keys on an existing
plumb entry, such as Codex's per-tool approval tables) and back up the config
before modifying it.

| Subcommand | Config target |
|---|---|
| `plumb setup claude-desktop` | Claude Desktop's platform-specific JSON config |
| `plumb setup claude-code` | `~/.claude.json` (user scope) |
| `plumb setup claude-code --project` | `.mcp.json` in the current directory (project scope) |
| `plumb setup codex` | `$CODEX_HOME/config.toml` (or `~/.codex/config.toml`) |
| `plumb setup gemini` | `~/.gemini/settings.json` |
| `plumb setup cursor` | `~/.cursor/mcp.json` |
| `plumb setup augment` | `~/.augment/settings.json` |
| `plumb setup qwen` | `~/.qwen/settings.json` |
| `plumb setup antigravity` | `~/.gemini/config/mcp_config.json` (shared `mcpServers` config Antigravity reads) |
| `plumb setup antigravity-desktop` | `~/.gemini/config/mcp_config.json` (same shared config) |
| `plumb setup opencode` | `~/.config/opencode/opencode.json` |
| `plumb setup crush` | `~/.config/crush/crush.json` |
| `plumb setup goose` | `~/.config/goose/config.yaml` |
| `plumb setup hermes` | `~/.hermes/config.yaml` |

| Flag | Applies to | Effect |
|---|---|---|
| `--project` | `claude-code` | Write to `.mcp.json` in the current directory (project-scoped) instead of the user-level config. |
| `--all` | `plumb setup` | Repoint **every** already-registered client at the current `plumb` binary, skipping clients that aren't installed or don't use plumb. The bulk repair after the binary moves or is rebuilt elsewhere — pairs with `plumb doctor`'s registered-binary check. Re-points only; never adds plumb to a client that didn't have it. When installed-but-unregistered clients are found, it prints a hint pointing at `--install-missing`. |
| `--install-missing` | `plumb setup` | Like `--all`, but also **registers** plumb in installed clients that don't have it yet — any client whose config file already exists but has no plumb entry. Clients with no config file at all are left untouched (plumb can't tell an absent config from an uninstalled client — use the client's named subcommand to create one). Triggers the bulk run on its own, so `plumb setup --install-missing` is the one-shot first-time setup for every client already present on the machine. |

---

## `plumb doctor`

```
plumb doctor [--workspace <dir>] [--json]
```

Run health checks grouped by topic and report what needs attention. Exits
non-zero if any check fails. Sections:

- **Daemon** — socket reachable; running version matches this binary.
- **Language Servers** — each configured LSP binary is on `PATH` (enabled
  servers that are missing fail; disabled ones are informational), plus a
  Java 21+ runtime check when `java` is configured.
- **MCP Clients** — for each supported client, whether plumb is registered
  **and** that the binary the config launches still exists and matches the
  running executable. A registered binary that no longer exists is a failure; a
  binary that exists but differs from the current one (e.g. after moving or
  rebuilding plumb elsewhere) is a non-fatal **warning** (`!`). Both carry a
  `plumb setup <client>` fix hint — or run `plumb setup --all` to repoint every
  client at once.
- **Configuration** — global and project `config.toml` parse cleanly.
- **Data** — the global stats database is readable.
- **Indexing** — when `[topology]` is enabled for the workspace, the topology index is present and healthy (passes when topology is disabled — the opt-in default). A *missing* or *corrupt* index fails; an index that exists but is still building (empty, all files skipped, or no symbols extracted yet) is reported as a non-fatal **warning** (`!`) so a freshly enabled workspace does not false-negative. Inspected strictly read-only (`mode=ro`) without starting an indexer or creating sidecar files; warnings and failures carry a fix hint.

| Flag | Default | Effect |
|---|---|---|
| `--workspace <dir>` | current dir | Include project-scoped checks (project config, stats rows) for this workspace. |
| `--json` | `false` | Emit results as a JSON array instead of the ANSI table. |

`plumb doctor` is the first thing to run when something isn't working — see
[Troubleshooting](troubleshooting.md).

---

## `plumb config`

```
plumb config <subcommand>
```

Inspect plumb's resolved configuration. See the
[Configuration reference](configuration.md) for what each field means.

| Subcommand | Description |
|---|---|
| `plumb config print` | Print the resolved configuration as TOML. |
| `plumb config reload` | Tell the running daemon to re-read global config now (same as the fsnotify watch). |
| `plumb config show [--workspace <dir>]` | Show the resolved configuration with **source provenance** — which layer (default, global, project, env) set each value. Includes a **Directories** section listing plumb's config, data, state, log, and runtime directories, and an **Agent-written keys** footer (`provenance=agent`). |
| `plumb config unset <key> [--workspace <dir>]` | Remove a project-config key (the one-step revert for an agent-written value): drops it from `.plumb/config.toml` and the provenance sidecar, then reloads. |

| Flag | Applies to | Default | Effect |
|---|---|---|---|
| `--workspace <dir>` | `show` | current dir | Resolve project-layer config from this workspace. |
| `--adapters` | `show` | off | Print only the language-server adapter table (language, server, validation tier, activation state). Aliases: `--adapter`, `--lsp`, `--lsps`, `--integration`, `--integrations`. |

---

## `plumb sessions`

```
plumb sessions [--all]
```

List active plumb sessions — one per live MCP connection — with the generated
session name, ID, resolved workspace, and client identity.

| Flag | Default | Effect |
|---|---|---|
| `--all` | `false` | Include sessions whose workspace has not resolved yet (Folder empty). |

---

## `plumb stats`

```
plumb stats [--workspace <dir>] [--limit <n>]
```

Aliases: **`plumb status`**.

Show tool-call statistics for a workspace: a per-tool summary (calls, average
and P95 latency, input/output bytes, errors, token-efficiency estimate) and a list
of the most recent calls.

| Flag | Default | Effect |
|---|---|---|
| `--workspace <dir>` | current dir | Workspace to inspect. |
| `--limit <n>` | `20` | Number of recent calls to show. |

> Statistics are global to the daemon (`stats.db`) but filtered to the requested
> workspace. `plumb status` is identical to `plumb stats` — it does **not**
> launch the TUI dashboard (run bare `plumb` for that).

---

## `plumb diagnostics`

```
plumb diagnostics [file]
```

Aliases: **`plumb diag`**, **`plumb diags`**.

Print LSP diagnostics for the workspace — a debugging aid. Pass an optional
`file` to scope output to a single file. Requires a running daemon with an
attached language server.

| Argument | Description |
|---|---|
| `file` | Optional. Restrict diagnostics to this file. |

---

## `plumb log-level`

```
plumb log-level <level>
```

Change the **running daemon's** log level at runtime, over its control socket.
The change lasts for the daemon's lifetime only — it does not persist.

| Level | Effect |
|---|---|
| `debug` | Verbose logging. |
| `info` | Standard logging (default). |
| `warn` | Warnings and errors only. |
| `error` | Errors only. |
| `reset` | Restore the level captured at daemon startup (including any `PLUMB_LOG_LEVEL` override active then). |

To make a level permanent, set `log_level` in `~/.config/plumb/config.toml`.
Fails clearly if the daemon is not running.

---

## `plumb enable-lsp`

```
plumb enable-lsp <language>
```

Enable a configured language (`[lsp.<language>]`) in the **running daemon**, over
its control socket, **without a restart**. Enabling a language normally requires
restarting the daemon; this flips it on live: the daemon adds it to its effective
language set, and its server attaches **lazily** on the next file of that
language a session opens (no process is spawned eagerly, and existing sessions
and their servers are untouched).

The change is daemon-lifetime only, like [`plumb log-level`](#plumb-log-level).
To make it permanent, set `enabled = true` under `[lsp.<language>]` in the config
file — though installing the server is usually enough, since an installed,
enabled language activates automatically at startup.

Errors are honest: an unknown language (no `[lsp.<language>]` block), or a server
binary that is not installed (the message names the binary to install). Enabling
a language that is already active is a reported no-op. Fails clearly if the
daemon is not running.

---

## `plumb debug`

```
plumb debug <subcommand>
```

Daemon introspection over the control socket. Requires a running daemon.

| Subcommand | Description |
|---|---|
| `plumb debug mem` | Print a `runtime.ReadMemStats` snapshot (heap, GC count, goroutines). |
| `plumb debug heap` | Force a GC and write a `runtime/pprof` heap profile to the cache dir. |
| `plumb debug stacks` | Write a full goroutine stack dump (the `SIGQUIT`-equivalent) for diagnosing a hang. |
| `plumb debug lsp` | List each language server's state, PID, RSS, and idle time. |

---

## `plumb version`

```
plumb version
```

Print the plumb build version and the Go runtime version. The build version is
stamped at compile time (see [Versioning in AGENTS.md](../AGENTS.md)).

---

## `plumb build` / `test` / `lint` / `e2e` / `verify`

```
plumb build [target]
plumb test  [target]
plumb lint  [target]
plumb e2e   [target]
plumb verify
```

Run the configured [`[tasks.<lang>]`](configuration.md) command for the current
workspace's primary language, streaming its output. `verify` runs the build slot
then the test slot. `[target]` fills a `{target}` placeholder (a single shell-safe
argument) in the stored command. A project-supplied command must be trusted first
(`plumb trust`); the shipped defaults and global-config commands always run.

---

## `plumb trust`

```
plumb trust [directory]
```

Trust this workspace's project-supplied task commands (those set in its
`.plumb/config.toml`), so `plumb build`/`test`/… and the `run_task` tool will run
them. Trust is recorded per workspace **root** in plumb's data directory (never
in the project itself), so a cloned repository can never mark itself trusted.

Trust is **bound to a hash of the trusted command set**: if a task command is
later added, removed, or rewritten (including via `agent_config`), the grant no
longer matches and the command is refused until you re-run `plumb trust` — so an
agent that changes a trusted command cannot have the new command run without a
fresh prompt. A `trust.json` written by an older plumb (the legacy boolean
format) is treated as untrusted and re-confirmed once.

When it records trust, `plumb trust` **prints each command it is about to
trust** and flags any that invoke an interpreter with inline code (`bash -c`,
`sh -c`, `python -c`, `node -e`, `perl -e`, `ruby -e`) as arbitrary code
execution by design — review those before trusting. Default- and global-config
commands always run and never need trusting.
