# CLI Reference

Every `plumb` command, argument, and flag. Run `plumb <command> --help` for the
same information at the terminal.

Plumb has **no global flags**. Daemon logging is controlled through
[configuration](configuration.md) (`log_level`, `log_format`, `log_file`), the
`PLUMB_LOG_*` environment variables, and — at runtime — the
[`log-level`](#plumb-log-level) command.

Many commands resolve the *workspace* by walking up from the given path (or the
current directory) until they find a `.plumb/` marker or a language root marker
(`go.mod`, `pyproject.toml`, …) — the same way the daemon does.

## Command index

| Command | Summary |
|---|---|
| [`plumb`](#plumb-dashboard) | Launch the interactive TUI dashboard |
| [`plumb serve`](#plumb-serve) | Start the MCP server over stdio (the command MCP clients run) |
| [`plumb daemon`](#plumb-daemon) | Run the shared background daemon (usually automatic) |
| [`plumb stop`](#plumb-stop) | Stop the background daemon |
| [`plumb init`](#plumb-init) | Create a `.plumb/` workspace marker |
| [`plumb setup`](#plumb-setup) | Register plumb as an MCP server for a client |
| [`plumb doctor`](#plumb-doctor) | Run health checks |
| [`plumb config`](#plumb-config) | Inspect resolved configuration |
| [`plumb sessions`](#plumb-sessions) | List active sessions |
| [`plumb stats`](#plumb-stats) | Show tool-call statistics (alias: `status`) |
| [`plumb diagnostics`](#plumb-diagnostics) | Print LSP diagnostics (alias: `diag`, `diags`) |
| [`plumb log-level`](#plumb-log-level) | Change the running daemon's log level |
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

Press `q` or `ctrl+c` to quit. See the [TUI conventions in AGENTS.md](../AGENTS.md)
for navigation details.

---

## `plumb serve`

```
plumb serve
```

Start the MCP server over stdio. **This is the command MCP clients invoke.**
`serve` is a thin proxy: it dials the daemon's Unix socket — spawning
`plumb daemon` if none is running — and then copies bytes between the client's
stdin/stdout and the socket until EOF. It registers no tools and owns no
language-server processes itself.

If the running daemon's build version differs from this binary's, `serve`
prints a warning to stderr suggesting `plumb stop` to refresh.

No flags.

---

## `plumb daemon`

```
plumb daemon
```

Run the shared background daemon. **Usually started automatically by
`serve`** — you rarely run this by hand. The daemon owns the language-server
subprocesses (one `gopls`/`pyright` per workspace root), the per-connection MCP
sessions, the stats database, and the topology pool.

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

Use `plumb stop` after rebuilding the binary so the next `serve` starts a daemon
running your new code.

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
Setup helpers preserve any existing MCP servers and back up the config before
modifying it.

| Subcommand | Config target |
|---|---|
| `plumb setup claude-desktop` | Claude Desktop's platform-specific JSON config |
| `plumb setup claude-code` | `~/.claude.json` (user scope) |
| `plumb setup claude-code --project` | `.mcp.json` in the current directory (project scope) |
| `plumb setup codex` | `$CODEX_HOME/config.toml` (or `~/.codex/config.toml`) |
| `plumb setup gemini` | `~/.gemini/settings.json` |

| Flag | Applies to | Effect |
|---|---|---|
| `--project` | `claude-code` | Write to `.mcp.json` in the current directory (project-scoped) instead of the user-level config. |

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
- **MCP Clients** — whether plumb is registered with Claude Desktop, Claude
  Code, Gemini CLI, and Codex.
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
| `plumb config show [--workspace <dir>]` | Show the resolved configuration with **source provenance** — which layer (default, global, project, env) set each value. |

| Flag | Applies to | Default | Effect |
|---|---|---|---|
| `--workspace <dir>` | `show` | current dir | Resolve project-layer config from this workspace. |

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
and P95 latency, input/output bytes, errors, estimated tokens saved) and a list
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

## `plumb version`

```
plumb version
```

Print the plumb build version and the Go runtime version. The build version is
stamped at compile time (see [Versioning in AGENTS.md](../AGENTS.md)).
