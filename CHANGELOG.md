# Changelog

## 0.5.3 — 2026-05-11

### Fixed
- **Session adapter no longer hardcoded to `"gopls"`** — sessions now register with empty Language/Adapter and have both filled in by `startGopls` once the project's language is detected. Python workspaces correctly show `adapter: pyright` from now on; the TUI shows `(resolving workspace…)` until the language is known.
- Dead-code cleanup in `daemon.go`: removed unused `findGoModRoot`, `findProjectRoot`, `goModRootForDir` (all superseded by `pool.Detect`).
- Style modernizations: `errors.AsType[T]` instead of `errors.As`+addressing, `fmt.Appendf` instead of `[]byte(fmt.Sprintf(...))`, `wg.Go(fn)` instead of `wg.Add(1)`+goroutine+`wg.Done`. All compiled out identically; cleaner reading.

### Added
- **`stats.db` schema versioning** — `PRAGMA user_version` is stamped to `1` on every Open. Future schema changes will compare-then-migrate. `DB.CurrentSchemaVersion()` reads the on-disk value (0 for pre-0.5.3 databases).

## 0.5.2 — 2026-05-11

### Added
- **Per-project config overrides** — `<workspace>/.plumb/config.toml` is now loaded on top of the global `~/.config/plumb/config.toml` (XDG-respectful) once the daemon resolves a workspace. Only the fields the project file sets are overridden; the rest inherit from global. New `[edits]` section controls write-tool safety:
  ```toml
  [edits]
  strict = true                    # require read_file before edit_file
  rate_limit_per_minute = 30       # 0 disables; default 120
  ```
  Precedence: defaults → global config → project config → env vars (`PLUMB_STRICT_EDITS`, `PLUMB_WRITE_RATE_LIMIT`). Env vars remain the highest layer for emergency overrides without editing files.

### Changed
- `strict_mode` and the rate limit no longer require an environment variable. They read from the resolved config; env still works as the highest-precedence override.
- `RateLimiter` gained `SetLimit(int)` so the daemon can adjust the running limit when project config loads after session start.

## 0.5.1 — 2026-05-11

### Added (9/9)
- **`transaction_apply` tool — atomic multi-file edits** — applies str_replace edits across up to 50 files as a single transaction. Phase 1 validates every operation in memory; if any old_str is missing/ambiguous or any expected_mtime mismatches, NO files are written. Phase 2 writes each file via safeWrite under per-path locks acquired in lexical order (deadlock-safe); if a write fails partway, already-written files are rolled back to their pre-transaction content. Phase 3 fires `didChangeWatchedFiles` and invalidates the symbol cache per file. Each operation consumes one rate-limit slot. Use for refactors that must land as a unit (cross-file string rename, coordinated config + caller updates).

### Added (8/9)
- **TUI "Recent Edits" panel** — distinct section in the right panel showing the last 5 write-tool calls (`write_file`, `edit_file`, `delete_file`, `rename_file`) for the selected session. Surfaces "what did Claude touch?" without scanning the full recent-calls list.

### Added (7/9)
- **Per-session write rate limit** — `RateLimiter` (sliding window) gates `write_file` / `edit_file` / `delete_file` / `rename_file`. Default 120 writes per minute per session; configurable via `PLUMB_WRITE_RATE_LIMIT` (0 disables). Protects against runaway-loop scenarios in autonomous agents; transparent under normal use. New constructor parameter on the four write tools; the daemon installs one limiter per connection.

### Added (6/9)
- **Post-write diagnostics in `write_file` and `edit_file` output** — after a successful write, the tools snapshot the current diagnostics for the URI, then poll for up to 300ms looking for a change. Any new errors/warnings (up to 3 of each) are appended to the response so the agent learns immediately whether it broke the build, without a follow-up `diagnostics` call. Skipped when the diagnostics source is nil (test setups).

### Added (5/9)
- **Strict-mode mtime auto-tracking** — set `PLUMB_STRICT_EDITS=1` to require every `edit_file` target to have been read in this daemon's lifetime AND for the file's current mtime to match what was observed at read time. Catches the "agent edits without reading first" footgun and silent-overwrite scenarios when an external process modifies the file. Read mtimes are recorded in a process-global map; cross-session interference is possible but acceptable for an opt-in safety mode. Off by default.

