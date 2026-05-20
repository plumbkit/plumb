# plumb

[![Go Reference](https://pkg.go.dev/badge/github.com/golimpio/plumb.svg)](https://pkg.go.dev/github.com/golimpio/plumb)
[![Go Report Card](https://goreportcard.com/badge/github.com/golimpio/plumb)](https://goreportcard.com/report/github.com/golimpio/plumb)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

**Real IDE intelligence for AI assistants** — go-to-definition, find-references, rename, diagnostics, atomic file editing, and semantic refactors, powered by the same language servers your editor uses.

Plumb is an [MCP](https://modelcontextprotocol.io) (Model Context Protocol) server that bridges AI assistants to [LSP](https://microsoft.github.io/language-server-protocol/) (Language Server Protocol) language servers. Instead of dumping raw source files into the assistant's context window, plumb exposes 34 structured tools so the assistant can query and edit a codebase the way an IDE would.

---

## Why Plumb

LLM coding assistants typically work by reading files into the context window. This approach is token-heavy, lossy at scale, and blind to symbol semantics. Plumb provides a superior architecture built on three pillars:

### 1. Semantic Intelligence
Plumb gives the assistant the same primitives your editor already has:
- **LSP-backed refactors** — `rename_symbol`, `replace_symbol_body`, and `safe_delete_symbol` understand scope, types, and references.
- **Real diagnostics inline** — Actual compiler output from `gopls` or `pyright` is appended to every write response, so the assistant learns it broke the build instantly.
- **Symbol search** — Scoped search across the workspace, filtered to your code (no stdlib or dependency noise).

### 2. Concurrency & Safety
Building with agents requires industrial-grade file safety:
- **Atomic I/O** — Writes are staged in temporary files and renamed into place. No partial writes, ever.
- **Per-path locking** — The daemon serialises concurrent writes to the same file from any session, preventing race conditions.
- **Multi-file transactions** — Apply edits across 50+ files with guaranteed atomic rollback if any part fails.

### 3. Visibility & Context Efficiency
- **Token Savings** — Only read the symbols or line ranges you need. No more loading 2000-line files to find one function.
- **Session Bootstrap** — `session_start` orients the assistant in one round-trip: workspace, git branch, recent commits, and active diagnostics.
- **Durable Memory** — Project-specific markdown notes travel with the repo and are exposed as MCP resources.

---

## Monitoring (TUI)

Engineers love visibility. Plumb includes a live Bubble Tea TUI to monitor what your agents are doing in real-time.

- **Live Dashboard:** Monitor daemon health, activity history, and tokens saved.
- **Session Inspector:** Drill down into every tool call, see request/response payloads, and debug failures.
- **Live Logs:** Stream daemon logs with filtering and follow support.

*Run `plumb` or `plumb status` from your terminal to launch the dashboard.*

---

## Quick Start

### 1. Prerequisites
Ensure the language servers you need are on your `$PATH`:

```sh
# Go
go install golang.org/x/tools/gopls@latest

# Python (optional)
npm install -g pyright
```

### 2. Install & Setup
```sh
# Install plumb
go install github.com/golimpio/plumb/cmd/plumb@latest

# Connect your assistant
plumb setup claude-desktop  # For Claude Desktop
plumb setup claude-code     # For Claude Code (user-level)
plumb setup codex           # For Codex
```

### 3. Initialise a Project
Go to your project root and run:
```sh
plumb init
```

---

## Core Capabilities

Plumb exposes 34 tools across several categories. For a full API reference, see [**docs/mcp-tools.md**](docs/mcp-tools.md).

- **Session:** One-shot bootstrap, identity tracking, and daemon info.
- **LSP Queries:** Definitions, references, symbols, call/type hierarchies, and diagnostics.
- **LSP Edits:** Scope-aware renames and targeted symbol insertions/deletions.
- **Filesystem:** Atomic writes, unique-string replacement, recursive search, and parallel reads.
- **VCS & Memory:** Git inspection, multi-file transactions, and durable project notes.

---

## How it works

`plumb serve` is a thin stdio proxy. When an assistant opens a project, it calls `plumb serve`, which connects to a shared background daemon.

```
Assistant (Claude, Gemini, etc.)
  └── plumb serve  (thin proxy, one per conversation)
        └── ~/Library/Caches/plumb/plumb.sock
              └── plumb daemon  (one shared process)
                    ├── gopls for /projects/foo
                    └── pyright for /projects/bar
```

**Benefits:**
- **Warm Servers:** LSPs stay warm between sessions — no re-indexing when you start a new chat.
- **Shared State:** One daemon manages all connections, ensuring per-path locks work across different chat windows.
- **LSP Native:** Full support for `workspace/didChangeWatchedFiles`, keeping symbol indexes live after every write.

---

## Configuration

Plumb is highly configurable via `config.toml` files (global or per-project) or environment variables.

```toml
[edits]
strict = true                  # require read_file before edit_file
rate_limit_per_minute = 30     # prevent runaway agent loops
```

Run `plumb config show` to see your resolved configuration.

## Contributing

See [`AGENTS.md`](AGENTS.md) for architecture details and code style. We follow Australian English conventions for all prose.

## License

MIT License - see [LICENSE](LICENSE) for details.
