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
| [`plumb skills`](#plumb-skills) | Show or sync plumb's embedded skills per client |
| [`plumb doctor`](#plumb-doctor) | Run health checks |
| [`plumb config`](#plumb-config) | Inspect resolved configuration |
| [`plumb sessions`](#plumb-sessions) | List active sessions |
| [`plumb mail`](#plumb-mail) | Report whether a session has unread agent-to-agent messages |
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
[TUI conventions in CONTRIBUTING.md](contributing.md#tui-conventions-bubble-tea-v2) for navigation details.

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
before modifying it. Registration is **config-only** — it never installs skill
files; those come from [`plumb skills sync`](#plumb-skills), and a named
registration prints a hint when it notices the client's skills missing or stale.

| Subcommand | Config target |
|---|---|
| `plumb setup claude-desktop` | Claude Desktop's platform-specific JSON config (macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`); also heuristically repoints any sibling `Claude*/claude_desktop_config.json` profile that already exists (e.g. `Claude-Personal`) |
| `plumb setup claude-code` | `~/.claude.json` (user scope) + `~/.claude/skills/` (skills via `plumb skills sync`) |
| `plumb setup claude-code --project` | `.mcp.json` in the current directory (project scope) + `~/.claude/skills/` (skills via `plumb skills sync`) |
| `plumb setup codex` | `$CODEX_HOME/config.toml` (or `~/.codex/config.toml`) + `$CODEX_HOME/skills/` (skills via `plumb skills sync`) |
| `plumb setup codex --lean` | Same, plus an `enabled_tools` allowlist pinning plumb's lean tool set |
| `plumb setup gemini` | `~/.gemini/settings.json` |
| `plumb setup gemini --lean` | Same, plus an `includeTools` allowlist pinning plumb's lean tool set |
| `plumb setup cursor` | `~/.cursor/mcp.json` (shared by the editor and the `cursor-agent` CLI) |
| `plumb setup augment` | `~/.augment/settings.json` (the `auggie` CLI) |
| `plumb setup qwen` | `~/.qwen/settings.json` |
| `plumb setup kimi-code` | `$KIMI_CODE_HOME/mcp.json` (or `~/.kimi-code/mcp.json`; `mcpServers` key — Kimi Desktop reads the same file, so one registration covers both) + `$KIMI_CODE_HOME/skills/` (skills via `plumb skills sync`) |
| `plumb setup kimi-code --lean` | Same, plus an `enabledTools` allowlist pinning plumb's lean tool set |
| `plumb setup antigravity` | `~/.gemini/config/mcp_config.json` (shared `mcpServers` config Antigravity reads); also repoints existing per-surface `~/.gemini/{antigravity-cli,antigravity-ide,antigravity}/mcp_config.json` |
| `plumb setup antigravity-desktop` | `~/.gemini/config/mcp_config.json` (same shared config; Antigravity regenerates the per-server `mcp/` dirs from it) |
| `plumb setup opencode` | `~/.config/opencode/opencode.json` (`mcp` key; `type:"local"`, command array) |
| `plumb setup crush` | `~/.config/crush/crush.json` (`mcp` key; `type:"stdio"`) |
| `plumb setup goose` | `~/.config/goose/config.yaml` (`extensions` key; YAML) |
| `plumb setup hermes` | `~/.hermes/config.yaml` (`mcp_servers` key; YAML) |

All clients funnel through one format-agnostic merge (`mergeServerEntry`)
backed by JSON, TOML, or YAML serialisers; config locations are resolved via
OS/user-home helpers — no hardcoded paths. (Aider is intentionally absent — it
has no native MCP **client**, only third-party servers that wrap it.)

| Flag | Applies to | Effect |
|---|---|---|
| `--project` | `claude-code` | Write to `.mcp.json` in the current directory (project-scoped) instead of the user-level config. |
| `--lean` | `kimi-code`, `codex`, `gemini` | Also write a **client-side tool allowlist** on the plumb entry, pinning the ~21 tools of plumb's lean set (`tools.LeanToolNames()`, its only source) so the client loads those schemas instead of all 57. Each client has its own key, and none supports globbing — the value is always exact tool names: `enabledTools` for Kimi Code, `enabled_tools` for Codex (a TOML array on `[mcp_servers.plumb]`), `includeTools` for Gemini CLI (whose sibling `excludeTools` wins wherever both are present; plumb never touches the deprecated global `tools.allowed`/`tools.exclude` settings). The saving has to be taken client-side because none of these clients carries a verified deferred-discovery capability: plumb's own `[tools] profile = "lean"` would remove capability rather than schemas, where a filtered-out tool in the client's own config is the user's explicit choice. The list is a **snapshot**: re-run `plumb setup <client> --lean` after upgrading plumb to refresh it. Re-running with `--lean` **replaces** a hand-edited list with plumb's; the bulk `plumb setup --all`/`--repair` sweeps carry no `--lean` state and therefore **preserve** whatever key is on disk, so a routine binary repoint never widens a surface you narrowed. A later **bare** named re-register differs by client: `plumb setup codex`/`plumb setup gemini` **clear** the key (the flag state on the command line is authoritative) and say so in their output, while `plumb setup kimi-code` **preserves** it — Kimi shipped first with no clearing path, so delete the key from `mcp.json` by hand to go back to its full surface. Because a bare re-register clears, `plumb doctor`'s repoint fix keeps `--lean` on the command it suggests whenever that client's config carries an allowlist today — following doctor's advice about a moved binary never widens a surface you narrowed. |
| | | `plumb doctor` grades all three the same way, one parameterised check per client. It mentions the flag when a client registers plumb without an allowlist (informational — a full surface is a valid default), stays silent when the allowlist equals today's lean set, and otherwise grades the key's *content* rather than its shape: a list naming no tool plumb registers earns a warning with a fix (it leaves the client with no plumb tools at all, however well-formed the file), an aged snapshot of the lean set earns an informational drift hint naming what is missing or no longer registered, and a value that cannot be an allowlist at all — `[]`, `null`, or a non-list — earns a warning worded for that specific shape, since only `[]` definitely means "no tools" (`null` most likely reads as no allowlist at all, and a wrong-typed value is one plumb cannot predict the client's handling of). |
| `--repair` | `plumb setup` | Repoint **every** already-registered client at the current `plumb` binary, skipping clients that aren't installed or don't use plumb. The bulk repair after the binary moves or is rebuilt elsewhere — pairs with `plumb doctor`'s registered-binary check. Re-points only; never adds plumb to a client that didn't have it. When installed-but-unregistered clients are found, it prints a hint pointing at `--all`. |
| `--all` | `plumb setup` | `--repair`, plus **register** plumb in installed clients that don't have it yet — any client whose config file already exists but has no plumb entry. Clients with no config file at all are left untouched (plumb can't tell an absent config from an uninstalled client — use the client's named subcommand to create one), with one exception: Kimi Code is detected via its data dir (`$KIMI_CODE_HOME`, or `~/.kimi-code`) because its `mcp.json` only exists once an MCP server is configured, so `--all` creates it fresh. Triggers the bulk run on its own, so `plumb setup --all` is the one-shot first-time setup for every client already present on the machine. `--install-missing` survives one release as a hidden, deprecated alias with the same behaviour. |

---

## `plumb skills`

```
plumb skills
plumb skills sync [client]
```

Bare `plumb skills` is **read-only**: a status table over the clients with a
verified skills directory (Claude Code `~/.claude/skills/`, Codex
`$CODEX_HOME/skills/` or `~/.codex/skills/`, Kimi Code `$KIMI_CODE_HOME/skills/`
or `~/.kimi-code/skills/`), showing each embedded skill as `installed`,
`missing`, or `stale` (content differs from the copy compiled into this
binary). A skill-capable client whose config does not register plumb is shown
as `not registered` — the reason `sync` would skip it. Every other client has
no skills directory and receives the same routing as the condensed
`session_start` guidance block instead.

Skill capability is per-client data (`setupTarget.skillsDirFn`,
`internal/cli/setup_skills.go`), verified against a live install rather than
inferred. Every other client's `skillsDirFn` stays `nil` until someone
verifies a real directory — writing files into a guessed path is worse than
not writing them — so `TestSkillCapableClients_ArePinned` makes the
skill-capable client set a deliberate edit, not something that drifts by
accident.

`plumb skills sync` installs or refreshes the eight embedded skills into the
skills directories of every skill-capable client that **registers plumb**, or
only the named client with `plumb skills sync <client>` (an unknown name is a
usage error listing the valid ones; naming an unregistered client is an error
pointing at `plumb setup <client>`). A changed skill is backed up before being
overwritten, an unchanged one is left alone, and a per-skill error is a
warning, not a failure. Sync is the only writer of skill files —
`plumb setup` is config-only.

Re-run `plumb skills sync` after upgrading plumb to pick up new skill content;
`plumb doctor` prints an informational line (never a warning) for any
registered client whose skills are missing or stale.

Clients with no skill channel get the same routing as a condensed
`session_start` block (`writeGenericGuidance`,
`internal/tools/session_start_guidance.go`) — one authored source, two render
targets: the skills are the canonical, expanded home for multi-tool
workflows, and tool descriptions and `session_start` guidance point at them
rather than restating them. The count is a stated ceiling, not drift: the
reasoning lives on `skillsFS` in `internal/cli/skills.go`, where each skill
past the stated limit has to argue against it in writing — `plumb-chat` is the
eighth and does so there, so a ninth argues against both paragraphs.

Only `SKILL.md` is installed. A skill directory in this repository may carry
supporting material — `plumb-chat` ships a `references/` note on waking an
idle agent — and that material stays in the repository; `sync` does not copy
it into a client's skills directory.

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
  `plumb setup <client>` fix hint — or run `plumb setup --repair` to repoint every
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

## `plumb mail`

```
plumb mail (--session <name> | --external-id <id> | --workspace <dir>) [--json]
```

Report how many agent-to-agent messages are waiting for a plumb session,
without reading or consuming them.

It exists for a client-side hook that wakes an idle agent. Plumb's mailbox
([`[collab] mailbox`](configuration.md)) delivers by polling — a message is
handed over on a tool result, a `check_messages` call, or `session_start` — so
an agent that has finished its turn and is waiting on its human never learns
that a peer wrote to it. Nothing server-side can reach it; this lets the client
ask the question from outside any session.

**It never claims.** The handle is `mode=ro`, so the delivery watermark cannot
be set: the messages stay undelivered and reach the agent through
`check_messages` as usual. A probe that consumed its answer would mark a message
delivered to an agent that never saw it — exactly-once turned into
exactly-never.

**It reports only whether, and how stale**: a count and the age of each waiting
message, never bodies, senders, or conversation ids. A session *name* is not an
identity (names come from a small pool, an ended session frees its name, and
`rename_session` lets a session pick one), and a message body is another agent's
free text — printed here it would flow straight into whatever consumes this
output, which is an injection channel into the agent the mailbox otherwise
labels these as unverified claims for.

| Flag | Default | Effect |
|---|---|---|
| `--session <name>` | — | A live session's name, as shown by `plumb sessions`. |
| `--external-id <id>` | — | The value a session passed to `session_start`'s `session_id` — a client's own conversation id. The reliable selector for a hook, which knows its conversation but not the session name. |
| `--workspace <dir>` | — | A directory, when exactly one live session is pinned there. Reports ambiguity rather than guessing. |
| `--json` | `false` | Emit `{"session","workspace","count","ages_seconds"}` instead of a sentence. `ages_seconds` is oldest first. |

Name exactly one selector. Exit status is 0 whether or not mail is waiting, and
non-zero only on error, so "has mail" is never confused with "the check failed"
— read the count. A workspace that has never used the mailbox has no
`collab.db`, which is reported as no mail rather than an error.

Scope: the workspace mailbox only. Cross-project messages live in the
daemon-level store behind the recipient's `[collab] cross_project` opt-in and
are not reported. Notes addressed to `"next"` are excluded, matching the listing
path `workspace_sessions` uses: such a note goes to whichever session claims it
first, so counting it for every candidate would report the same message to
several sessions that cannot all have it. The cost is that a `"next"` note left
while a session is idle will not show up here.

The `plumb-chat` skill's `references/idle-agent-wake-hook.md` gives the full
Claude Code Stop-hook recipe built on this command.

---

## `plumb stats`

```
plumb stats [--workspace <dir>] [--limit <n>] [--since <age>] [--failures]
```

Aliases: **`plumb status`**.

Show tool-call statistics for a workspace: a per-tool summary (calls, average
and P95 latency, input/output bytes, errors, token-efficiency estimate) and a list
of the most recent calls.

| Flag | Default | Effect |
|---|---|---|
| `--workspace <dir>` | current dir | Workspace to inspect. |
| `--limit <n>` | `20` | Number of recent calls to show. |
| `--since <age>` | all history | Only count calls newer than this age: `90m`, `24h`, `7d`, `2w`. |
| `--failures` | `false` | Replace the default view with a failure breakdown grouped by kind, tool and client build. |

`--failures` is the triage view. It groups failed calls by their machine-readable
kind (`dirty_file`, `lsp_timeout`, …), the tool, and the client build, and reports
how many of each were retryable. It is a separate view rather than extra columns
because the grain differs: the default table is one row per tool, a failure bucket
is one row per (kind × tool × client). `--limit` caps how many buckets are shown.

Failures plumb makes no structured claim about — and every call recorded before
the classification columns existed — appear under an explicit `unclassified`
label, always last, with their retryable count shown as `—` because the honest
answer is "unknown" rather than zero. They are fetched outside the `--limit`, so
raising or lowering it never makes them disappear, and the note beneath the table
counts **every** unclassified failure the filter matched, not just the rows on
screen. Nothing is inferred from the stored error text, so that bucket is honest
about what is unknown rather than folded into `internal`.

A bounded view says so: when `--limit` cuts buckets, a footer reports how many
buckets and failed calls the table is leaving out.

> The database is never pruned, so on an installation with history the
> pre-classification failures outnumber the classified ones for a long time.
> Classified buckets therefore sort ahead of the unclassified one regardless of
> size, and `--since` scopes the whole command to a window — `plumb stats
> --failures --since 7d` is the useful triage call.

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
plumb version [--json]
```

Print the plumb build version, the Go runtime version, and — when the binary
carries one — the source commit it was built from. The build version is stamped
at compile time (see [Versioning in AGENTS.md](../AGENTS.md)).

| Flag | Default | Effect |
|---|---|---|
| `--json` | `false` | Print a machine-readable report instead of the human line. Suppresses the logo banner. |

### Human output

A binary with no revision stamp prints exactly what plumb has always printed:

```
plumb 0.16.3 (go1.26.4)
```

A stamped binary extends that same single line with the abbreviated (12-char)
commit. The suffix distinguishes all three tree states — measured clean,
measured dirty, and not measurable — so `dirty` is never readable here without
its `known` counterpart either:

```
plumb 0.16.3 (go1.26.4, rev 4c6e4da9d8fa)         # measured, clean
plumb 0.16.3 (go1.26.4, rev 4c6e4da9d8fa-dirty)   # measured, dirty
plumb 0.16.3 (go1.26.4, rev 4c6e4da9d8fa-dirty?)  # could not be measured
```

It stays one line on purpose — `make install` echoes `plumb version | tail -1`,
and assorted tooling scrapes the first line.

### JSON output

```json
{
  "version": "0.16.3",
  "revision": "4c6e4da9d8fafc5ca36d762460caf6abf46c5ca6",
  "revision_known": true,
  "dirty": false,
  "dirty_known": true,
  "go_version": "go1.26.4",
  "os": "darwin",
  "arch": "arm64",
  "build_channel": "dev"
}
```

`revision_known` and `dirty_known` exist so a consumer can tell "clean" from "we
have no idea". An unstamped binary reports `revision: ""`, `revision_known:
false`, `dirty: false`, `dirty_known: false` — reading `dirty` without
`dirty_known` would misreport an unknown build as clean. `build_channel` is
`release` (a GoReleaser release build), `dev` (the Makefile, or a GoReleaser
`--snapshot` dry run), or `""` when unknown.

The same source commit is reported by the `daemon_info` MCP tool, on its
`source commit:` row, so the commit a *running daemon* was built from can be read
without shelling out to the binary.

### How the revision is stamped

The revision, its dirty flag, and the build channel are injected at link time,
alongside the version:

```
-X github.com/plumbkit/plumb/internal/cli.Revision=<full sha>
-X github.com/plumbkit/plumb/internal/cli.RevisionDirty=true|false
-X github.com/plumbkit/plumb/internal/cli.BuildChannel=dev|release
```

They are resolved in a fixed order: the ldflags stamps first, then Go's own
embedded `vcs.revision` / `vcs.modified` build settings, then unknown. Dirtiness
is always read from the same source as the revision — never mixed.

The **presence** of a revision stamp — not its plausibility — decides which
source answers. A stamp that is present but is not a plausible commit SHA (seven
or more hex digits and nothing else) yields *unknown*; it does **not** fall
through to the embedded VCS settings. GoReleaser renders `{{ .FullCommit }}` as
the literal string `none` when it cannot resolve git information, and the only
situations that produce such a stamp are also situations where the embedded
settings describe the *outer* module — so deferring to them would report
plumb-ops' HEAD as this repository's, confidently and wrongly. A wrong commit is
strictly worse than an honest "unknown".

Both stampers **measure** dirtiness rather than asserting it, and emit nothing
when they cannot — which lands as `dirty_known: false`. The Makefile treats a
failing `git status` as unknown rather than as clean (`git rev-parse` can succeed
while `git status` fails), and `.goreleaser.yml` uses `{{ .IsGitDirty }}` rather
than a hard-coded `false`, because `goreleaser --snapshot` skips the dirty-tree
validation a real release run enforces.

The explicit stamps exist because `debug.ReadBuildInfo()` alone gets this wrong
for plumb: the private plumb-ops superproject mounts this repository as a
submodule through `go.work`, so a build launched from the ops root resolves the
embedded VCS settings against the *outer* module — naming the wrong commit (or
none) and marking a clean tree modified. The `Makefile` always runs with its
working directory inside this repository, so `git rev-parse HEAD` there is
correct, and `.goreleaser.yml` passes `{{ .FullCommit }}` for releases.

No build timestamp is stamped, deliberately: development builds stay
reproducible.

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