### Added (4/9)
- **Watched-files unit tests for both adapters** — gopls and pyright now have explicit test coverage for `DidChangeWatchedFiles` wire transmission with `FileChanged` / `FileCreated` events. Catches regressions if the adapter wiring breaks. (A real-process integration smoke test against running pyright is not part of this release because pyright availability isn't guaranteed in all environments; the unit-level test confirms the LSP wire format is correct.)

### Added (3/9)
- **`session_start` roots/list fallback** — when the daemon hasn't yet resolved a workspace, `session_start` now queries the MCP client via `roots/list` to discover one before falling back to the cwd walk. New `RootsResolver` parameter on `NewSessionStart`; the daemon constructs one that uses the captured `RequestFn` from `OnInit`/`OnRootsChanged`. Properly fixes Claude Desktop cold-start where the daemon launches from `$HOME` and the cwd walk can't find the project.

### Added (2/9)
- **Direct cache invalidation on writes** — `write_file`, `edit_file`, `delete_file`, and `rename_file` now evict matching entries from the symbol cache (`cache.InvalidateByPath`) immediately after a successful write. Closes the stale-symbol window without depending on gopls's diagnostic-republish timing. Constructors gained an optional `*cache.Cache` parameter (`nil` is safe for tests).

### Added (1/9)
- **LSP server-request handling** — `jsonrpc.Conn` now dispatches server-initiated requests through a registered `RequestHandler`, sending back JSON-RPC responses (or `-32601 method not found` if no handler is registered). Closes the gap where `client/registerCapability` requests from gopls were silently dropped.
- **`workspace.didChangeWatchedFiles.dynamicRegistration` advertised** — `DefaultClientCapabilities` now declares this capability so gopls actually registers its file-watcher globs and consumes our `workspace/didChangeWatchedFiles` notifications. Makes the 0.5.0 LSP rewrite load-bearing.
- **gopls + pyright accept `client/registerCapability`** — both adapters register a default request handler that responds OK to `client/registerCapability` / `client/unregisterCapability` and method-not-found to anything else. Unregistered methods are answered politely so the server can decide what to do.

## 0.5.0 — 2026-05-11

### Changed (architectural)
- **LSP notification primitive rewritten** — write tools now use `workspace/didChangeWatchedFiles` (one notification per write) instead of the prior `didOpen`/`didChange`/`didClose` lifecycle dance. The new primitive is the LSP-correct way to tell a server about external file changes: no buffer-ownership claim, no version counter abuse, no language-ID guessing. `langIDForPath` (~60 lines) deleted. Plumb now declares `FileCreated`, `FileChanged`, or `FileDeleted` explicitly per event. Protocol types, gopls + pyright adapter methods, routing-proxy fanout, and test mocks added across the LSP layer.

### Added
- **`delete_file` tool** — atomic file delete with `FileDeleted` notification.
- **`rename_file` tool** — atomic move/rename with paired `FileDeleted` + `FileCreated` notifications. Deadlock-safe two-path locking. Distinct from `rename_symbol` (LSP-semantic identifier rename).
- **Per-path lock** — process-global `sync.Map[path]→Mutex` serialises concurrent `write_file` / `edit_file` / `delete_file` / `rename_file` calls to the same path. Two parallel sessions can no longer interleave reads and writes on the same file.
- **`expected_mtime` on `edit_file`** — optional RFC3339Nano timestamp (typically copied from `read_file`'s output header). When supplied, the edit is rejected if the file's current mtime differs — optimistic-concurrency guarantee that the agent is editing the same revision it read.
- **mtime header on `read_file` output** — every response begins with `# plumb-read mtime=<RFC3339Nano>` so the agent can thread it back as `expected_mtime`.
- **Line-change summary in `edit_file` output** — response now includes the new mtime and a compact `lines changed: L12-15, L45` summary so the agent can verify changes without a follow-up read.
- **CRLF tolerance in `edit_file`** — line endings in `old_str` are normalised against the file before matching, so an LF `old_str` matches a CRLF file (and vice versa).
- **Symlink-aware `safeWrite`** — writes to a symlinked path follow the link to the real target instead of replacing the symlink with a regular file.
- **Pre-rename mtime check in `edit_file`** — if the file changes between our read and our rename, the attempt is surfaced as retryable rather than silently overwriting.
- **Daemon version-mismatch warning** — the daemon publishes its build version to `<runtime>/plumb.version` on start. `plumb serve` reads it and prints a stderr warning if the connected daemon's version differs from the binary that's launching ("run `plumb stop` to refresh").
- **`session_start` enhancements** — now includes current git branch, 3 most recent commits, 5 most recently-modified files (workspace-relative, skipping `.git`/`node_modules`/`vendor`/etc.). `context.md` cap raised from 80 → 200 lines. Cold-start fallback walks up from `os.Getwd()` for project markers when the daemon hasn't resolved a workspace.
- **Glob filter on `list_directory`** — optional `pattern` parameter (e.g. `*.go`) for consistency with `list_files`.

### Fixed
- **`list_directory` modified times** — the column was collected but never rendered. Now visible alongside name and size.
- **`read_file` streaming line ranges** — when `start_line`/`end_line` are set, `bufio.Scanner` stops at `end_line` instead of reading the whole file into memory and slicing.
- **`read_file` binary detection** — refactored to use `io.MultiReader` instead of read-seek-reread (works on pipes/devices that don't support seeking).
- **`read_multiple_files` parallelism** — now reads up to 8 files concurrently and propagates the parent context. Previously serial and passed `nil` ctx to the inner reader.
- **`write_file` schema cleanup** — removed the dead-code empty-content double-unmarshal hack; `content` is schema-required and enforced by the MCP layer.

### Known gaps
- `client/registerCapability` is silently dropped by the jsonrpc `Conn` (no server-request handler). Functionally OK for gopls — it has built-in static watchers for `.go` files and consumes our notifications via that path — but full LSP correctness needs the conn-level request handler plus `dynamicRegistration` declared in client capabilities. Slated for 0.5.1.
- Symbol cache (`internal/cache`) is invalidated only indirectly via the existing `Invalidator` that listens for `publishDiagnostics`. After a write, the cache stays stale until gopls re-publishes. Direct eviction from the write tools is the right fix; deferred to 0.5.1.
- Pyright `didChangeWatchedFiles` is wired but untested in this release.

## 0.4.1 — 2026-05-11

### Added
- **`write_file` tool** — creates or overwrites a file atomically. Content is staged in a temp file in `os.TempDir()` (no project-tree noise) then renamed into place. If the system temp dir and target are on different filesystems (EXDEV), falls back to a `.plumb.tmp` sibling automatically. Preserves existing file permissions. Notifies the LSP server via `didOpen`/`didChange`/`didClose` after writing so diagnostics and symbol lookups reflect the new content immediately.
- **`edit_file` tool** — applies one or more str_replace edits to an existing file with a four-layer safety model: (1) uniqueness lock — each `old_str` must appear exactly once, rejecting absent or ambiguous matches cleanly; (2) in-memory application — all edits applied before any write, file untouched on any failure; (3) atomic write — staged in `os.TempDir()` + rename, EXDEV-fallback to sibling; (4) concurrent-write retry — after the rename, plumb re-stats the file and retries the edit up to 3 times if a third-party write is detected. LSP notification on success.
- **`read_multiple_files` tool** — reads up to 20 files in a single call. Per-file errors reported inline.
- **`list_directory` tool** — immediate directory contents with `[FILE]`/`[DIR]` type prefixes, file sizes, and modification times. Sortable by name, size, or modification time.
- **`DaemonVersion` in session info** — `session.Info` now carries the daemon's version string. The TUI right panel shows it as a `Daemon` row so sessions from different daemon versions are distinguishable.

### Changed
- File write safety model revised: temp files go to `os.TempDir()` rather than a sibling `.plumb.tmp`, keeping the project tree clean. EXDEV cross-device rename is handled transparently by falling back to the sibling approach only when needed.
- `file_write_helpers.go` centralises `safeWrite`, `notifyLSP`, and `langIDForPath`. `safeWrite` returns a `writeResult` carrying timestamps used for concurrent-write detection in `edit_file`.

## 0.4.0 — 2026-05-11

### Added
- **`read_file` tool** — reads the text contents of any file by absolute path or `file://` URI. Supports `start_line`/`end_line` for slicing large files without loading them entirely. Binary files are detected and rejected. Output is capped at 200 KiB with guidance to use line ranges for larger files. Fixes the hard ceiling Claude Desktop users hit when navigating to a file but being unable to open it.
- **`session_start` tool** — bootstrap tool designed to be called first in every session. Returns in one round-trip: workspace path and auto-detected language, first 80 lines of `.plumb/context.md`, all memory names and descriptions, top-5 most-used tools from session history, and active LSP errors and warnings. Eliminates the "starts blind" problem on Claude Desktop where no filesystem access is available without tools.
- **`context.md` as MCP resource** — `.plumb/context.md` is now exposed as `plumb://workspace/context` via the MCP resource system, appearing as the first entry in the resources panel (above memories). Claude Desktop users can attach project context with one click.
- **MCP Prompts** — three named workflows surfaced as buttons/menu items in Claude Desktop:
  - **`orient`** — calls `session_start` and delivers a structured 4-point project summary.
  - **`whats-broken`** — chains `session_start` → `diagnostics` → `read_file` per broken file → triage and suggested fixes.
  - **`recent-changes`** — chains `session_start` → `git log` → `git diff --stat` → `diagnostics` for a recent-activity summary.
  All three accept an optional `workspace` argument; `recent-changes` also accepts `since` (e.g. `'1 week ago'`, a commit SHA).

## 0.3.3 — 2026-05-11

### Fixed
- **`plumb stop` now stops all daemon instances** — previously only one process was killed per invocation; users had to run `plumb stop` multiple times if multiple daemon processes were running. The command now collects all PIDs from all three lookup strategies (PID file, lsof, pgrep) and stops each one. Output is more verbose: prints each PID being stopped and reports upfront when multiple daemons are found.
- **Workspace not resolving from filesystem tool calls** — `list_files`, `find_files`, and `search_in_files` calls were not triggering workspace resolution, leaving sessions stuck in "pending" state in the TUI. Two causes: (1) `list_files` passes its directory as `root` but `OnBeforeTool` only read `path`; (2) when a tool passes a directory path, `filepath.Dir` was stripping the last component and causing `Detect` to start from the parent, missing the project root marker. Both fixed.

## 0.3.2 — 2026-05-11

### Added
- **`include_doc_comment` flag on symbol edits** — `insert_before_symbol`, `replace_symbol_body`, and `safe_delete_symbol` now accept an optional `include_doc_comment` boolean. When true, the operation extends to cover any contiguous comment lines (`//`, `#`, `/*`, `*`) directly above the symbol declaration. Lets you replace a function together with its doc comment, delete a function without orphaning its comment, or insert a new declaration above an existing doc comment instead of between the comment and its symbol. Default is false (backwards-compatible).
- **Stats `RecentCall` now surfaces `ErrorMsg`, `InputBytes`, `OutputBytes`** — the columns were already stored on every call but the read path discarded them. `plumb stats` recent-calls table now prints failed-call error messages on a continuation line below the row, and the TUI uses the same data for inline error expansion.
- **TUI keyboard navigation of recent calls** — press `tab` to move focus to the right panel's recent-calls list; `j/k`/`↑↓` then scrolls within it. The selected row is marked with `▸`, and for failed calls the error message expands inline below the row (wrapped to panel width). `tab` again returns focus to the sessions list. Footer hint updated.

### Changed
- **`find_symbol` is now single-file only** — the `uri` parameter is required. Previously, calling without `uri` ran a workspace-wide search that was a byte-identical duplicate of `workspace_symbols` (same LSP call, same cache key, same output format). After the split, `find_symbol` and `workspace_symbols` have clearly distinct purposes: file-scoped (case-insensitive substring against the document symbol tree) vs. workspace-scoped (LSP `workspace/symbol`, fuzziness depends on the language server).

### Fixed
- **TUI "No calls recorded yet" stuck on a session that does have calls** — `dbFor()` was caching the `nil` returned by `OpenReadOnly` when the per-project `<workspace>/.plumb/stats.db` didn't yet exist. Once cached, subsequent polls returned that nil even after the daemon created the file. The cache now only stores non-nil handles, so the TUI picks up writes as soon as they begin.

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
