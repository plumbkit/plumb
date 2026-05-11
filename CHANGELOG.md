# Changelog

## 0.3.1 — 2026-05-11

### Changed (architecture)
- **Per-project stats DB** — moved from a single global `~/.local/share/plumb/stats.db` to per-workspace `<workspace>/.plumb/stats.db`. Plumb is for the projects you're working on now, not a multi-project history archive. `plumb stats` defaults to the current directory's DB; `--workspace <path>` targets another project. Old global DB stays in place but is no longer written; safe to delete.
- **TUI hides unresolved sessions by default** — dormant Claude Desktop connections that never resolved a workspace no longer clutter the dashboard. Press `a` to show them; the footer reports the hidden count.
- **`plumb sessions` filters too** — pending sessions hidden unless `--all` is passed.

### Added
- **`plumb diag` alias** for `plumb diagnostics`.
- **`plumb diag` actually works** — without a file argument, walks every Go file in cwd and aggregates per-file diagnostics (gopls only emits for files it's seen via `didOpen`, so we explicitly request each one).
- **CLI diag prints session/workspace banner** so you know which daemon session produced the output.

### Fixed
- **Per-file diag distinguishes "clean" from "not tracked"** — a file gopls has analysed reports "clean"; an unanalysed file says "not yet tracked" with a hint to open it.
- **`find_references` sends `didOpen` first** — eliminates shifted line numbers when gopls hasn't seen the file via its in-memory view.
- **`workspace_symbols` and `find_symbol` post-filter dependency-cache hits** — drops symbols from `/pkg/mod/`, `/usr/local/go/src/`, etc. Workspace-only results.
- **`find_files` schema** — clarified that `*` matches everything (a literal `.` only matches a file named `.`).

## 0.3.0 — 2026-05-11

### Added
- **MCP resources** — memories are now exposed as MCP resources (`resources/list`, `resources/read`) so Claude Desktop's resources panel surfaces them as browseable markdown artifacts. URI scheme: `plumb-memory://<name>`.
- **Multi-language support** — pool now starts the right adapter (`gopls` for Go, `pyright` for Python) based on which root marker the workspace contains. Python is shipped disabled-by-default in config; enable via `~/.config/plumb/config.toml` and ensure `pyright-langserver` is on `$PATH`.
- **`search_memories` tool** — grep across all memories in a workspace (smart-case, regex, with file:line locators).
- **`relevant_memories` tool** — return memories whose frontmatter `paths:` globs match a given file. Memories can declare `paths: internal/auth/**` to auto-attach to relevant areas.
- **Tokens-saved metric** — each tool call's estimated savings are aggregated per tool and globally; shown in `plumb stats` (SAVED column) and the TUI footer.
- **Multi-workspace LSP routing (carried over from 0.2.2)** — each tool call routes to the gopls/pyright for the workspace containing its URI. Cross-project queries within one MCP connection just work.

### Changed
- Pool now keys on root and selects adapter by detected language; `acquireLang` is the new direct API.
- `findProjectRoot` superseded by `pool.Detect(start)` which returns `(root, language, error)`.
- TUI footer shows tokens-saved estimate alongside session and call counts.

### Internal
- `internal/memory/provider.go` — implements `mcp.ResourceProvider`.
- `internal/stats/savings.go` — per-tool alternative-cost multipliers and `TokensSaved()` helper.
- Routing proxy reuses the pool's multi-language detection.

## 0.2.2 — 2026-05-11

### Added
- **Multi-workspace LSP routing** — `routingProxy` / `routingInvProxy` dispatch each LSP call to the workspace containing its URI. Connection still has one "primary" workspace as fallback for URI-less methods.
- `pool.lookup(root)` — read-only entry lookup used by diagnostics routing without spinning up new gopls.
- `workspaceFromArgs(args)` — daemon helper that captures the per-call workspace for stats attribution.

### Changed
- Stats are now attributed to the call's own workspace, not the connection's primary.

## 0.2.1 — 2026-05-11

### Added
- **Edit tools** (read-only philosophy relaxed):
  - `rename_symbol` — LSP semantic rename
  - `replace_symbol_body` — replace a symbol's full declaration
  - `insert_before_symbol`, `insert_after_symbol` — add code around symbols
  - `safe_delete_symbol` — delete only if no references remain
  - `find_replace` — text/regex search-and-replace across files, dry-run by default
- **Onboarding** — `plumb init --discover` auto-detects build system, entry points, test layout, seeds `.plumb/context.md`.

### Internal
- `internal/tools/edit_apply.go` — `applyWorkspaceEdit`, `applyTextEditsToFile`, `findSymbolByPath`.

## 0.2.0 — 2026-05-11

### Added
- **Memory system** — `list_memories`, `read_memory`, `write_memory`, `delete_memory`. Persistent markdown notes at `<workspace>/.plumb/memories/<name>.md` with optional YAML frontmatter.
- **`plumb diagnostics [file]`** — debug LSP diagnostics from the terminal.
- **`version` MCP tool** — server identity for bug reports.
- Architecture doc updated with daemon, session registry, persistence layout, stats schema, memory store.

### Fixed
- TUI per-session stats filter by `session_id`, not workspace (fixes "all sessions show same stats").
- Sessions register immediately on connect (was: 8-minute lazy wait until first LSP call).
- Workspace resolves from `path` argument (filesystem tools now seed the workspace).
- `plumb stop` finds the daemon via three-stage lookup: PID file → `lsof` → `pgrep -f "plumb daemon"`.

### Internal
- `internal/memory/` package.
- `internal/cli/mcpclient.go` — reusable MCP client for future CLI commands.
- Stats `Filter` struct — query by workspace, session_id, or both.
