# Troubleshooting

Start here when something isn't working. The fastest first step is almost always:

```sh
plumb doctor --workspace .
```

It checks the daemon, language servers, MCP client registrations, config, and
the stats DB, and prints a one-line fix for anything that fails.

## The daemon is running old code after a rebuild

The daemon is long-lived and shared across conversations, so it keeps running
the binary it started with. After rebuilding:

```sh
plumb stop           # prompts if sessions are active
plumb stop --force   # skip the prompt (useful in scripts/Makefiles)
```

`plumb serve` warns on stderr when the running daemon's version differs from
your binary.

## "… has not been read in this session"

Strict mode is on (`[edits] strict = true`). `edit_file` requires the target to
have been read via `read_file` first, with a matching mtime. Read the file, then
pass its `mtime` header back as `expected_mtime`. To disable strict mode, unset
`[edits] strict` or `PLUMB_STRICT_EDITS`.

## Writes are being throttled / rate-limited

You hit the per-session write cap (default 120/min). Wait, raise
`[edits] rate_limit_per_minute`, or set `PLUMB_WRITE_RATE_LIMIT=0` to disable it.

## "… has uncommitted changes" on a write

The target file is dirty in git and a write tool refused to clobber it. Review
and commit the changes, or pass `dirty_ok: true` to the write call to proceed
anyway. This guards only pre-existing uncommitted work plumb did not create —
re-editing a file plumb wrote this session is never blocked. If it fires too
often for your workflow (you iterate on uncommitted WIP), disable the guard
with `[edits] block_dirty_writes = false` (or `PLUMB_BLOCK_DIRTY_WRITES=0`).

## The TUI / `plumb sessions` is stuck on "resolving…"

The session attached but no workspace resolved. The workspace resolves on the
first tool call that carries a path, so a brand-new session can briefly show
"resolving…" until then — if it persists, the directory has no recognised
boundary: no `.plumb/` marker, no language root marker (`go.mod`,
`pyproject.toml`, …), **and no `.git/` directory** in it or any ancestor.

A git repository *is* a recognised boundary (since 0.7.20): a repo with no
language marker resolves to its git root with language `none` (filesystem tools,
stats, memory, and project config all work; LSP tools are unavailable). So the
remaining stuck case is a directory that is neither a git repo nor a marked
project. Fixes:

