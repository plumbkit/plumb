# Plumb — Agent Instructions

> Source of truth: edit `AGENTS.md` only. `CLAUDE.md` and `GEMINI.md` are relative symlinks to it; never edit, replace, or unlink those paths.

This is the always-loaded engineering contract for the public Plumb codebase. Keep rules here and reference detail in `docs/`; `make check-brief` enforces the budget.

## Working contract

- **Use Plumb's lane.** When Plumb exposes a capability, use its MCP tool instead of native file, search, shell, or Git tools. Native tools bypass Plumb's concurrency guards, diagnostics, and session tracking. Exceptions are capabilities Plumb does not cover and read-only commodity tools hidden by a lean profile.
- **Bootstrap and guard writes.** Call `session_start` first. After `read_file`, use Plumb edits and pass its `mtime` or `sha256` as `expected_mtime` or `expected_sha` when a concurrent writer may intervene. Prefer configured `run_task` slots over raw build/test/lint commands.
- **Trust live descriptions.** Tool behaviour and schemas come from `tools/list`; `session_start` supplies client-specific guidance. This brief is orientation, not a second tool reference.
- **Avoid version drift.** Read `VERSION` and `CHANGELOG.md` for the current version.

## Purpose and invariants

Plumb gives coding agents an IDE intelligence layer over MCP: LSP semantics, a tree-sitter topology index, project memory, and a complete safe-filesystem toolkit. `plumb serve` is a resilient stdio proxy in front of a shared daemon.

1. **LSP-correct semantics.** Writes notify language servers through `workspace/didChangeWatchedFiles`; capabilities are negotiated, including server-initiated registration.
2. **Concurrency-safe durability.** Write tools use per-path locks, optimistic checks, atomic rename, and the shared durable-write path.
3. **Per-session isolation.** Read tracking, rate limits, and caches belong to a connection or declared agent, never accidental process globals.
4. **Configurable safety.** Compiled defaults, global config, project config, and environment overrides resolve with explicit provenance.

## Architecture

Lower layers never import higher ones. `internal/arch` assigns every first-party package and tests reject violations, missing assignments, and stale entries.

```
Foundation (paths, durable writes, tokenisation, redaction, colour data)
→ Transport (MCP/LSP) → Domain (symbols, edits, capabilities)
                    → Intelligence (topology)
                    → Application (composite tools, caching, rate-limiting)
                    → Presentation (TUI, CLI)
```

The package map, daemon/persistence layout, extractor choices, and concurrency model live in [`docs/architecture.md`](docs/architecture.md). Hard seams worth keeping in context:

- `internal/mcp` and `internal/lsp` are transport; `internal/topology` is intelligence; `internal/tools`, `internal/cache`, and `internal/quality` are application; `internal/cli` and `internal/tui` are presentation.
- `internal/lsp/adapters/base.Adapter` exports exactly `lsp.Client`. Put escape hatches in package functions, not methods, or every embedded adapter gains a false capability.
- Every durable write goes through `internal/fsync.AtomicWrite`; every SQLite DSN goes through `internal/sqlitex`.
- `internal/langsupport` is the source of truth for each language's structural engine and LSP adapter.

## Reference map

- Architecture, package map, daemon, workspace detection, persistence: [`docs/architecture.md`](docs/architecture.md).
- Config precedence, every section, reload tiers, trust: [`docs/configuration.md`](docs/configuration.md).
- Tool inputs and behaviour: each MCP description, then [`docs/tools.md`](docs/tools.md).
- Setup, hooks, skills, doctor, session/mail CLI: [`docs/cli-reference.md`](docs/cli-reference.md).
- Build, style, testing, commits, and TUI rules: [`docs/contributing.md`](docs/contributing.md).
- Adapter validation and implementation: [`docs/adding-an-lsp.md`](docs/adding-an-lsp.md).

## Configuration and clients

Resolution is compiled defaults → global config → project `.plumb/config.toml` → environment; use `plumb config show` for values and provenance. The lean profile hides the non-lean remainder (37 tools today, out of 58) only for clients with verified deferred discovery; hidden tools remain callable by name.

`plumb setup <client>` registers MCP configuration. `plumb skills sync` installs eight idempotent user-scoped skills for clients that read `SKILL.md`: `plumb-explore`, `plumb-refactor`, `plumb-testing`, `plumb-minimal-change`, `plumb-memory`, `plumb-diagnose`, `plumb-git`, and `plumb-chat`. Lifecycle hooks are separate and opt-in through `plumb hooks install [client]`.

Contributor recipes live in `.claude/skills/`: use `add-mcp-tool` for a new tool and `add-lsp-adapter` for a new language server.

## Available tools (58)

Full schemas live in `tools/list` and [`docs/tools.md`](docs/tools.md). Use the smallest semantic lane that answers the question: `workspace_search` → topology/LSP → exact `search_in_files` → bounded `read_file`. Prefer semantic edits for symbols, Plumb filesystem writes for text, the policy-gated `git` tool for repositories, and `topology_affected` plus `run_task` for verification.

## Code style rules

- Use Australian English in prose, comments, logs, and errors; external protocol and library identifiers keep canonical spelling.
- Format through `golangci-lint run --fix ./...`, never a standalone `gofumpt`; install the pinned hooks after cloning.
- Use `log/slog`; wrap errors with context; put `context.Context` first on blocking/I/O calls; document each type's concurrency contract.
- Do real wiring in constructors, not `init()`. Add no globals beyond the documented TUI styles and intentional tool locks.
- Comments explain non-obvious why. Keep production files near 600 lines, tests near 900, and first-party non-test functions at cyclomatic complexity 15 or below.
- Every `//nolint` needs an inline reason. Re-review deliberately rejected linters before enabling one.

Detailed rules and exceptions live in [`docs/contributing.md`](docs/contributing.md#code-style-essentials); TUI code uses Bubble Tea/Lip Gloss/Bubbles v2 only.

## Testing requirements

- Keep tests beside code and table-driven where appropriate. `internal/lsp`, `internal/cache`, and `internal/tools` require meaningful coverage; write-tool tests use `WriteDeps{}` and session-isolation tests stay with the package they protect.
- Internal tool-to-tool `Execute` calls use canonical parameter names because aliases resolve only at the MCP dispatch boundary.
- Gate tests needing external binaries with `//go:build integration`; do not chase TUI coverage.
- Use `topology_affected` during the edit loop. Before delivery run `make verify`; run `make lint-cross` after platform-constrained or linter-config changes. `make cover` and `make vuln` remain separate CI/on-demand gates.
- `make test` puts `t.TempDir()` under repository `.testcache`; tests must not assume a system-temp ancestry.

## Delivery and risk

- `make install-hooks` is required after cloning; `make verify` defines ready to commit. `make help` and [`docs/contributing.md`](docs/contributing.md) own the full command matrix.
- Version resolution is exact tag → `VERSION` → short commit. After rebuilding, run `plumb restart`; source changes do not activate in an old daemon.
- Use conventional commit types (`feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`). Keep changes bisectable and add a `CHANGELOG.md` entry for each discrete change.
- Treat concurrency, the rate limiter, read tracking, and the stats schema as high-risk invariants; inspect their tests and architecture before editing.
- Keep this file as rules plus pointers. `make check-brief` enforces its line and byte budgets.
