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
anyway.

## The TUI / `plumb sessions` is stuck on "resolving…"

The session attached but no workspace resolved — usually a project with no
`.plumb/` marker and no recognised language root marker. Fixes:

- Run `plumb init` in the project root to create a `.plumb/` marker, or
- Enable the synthetic-root fallback: `[workspace] auto_attach = true` (see
  [Configuration](configuration.md#workspace--root-detection-fallback)).

## Python or Java tools don't work

Installing the language-server binary is not enough — the language must be
enabled in config. Add `[lsp.python] enabled = true` (or `[lsp.java]`) and make
sure the binary (`pyright-langserver` / `jdtls` + Java 21+) is on your `PATH`.
See [Getting Started → Enabling more languages](getting-started.md#enabling-more-languages).

## No diagnostics appear after a write

The language server may be slower than the post-write wait. Raise
`[edits] post_write_diagnostics_ms` (e.g. to `1000`). On a cold module cache,
gopls may emit "not in your go.mod file" — run `go mod tidy`.

## `rename_symbol` fails with "out of range"

The LSP position index is stale after in-session edits. Recovery options:

1. Call `diagnostics` to confirm the server re-indexed, then retry.
2. Fall back to `find_replace` for the fully-qualified name, then fix bare-name
   references in comments manually.
3. `plumb stop --force` to restart the daemon if re-indexing doesn't help.

## "LSP server not yet ready" / language is `none`

The workspace is marked (`.plumb/` present) but has no *enabled* LSP language.
Filesystem tools, stats, and topology still work; LSP-backed tools won't until a
language attaches. Enable the relevant `[lsp.<language>]` and install its binary.

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
- Tail the daemon log (path shown by `plumb doctor`; under the system cache dir,
  e.g. `~/Library/Caches/plumb/daemon.log` on macOS).
- `plumb config show --workspace .` to confirm the resolved configuration.