- Run `plumb init` in the project root to create a `.plumb/` marker, or
- `git init` the project (any git repo now resolves), or
- Enable the synthetic-root fallback: `[workspace] auto_attach = true` (see
  [Configuration](configuration.md#workspace--root-detection-fallback)).

**Claude Desktop specifically:** Desktop does not tell plumb which folder you're
working in (it sends no MCP `roots`), and the daemon is shared across all your
conversations — so a fresh Desktop session has no workspace until it gets one.
If your MCP entry launches `plumb serve` from the project directory, the proxy
now transports that directory as an attach hint and the workspace resolves
automatically (the hint is validated against project markers and never
overrides an explicit pin). Otherwise, pin the project by passing an absolute
path to `session_start`:
`session_start({"workspace": "/Users/you/projects/myapp"})` (passing `workspace`
or an absolute `path` to any tool also pins it). plumb never guesses the
workspace from the shared *daemon's* launch directory, so with no roots, no
usable serve cwd, and no explicit pin it will say "resolving…" / return a
"pass `workspace`" error rather than silently attach the wrong project.

If you recently upgraded plumb but the daemon is still on the old build, the
fix won't be active — restart it with `plumb stop --force` (it respawns on the
next client request). The TUI footer shows the running daemon version; if it
lags your `plumb version`, the daemon needs restarting.

## A language's tools don't work

Plumb activates a language automatically when its server binary is on your
`PATH` — so the usual cause is a missing or unfound server. Confirm the binary
(`pyright-langserver`, `rust-analyzer`, `jdtls` + Java 21+, …) is installed and
on `PATH`, then restart the daemon (`plumb stop`) so a running daemon picks it
up. `plumb doctor` and `plumb config show` print an `active` row per language
telling you whether it's installed and enabled. If a language is *active* but you
want it off, set `[lsp.<lang>] enabled = false`.
See [Getting Started → Enabling more languages](getting-started.md#enabling-more-languages).

## No diagnostics appear after a write

The language server may be slower than the post-write wait. Raise
`[edits] post_write_diagnostics_ms` (e.g. to `1000`). On a cold module cache,
gopls may emit "not in your go.mod file" — run `go mod tidy`.

## How do I read the resolved diagnostics mode?

`[lsp.<lang>] diagnostics` defaults to `auto` (push for every adapter today);
setting it to `pull` negotiates the LSP 3.17 `textDocument/diagnostic` model
when the server supports it (see
[Configuration → Diagnostics mode](configuration.md#lsplanguage--language-servers)).
plumb never infers the connection's mode from cache contents — it's the
recorded negotiation outcome, surfaced in four places:

- `plumb doctor` — the live-server detail line appends `diagnostics: <mode>`
  only when it isn't the default `push`.
- `plumb debug lsp` (the `lsp-status` line) — the `diag=<mode>` field on each
  server row.
- `daemon_info` — the `lsp:` row appends `, diagnostics: <mode>` for a
  non-push mode.
- `session_start` — the same non-default-only annotation on the LSP identity
  line.

The four resolved values are `push`, `pull`, `hybrid` (the server answers
pulls AND keeps pushing — e.g. gopls forced into pull), and
`pull-requested-but-unavailable` (you asked for `pull` but the server never
advertised `diagnosticProvider` — e.g. typescript-language-server, zls; plumb
logs one warning and the connection behaves as push).

**The `-32601` downgrade.** If a negotiated `pull`/`hybrid` connection ever
gets a method-not-found response from a pull request, plumb flips that
connection to `push` for the rest of the session, logs one warning (`pool:
pull diagnostics returned method-not-found — downgrading to push for this
session`), and does not retry the pull or flap back. The downgrade is sticky
for the life of the pool entry: it survives a hibernation wake (which re-runs
negotiation) rather than re-pulling and re-warning once per wake, and is
re-negotiated only on a genuine restart (a fresh entry / explicit server
restart). A single-URI `diagnostics` call falls back to the push open-and-wait
path; a multi-URI call now re-verifies the downgraded file inline the same way
— open-and-wait — surfacing it as UNVERIFIED only if that verification itself
cannot complete (an unreadable file or an indexing timeout), never silently
dropped or silently clean.

**Pull failures degrade explicitly, never silently.** A pull request that
errors for any other reason never reports a false "No issues" — the
`diagnostics` tool always surfaces the error text plus either the last-known
cached diagnostics (labelled possibly stale) or, if nothing is cached yet, an
explicit notice that the file's state is unverified. Retry, or set
`[lsp.<lang>] diagnostics = "push"` if a server proves unreliable under pull.

## `rename_symbol` fails with "out of range"

The LSP position index is stale after in-session edits. Recovery options:

1. Call `diagnostics` to confirm the server re-indexed, then retry.
2. Fall back to `find_replace` for the fully-qualified name, then fix bare-name
   references in comments manually.
3. `plumb stop --force` to restart the daemon if re-indexing doesn't help.

## "LSP server not yet ready" / language is `none`

The workspace resolved but no LSP language attached — either no language server
for the project's language is installed (install it and restart the daemon with
`plumb stop`), or the root has no recognised language marker. Filesystem tools,
stats, and topology still work; LSP-backed tools won't until a language attaches.
For a project with sources but no marker (e.g. a loose Xcode / `.swift`
directory), pass `session_start({"language": "swift"})` to force the primary
server.

## Swift semantic tools are empty in an Xcode project

SourceKit-LSP needs Build Server Protocol metadata for a bare `.xcodeproj` or
`.xcworkspace`. Run `plumb doctor --workspace .`; it distinguishes missing or
malformed configuration, workspace trust, generation/restart warm-up, missing
build data, and a non-empty semantic query that proves readiness.

Manual setup remains available through doctor's exact `xcode-build-server config`
command. For automatic setup, review the workspace and add:

```toml
[xcode]
auto_build_server = true
# scheme = "MyApp"  # required only when discovery finds several schemes
```

Then run `plumb trust` in the workspace and start a new session. Plumb validates
the marker and scheme, generates `buildServer.json` with bounded argv-only
commands, and restarts only that root's SourceKit-LSP. It never runs a project
build or a shell. The external `xcode-build-server` program still invokes Xcode
and may interpolate Xcode-derived values internally, so trust only a workspace you
have reviewed.

Configuration alone does not provide compiler flags. If doctor reports warming or
unproven semantics, build the selected scheme once in Xcode; Plumb will not do so
automatically. `session_start`, `workspace_symbols`, `get_definition`, and
`find_references` repeat lifecycle-aware guidance when applicable.

## Too much (or too little) log output

Change the running daemon's level instantly, no restart:

```sh
plumb log-level warn     # quieter
plumb log-level debug    # verbose
plumb log-level reset    # back to the startup/config level
```

To make it permanent, set `log_level` in `~/.config/plumb/config.toml`.

## Still stuck?

- `plumb doctor --json` for machine-readable check output.
- Tail the daemon log (path shown by `plumb doctor`; under the OS log dir,
  e.g. `~/Library/Logs/plumb/daemon.log` on macOS,
  `~/.local/state/plumb/daemon.log` on Linux).
- `plumb config show --workspace .` to confirm the resolved configuration.
