# Getting Started

This guide takes you from nothing to a working plumb setup: install the binary,
connect your AI assistant, initialise a project, and confirm everything works.

Plumb is an [MCP](https://modelcontextprotocol.io) server that gives AI
assistants real IDE intelligence — go-to-definition, find-references, rename,
diagnostics, atomic edits — backed by the same [LSP](https://microsoft.github.io/language-server-protocol/)
language servers your editor uses. For the bigger picture, see the
[Architecture](architecture.md) doc.

## 1. Prerequisites

**A language server for each language you work in, on your `PATH`.** Plumb
activates a language automatically when its server binary is installed — there
is no per-language config step:

```sh
go install golang.org/x/tools/gopls@latest    # Go
npm install -g pyright                          # Python
# Java: jdtls + a Java 21+ runtime
# also: rust-analyzer, sourcekit-lsp, zls, typescript-language-server,
#       kotlin-language-server, vscode-html-language-server
```

Every supported language is enabled by default and activates the moment its
server is on your `PATH`. To *exclude* one even when its server is installed,
set `[lsp.<lang>] enabled = false` (see [below](#enabling-more-languages)).

**The Go toolchain** (1.26+) if you install plumb with `go install` or build
from source.

## 2. Install plumb

```sh
go install github.com/plumbkit/plumb/cmd/plumb@latest
```

Or build from source:

```sh
git clone https://github.com/plumbkit/plumb
cd plumb
make build        # produces ./plumb, version stamped from git/VERSION
```

Confirm the install:

```sh
plumb version
```

## 3. Connect your assistant

`plumb setup <client>` registers the current binary as a stdio MCP server. It
preserves any MCP servers you already have and backs up the config first.

```sh
plumb setup claude-desktop      # Claude Desktop
plumb setup claude-code         # Claude Code (user scope, ~/.claude.json)
plumb setup claude-code --project  # Claude Code (project scope, ./.mcp.json)
plumb setup codex               # Codex
plumb setup gemini              # Gemini CLI
```

| Client | Config written |
|---|---|
| Claude Desktop | Platform-specific Claude Desktop JSON config |
| Claude Code (user) | `~/.claude.json` |
| Claude Code (project) | `.mcp.json` in the current directory |
| Codex | `$CODEX_HOME/config.toml` (or `~/.codex/config.toml`) |
| Gemini CLI | `~/.gemini/settings.json` |

> **Claude Desktop:** fully quit and reopen the app (Quit, not just close the
> window) after running setup so it reloads its MCP config.

You don't need to start anything by hand — when the assistant connects, it runs
`plumb serve`, which spawns the shared background daemon automatically.

## 4. Initialise a project

From your project root:

```sh
plumb init                  # create .plumb/ + a blank context.md
plumb init --discover       # also auto-detect languages, build, entry points, tests
```

This creates a `.plumb/` marker directory holding `context.md` (project notes
loaded on every session), the `memories/` store, and — if you enable it —
`topology.db`. Commit `.plumb/` to share context with your team, or add it to
`.gitignore` to keep it local.

A `.plumb/` marker is optional for Go/Python/Java projects (plumb also detects
`go.mod`, `pyproject.toml`, etc.), and any git repository resolves to its git
root even with no language marker (as language `none` — filesystem tools,
stats, and memory still work). A `.plumb/` marker is still worthwhile: it pins
the workspace root explicitly and is where per-project
[configuration](configuration.md) lives.

## 5. Confirm it works

Run the health check:

```sh
plumb doctor --workspace .
```

You want green checks under **Daemon**, **Language Servers**, **MCP Clients**,
**Configuration**, and **Data**. The output names a fix for anything that fails.

In your assistant, the first call each session should be `session_start` (Claude
clients expose it as the `/orient` prompt). It returns an orientation packet:
workspace, language, git branch, recent commits, recently-modified files,
memories, top tool usage, and active diagnostics.

While a session is live, watch it from a terminal:

```sh
plumb               # the live TUI dashboard
plumb sessions      # active sessions, one per connection
plumb stats         # per-tool call statistics for this workspace
```

## Enabling more languages

Installing a language-server binary is all it takes — plumb enables every
supported language by default and activates one automatically when its server is
on your `PATH`. Install `pyright-langserver` and Python is live; install
`rust-analyzer` and every Cargo project resolves as Rust; and so on. Restart the
daemon (`plumb stop`) or start a new session to pick up a newly-installed server.

Go and Python are validated; Rust, Swift, Zig, TypeScript/JavaScript, Kotlin, and
HTML are experimental (see the *Adapter validation status* table in `AGENTS.md`).

To **exclude** a language even when its server is installed, set `enabled = false`
in `~/.config/plumb/config.toml` (global) or `<workspace>/.plumb/config.toml`
(project):

```toml
[lsp.python]
enabled = false   # don't activate Python even though pyright is on PATH
```

See the [Configuration reference](configuration.md#lsplanguage--language-servers)
for the full per-language settings.

## After a rebuild

The daemon is long-lived and shared across conversations. If you rebuild plumb,
the old daemon keeps running your old code until you restart it:

```sh
plumb stop           # prompts if sessions are active
plumb stop --force   # skip the prompt (useful in scripts/Makefiles)
```

`plumb serve` warns on stderr when the running daemon's version differs from
your binary.

## Next steps

- [CLI Reference](cli-reference.md) — every command and flag.
- [Configuration Reference](configuration.md) — all settings and environment variables.
- [Tools](tools.md) — the full MCP tool API.
- [Architecture](architecture.md) — how the daemon, proxy, and language servers fit together.
- [Troubleshooting](troubleshooting.md) — when something doesn't work.
