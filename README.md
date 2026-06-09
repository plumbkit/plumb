# plumb

[![CI](https://github.com/plumbkit/plumb/actions/workflows/ci.yml/badge.svg)](https://github.com/plumbkit/plumb/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/plumbkit/plumb.svg)](https://pkg.go.dev/github.com/plumbkit/plumb)
[![Go Report Card](https://goreportcard.com/badge/github.com/plumbkit/plumb)](https://goreportcard.com/report/github.com/plumbkit/plumb)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Give your coding agent real IDE intelligence — and let it run unattended without corrupting your code.**

Plumb is an [MCP](https://modelcontextprotocol.io) server that bridges AI coding agents to [LSP](https://microsoft.github.io/language-server-protocol/) language servers — the same ones your editor uses. Other tools make an agent *semantic*. Plumb makes it semantic **and safe**: concurrency-correct atomic writes, multi-file transactions with rollback, a crash-resilient daemon, and scoped filesystem + git access you control. It's a single pure-Go binary — no Python or Node toolchain to install.

<!-- TODO(launch): record a 20–30s asciinema/vhs demo (agent editing a multi-file Go
     project + recovering from a forced daemon crash) and embed it here as
     docs/assets/demo.gif. This single asset converts better than any paragraph. -->

---

## Why Plumb

LLM agents usually work by reading whole files into the context window — token-heavy, lossy at scale, blind to symbol semantics, and unsafe to let loose on a real repo. Plumb is built on three pillars, in priority order.

### 1. Reliability & write-safety (the part nobody else gets right)
Leaving an agent to edit a codebase for an hour is only viable if writes can't corrupt files and a crash can't wedge your session.

- **Atomic I/O** — every write is staged in a temp file and renamed into place. No partial writes, ever. Symlink-aware, CRLF-tolerant.
- **Per-path locking** — the daemon serialises concurrent writes to the same file across every session and chat window. No races.
- **Multi-file transactions** — apply edits across dozens of files with guaranteed atomic rollback if any step fails.
- **Crash-resilient daemon** — `plumb serve` is a reconnecting proxy. If the daemon crashes or hangs, it respawns one and replays the handshake; the agent never notices. In-flight writes are never silently re-run.
- **Optimistic concurrency** — mtime/sha guards catch stale edits before they clobber newer changes.

### 2. Semantic intelligence
The same primitives your editor has, exposed as structured tools:

- **LSP-backed refactors** — `rename_symbol`, `replace_symbol_body`, `safe_delete_symbol` understand scope, types, and references.
- **Real diagnostics inline** — actual `gopls`/`pyright` output is appended to every write, so the agent learns it broke the build immediately.
- **Symbol search** — scoped to your code, no stdlib or dependency noise.

### 3. Context efficiency & safety controls
- **Read only what you need** — symbols or line ranges, not 2,000-line files.
- **Scoped access you control** — a per-connection path allowlist (read-only vs read-write roots) plus tiered git gating (destructive and network operations are off by default and need explicit confirmation). See [SECURITY.md](SECURITY.md).
- **One-round-trip bootstrap** — `session_start` returns workspace, branch, recent commits, diagnostics, and project memory.

---

## Install

Plumb is a single binary. Pick whichever you like:

```sh
# Homebrew (macOS + Linux) — recommended
brew install plumbkit/plumb/plumb

# Go
go install github.com/plumbkit/plumb/cmd/plumb@latest

# Or download a prebuilt binary from the Releases page:
# https://github.com/plumbkit/plumb/releases
```

> **macOS note:** prebuilt binaries are not yet notarised — on first run you may need
> `xattr -d com.apple.quarantine ./plumb`, or right-click → Open. Homebrew installs
> avoid this.

### Connect your agent and go

```sh
plumb setup claude-desktop   # also: claude-code, codex, gemini, cursor, …
cd your/project && plumb init
```

`plumb setup` writes the MCP config for you — no hand-editing JSON. Then make sure the language servers you need are on your `$PATH`:

```sh
go install golang.org/x/tools/gopls@latest   # Go
npm install -g pyright                        # Python
```

Full walkthrough: [**docs/getting-started.md**](docs/getting-started.md).

---

## Language support (honest version)

Plumb negotiates LSP capabilities per language and also ships a pure-Go tree-sitter index for language-server-free search and navigation. Support comes in tiers — we'd rather be precise than claim a big number.

| Tier | Languages | What you get |
|---|---|---|
| **First-class** (CI-tested, real-binary integration) | **Go** (gopls), **Python** (pyright) | Full LSP: definitions, references, rename, diagnostics, hierarchies + all write tools |
| **Validated, opt-in** | **Java** (jdtls), **Rust** (rust-analyzer), **Swift** (sourcekit-lsp) | Full LSP; enable per-language and put the server on `$PATH` |
| **Experimental, opt-in** | **TypeScript/JS**, **Kotlin**, **Zig**, **HTML** | Works; less battle-tested. Enable with `[lsp.<lang>] enabled = true` |
| **Search & navigation** (tree-sitter, no LSP needed) | 15+ incl. JS/TS/TSX, Bash, SQL, HCL, Dockerfile, TOML, YAML, Markdown | Ranked symbol search, outlines, graph exploration via the Topology index |

Real-binary validation has been exercised on **macOS and Linux** (see CI). Windows is [tracked but not yet supported](https://github.com/plumbkit/plumb/issues) — the daemon's Unix-socket architecture needs a port.

---

## How it works

`plumb serve` is a thin, reconnecting stdio proxy. The real work happens in one shared background daemon, so language servers stay warm across chats.

```
Agent (Claude, Codex, Gemini, …)
  └── plumb serve   (reconnecting proxy, one per conversation)
        └── ~/Library/Caches/plumb/plumb.sock   (~/.cache/plumb on Linux)
              └── plumb daemon   (one shared process)
                    ├── gopls for /projects/foo
                    └── pyright for /projects/bar
```

Warm servers (no re-indexing each chat), shared per-path locks across all connections, and full `workspace/didChangeWatchedFiles` support so symbol indexes stay live after every write.

---

## Monitoring (TUI)

Run `plumb` with no arguments to launch a live [Bubble Tea](https://github.com/charmbracelet/bubbletea) dashboard: daemon health and tokens saved, a session inspector for every tool call, and streaming logs with follow + filtering.

---

## Core capabilities

Plumb exposes **50 tools**. The ones you'll use constantly:

`session_start` · `find_symbol` · `get_definition` · `find_references` · `rename_symbol` · `edit_file` · `transaction_apply` · `diagnostics`

The rest cover filesystem reads/writes, LSP hierarchies, tiered git, an optional SQLite/FTS5 **Topology** index (ranked search + blast-radius/route analysis with no language server), and durable per-project memory. Full API reference: [**docs/tools.md**](docs/tools.md).

---

## Configuration

Global or per-project `config.toml`, or environment variables. Run `plumb config show` to see the resolved config with provenance.

```toml
[edits]
strict = true                  # require read_file before edit_file
rate_limit_per_minute = 30     # bound runaway agent loops

[git]
allow_destructive = false      # reset/checkout/rebase off by default
allow_push = false             # push/fetch/pull off by default
```

Full settings reference: [**docs/configuration.md**](docs/configuration.md).

---

## How Plumb compares

| | **Plumb** | **Serena** | Thin `lsp-mcp` bridges | Agent's built-in file tools |
|---|---|---|---|---|
| LSP-backed semantics | ✅ | ✅ | ✅ (often 1 language) | ❌ |
| Concurrency-safe atomic writes | ✅ | ⚠️ | ❌ | ❌ |
| Multi-file transactions w/ rollback | ✅ | ❌ | ❌ | ❌ |
| Crash-resilient daemon / auto-recovery | ✅ | ⚠️ (crashes reported) | ❌ | n/a |
| Scoped path + tiered git safety | ✅ | ⚠️ | ❌ | ❌ |
| Single binary, no runtime deps | ✅ (pure Go) | ❌ (Python) | varies | n/a |
| Language breadth (full LSP) | Focused, validated tiers | 40+ (mostly install-it-yourself) | 1–few | n/a |

Plumb's bet: most agents can already *read* code well enough. What's missing is the ability to *write* — concurrently, transactionally, and recoverably — without supervision. If you need maximum language breadth today, [Serena](https://github.com/oraios/serena) is excellent; if you need an agent you can trust to edit a real codebase unattended, that's Plumb.

---

## Roadmap

Near-term, roughly in order: green Linux CI + Homebrew distribution, more validated LSP adapters promoted out of experimental, opt-in semantic re-rank for Topology search (GA), and Windows support. Issues and ideas welcome.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md) for architecture and code style. We follow Australian English in all prose. By contributing you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

MIT — see [LICENSE](LICENSE).
