# Plumb — Completed Work (Pending Review)

Items that appeared in `todo.md` at commit `3728b3ef` and are no longer in the current version because they were implemented. Each entry notes the commit(s) that completed it and what was actually shipped.

---

## Architecture

### `expected_sha` parameter on `edit_file` and `transaction_apply`

**Completed in:** 0.6.x (multiple commits)
**Original priority:** medium-high

`read_file` now returns a `sha256=<hex>` field in its header alongside `mtime`. `edit_file` and `transaction_apply` both accept an optional `expected_sha` parameter; if provided, the file's SHA-256 is verified before any edit is applied, with a clear rejection message showing the expected vs current hash. This is stronger than `expected_mtime` because it survives `touch -d`, restore-from-backup, and same-second coarse-mtime aliasing.

Implementation: `internal/tools/read_file.go` (hash computation + header field), `internal/tools/edit_file.go` (`ExpectedSha` field + pre-edit gate), `internal/tools/transaction.go` (same field on `txOperation`).

---

### Transaction durable rollback log

**Completed in:** commit `ed3d128` — `feat(tools): add durable rollback log to transaction_apply`
**Original priority:** medium-low

`transaction_apply` now writes a per-transaction WAL to `.plumb/tx-log/<txID>/` before phase-2 writes begin. Each pre-edit snapshot is written before its corresponding file is touched. On success, the log directory is removed. On failure, the daemon rolls back from the snapshot files. On next daemon startup, `txlog.Scan` finds orphaned log directories from a previous crash and replays their rollback.

Implementation: new package `internal/tools/txlog/` with `Begin`, `Record`, `Commit`, `Rollback`, and `Scan`. Tests in `internal/tools/txlog/`.

---

### Watcher glob: `{a,b}` alternation and absolute-path `/**/`

**Completed in:** commit `53da915` — `fix(watcher): support {a,b} alternation and absolute-path **/ globs`
**Original priority:** blocker for post-write diagnostics

`internal/lsp/watcher/watcher.go`'s `matchGlob` was rewritten to handle two LSP glob forms that `filepath.Match` cannot:

1. **`{a,b,c}` alternation** — `expandAlternation` splits `{go,mod,sum}` into individual alternatives and recurses.
2. **Absolute-path `/**/`** — splits the pattern on `/**/` and matches paths that start with the prefix and whose suffix matches the remainder.

gopls v0.22+ registers both forms after `client/registerCapability`. Before the fix all file-change events were silently filtered, causing post-write diagnostics to never arrive. Regression tests added to `watcher_test.go`.

---

## Features

### Automatic session orientation via the MCP `instructions` field

**Completed in:** commit `015f32b` (or nearby) — `feat: add doctor and agent safeguards`
**Original priority:** high

`internal/mcp/server.go` now populates the `instructions` field in the `initialize` response. The default text lives in `internal/mcp/instructions.go` as `DefaultInstructions` — it tells the model to call `session_start` as the first tool of every session, explains what it returns, covers the no-workspace branch, and nudges toward symbol-aware tools. The field is `omitempty` so clients that predate the spec change see no unexpected keys. Override with `ServerInfo.Instructions = "-"` to suppress.

---

### Token Usage Optimisation — Unified diff and smart truncation

**Completed in:** commit `31fe447` — `feat(tools): unified diff in edit/write responses, smart truncation for search and git`
**Original priority:** high

`edit_file` and `write_file` now return a unified diff of the change alongside the line-range summary, giving the agent immediate confirmation without a follow-up `read_file` turn. `search_in_files` and `git log` outputs are automatically capped and include a summary line ("Showing N of M matches") when truncated.

---

### `plumb doctor` — discovery and health-check CLI

**Completed in:** commits `015f32b`, `600ce76`, `b208fb9`, `a8ca361`, `5389138`, `db1f507`
**Original priority:** medium

`plumb doctor` is a traffic-light report showing: binary path + version, daemon running + version-match, gopls/pyright/jdtls/java on PATH, stats DB status, global and project config existence, and MCP client registration for Claude Desktop, Claude Code, Gemini CLI, and others. Exit code 0 on all ✓, 1 on any ✗. `--json` flag for machine-readable output. Implementation: `internal/cli/doctor.go`.

---

### "Working tree is dirty" guard on write tools

**Completed in:** commit `840d4d4` — `feat(tools): add dirty_ok guard to all write tools`
**Original priority:** medium

`write_file`, `edit_file`, `delete_file`, `rename_file`, and `transaction_apply` all accept a `dirty_ok bool` parameter (default `false`). When `false`, the tool checks `git status --porcelain` for the target file and refuses if uncommitted changes are present, with a message explaining how to proceed. `dirty_ok: true` bypasses the check.

Implementation: `pathIsDirty` helper in `internal/tools/file_write_helpers.go`; parameter wired into each tool's `args` struct and schema.

---

### TUI: Live Log Viewer

**Completed in:** commit `2656db6` — `feat(tui): add live log viewer tab (v0.6.3)`, refined in subsequent commits
**Original priority:** low

A full-width Logs tab (section index 3) tails `daemon.log` in real time. Log lines are displayed as they arrive; substring filtering (`logFilter`) narrows visible lines; `logFollow = true` (default) keeps the view pinned to the newest entry. Press `G` to re-engage follow after scrolling up, `esc` to clear the filter, type to filter.

Implementation: `internal/tui/log_view.go`, integrated into `internal/tui/model.go`.

---

## Testing & verification

### Claude Desktop end-to-end smoke test

**Completed in:** commit `b0d58c1` — `docs: complete Claude Desktop smoke test (0.5.30)`
**Original priority:** highest

A manual smoke checklist was run against real Claude Desktop and results captured. Verified: workspace resolution via `roots/list`, `/orient` prompt, `read_file` header, `edit_file` with mtime, post-write diagnostics after a syntax error, and MCP resources sidebar. Results recorded in `docs/claude-desktop-smoke.md`.

---

### Pyright integration smoke test

**Completed in:** commit `4a4981b` — `feat: session names, daemon_info, rename_session, config fields, pyright integration (0.5.30)`
**Original priority:** high

`TestIntegration_DidChangeWatchedFiles` exists in `internal/lsp/adapters/pyright/adapter_test.go`, gated `//go:build integration`. It spawns real `pyright-langserver --stdio`, initialises against a temp Python workspace, writes a syntactically broken `.py` file, sends `DidChangeWatchedFiles{FileCreated}`, and asserts pyright republishes at least one error diagnostic within 5 seconds. `testdata/python-fixture/` provides the minimum fixture. AGENTS.md marks pyright as "Validated".

---

### End-to-end MCP wire protocol smoke test

**Completed in:** commit `31bdc46` — `test(smoke): add end-to-end MCP wire protocol integration test`
**Original priority:** high

`cmd/smoke/smoke_test.go` is an automated replacement for the manual smoke checklist. It builds the plumb binary, spawns a daemon with an isolated HOME (short `/tmp/plsmk*` path to stay under macOS `sun_path` limit), and speaks MCP wire protocol over stdin/stdout via `plumb serve`. Steps: `session_start` → `read_file` → `edit_file` → `write_file` (new broken.go, FileCreated) → assert post-write diagnostics → `delete_file` → `list_memories`. Tears down via `plumb stop`. Run: `go test -tags=integration -timeout=3m ./cmd/smoke/`.

---

### CI matrix for integration tests

**Completed in:** commits `5e959b5` (Java), prior CI work
**Original priority:** high

CI now runs `go test -tags=integration ./...` with gopls, pyright, and Java 21 + jdtls installed. A `test-java` job installs Java 21, locates jdtls, and runs the jdtls integration test with a 10-minute timeout. A `make integration-test` target mirrors what CI runs.

---

## Bugs & known limitations (resolved)

### `pathLocks` unbounded memory growth

**Resolved in:** commit `ba82949` — `feat(tools): add LRU sweep to pathLocks — prevent unbounded memory growth`

An LRU sweep now runs every 5 minutes and evicts entries from the `sync.Map` that have been idle for more than an hour. Each entry tracks `lastUsed time.Time`; the sweep calls `TryLock` before deletion to avoid racing with in-flight locks.

---

### Rate limiter was per-connection, not per-agent

**Resolved in:** commit `8769fba` — `feat(tools): key rate limiter by client identity to prevent multi-connection bypass`

The rate limiter is now keyed by `clientName + clientVersion` at daemon scope (a shared `sync.Map[string]*RateLimiter]`), preventing a client from bypassing the per-minute write cap by opening multiple MCP connections simultaneously.

---

### CRLF normalisation limitation documented

**Resolved in:** docs update to `docs/mcp-tools.md`

The `edit_file` section now explicitly documents the mixed-line-ending limitation: "detection looks for the first CRLF in the file; files with mixed line endings have undefined matching behaviour — normalise with `dos2unix` or `unix2dos` before editing." This closes the documentation gap that implied CRLF tolerance was comprehensive.

---

### `client/registerCapability` globs now tracked

**Resolved in:** commit `218f76b` — `fix(lsp): track registerCapability globs; language-aware DidOpen`

Plumb no longer responds `null` to `client/registerCapability` and ignores the registered watchers. The watcher `Filter` now stores registered glob patterns by registration ID and `FilterEvents` passes only matching events. This was the root cause of post-write diagnostics never arriving with gopls v0.22+ (which registers `{a,b,c}` and absolute-path `/**/` patterns — also fixed in `53da915`).

---

### 100 ms concurrent-write skew constant made configurable

**Partially resolved:** constant renamed `defaultConcurrentWriteSkew`; `concurrentWriteDetected` now takes the skew as a parameter

The hard-coded `100ms` is now a named constant (`defaultConcurrentWriteSkew`) and the detection function accepts a `skew time.Duration` parameter (zero value falls back to the default). This makes the value injectable in tests and overridable by future config without touching the detection logic. A fully user-configurable version would pair with the `[edits]` config block; that extension is not yet implemented.

---

### Java adapter (jdtls) — write-tool `DidOpen`/`DidClose` and CI wiring

**Completed in:** commits `5e959b5`, `b905c03`, `218f76b`

Write tools (`write_file`, `edit_file`, `delete_file`, `rename_file`, `transaction_apply`) now send `textDocument/didOpen` + `textDocument/didClose` after the file-change notification when the workspace language is Java. Unlike gopls/pyright, jdtls requires the open-document lifecycle to trigger its reconcile pass and emit `publishDiagnostics` reliably after an external file write. CI jdtls integration step also wired.

Remaining gaps from the parent todo (cold-start tuning, binary naming docs, doctor version-check coverage for JDK distributions) are still open in `todo.md`.

---

## Code quality & engineering practices

### CQ-2 — Delete dead code

**Completed in:** 0.6.6
**Original priority:** P0 (quick win)

Removed all unused declarations and simplified vestigial signatures flagged by `golangci-lint unused`/`unparam`:

- `invProxy` struct + `Diagnostics` + `AllDiagnostics` methods deleted from `internal/cli/proxy.go`; `cache` import removed.
- `parseFrontmatter` wrapper deleted from `internal/memory/store.go` (callers already used `parseFrontmatterFull` directly).
- `spliceOverlayLower` deleted from `internal/tui/model_utils.go`.
- `splitFrontmatter` `delim` return removed — both callers discarded it; function now returns `(fm, body []byte)`.
- `defaultWriteRateLimit` `time.Duration` return removed — window is always `time.Minute`; caller now passes `time.Minute` directly.
- `setState` in `internal/lsp/supervisor.go` had `conn *jsonrpc.Conn` and `proc *exec.Cmd` always passed as nil; both parameters removed, body now sets both fields to nil explicitly.

Result: zero `unused`, zero `unparam` findings. All tests pass.

---

### CQ-1 — Mechanical lint cleanup (P0)

**Completed in:** 0.6.6
**Original priority:** P0 (foundational)

Cleared all non-gocyclo findings from 79 total down to 51 (only gocyclo 37 + gosec 14 remain, both deferred):

- **gofumpt/goimports**: `golangci-lint run --fix ./...` applied the embedded formatter to 10+ files that the standalone `gofumpt` binary (v0.10.0) would not flag, revealing a version mismatch. All formatting issues resolved.
- **ineffassign**: Dead `rootURI = "file://" + folder` assignment in `daemon.go` removed (value never read after reassignment). Intermediate `name := relPath` in `walk.go` removed (overwritten immediately or unused).
- **prealloc**: 5 slice preallocations added in `diff.go`, `edit_apply.go`, and `model_render.go` (×3).
- **revive** (stutter): `lsp.LSPClient` renamed to `lsp.Client` across 18 files. LSP semantic rename attempted first but failed due to stale position index (proxy.go had been edited); completed via `find_replace` on the qualified name + targeted edits for bare-name comments.
- **errcheck**: `os.MkdirAll` calls in `stats_test.go` wrapped with `t.Fatal`; `io.Copy` drain goroutine in `conn_test.go` uses `_, _ =`.
- **staticcheck**: `QF1008` embedded Duration field selectors simplified; `QF1001` De Morgan applied; `QF1003` if-else chain → tagged switch in `mcp/server.go`; `ST1005` trailing periods removed from 4 error strings in `edit_file.go` and `lsp_err.go`; `SA4010` dead `uris = append(uris, uri)` in `transaction.go` removed (confirmed not a rollback bug — per-file `notifyLSP` calls already handle LSP notification; the slice was never consumed).

Notes: Items 3, 4, 5 (make verify, pre-commit hook, CI enforcement) from the original CQ-1 definition of done are **not** completed here — those belong to CQ-6 and are tracked there.

---

### CQ-6 — Codify and enforce the engineering standard

**Completed in:** 0.6.9
**Original priority:** P2, anti-regression keystone

All four required items delivered:

1. **AGENTS.md: "Tool implementation pattern" subsection.** Documents the `parseArgs / validate / run / format` blueprint with a real before/after example for `FindFiles.Execute`. States that PRs adding a monolithic `Execute` are non-conforming.
2. **AGENTS.md: gocyclo-15 contract + file-size exception allowlist.** Explicitly states no first-party non-test function may exceed cyclomatic complexity 15. File-size rule (~400 lines) now has a short exception allowlist: `internal/lsp/protocol/types.go` (LSP spec type catalogue — splitting harms readability).
3. **Pre-commit hook updated** (`scripts/pre-commit`): now runs `go build ./...` then `golangci-lint run --fix ./...`. Dropped standalone `gofumpt -l -w .` call — standalone binary can disagree with the version embedded in golangci-lint, causing phantom diffs. `make install-hooks` documented as **required** after every fresh clone in AGENTS.md "Build commands".
4. **`make verify` target added** to `Makefile` (`build + test + lint`). Referenced as the canonical "ready to commit" definition.

Item 5 (CI file-size enforcement) was optional and not implemented.

---

### CQ-5 #1 — SQL string concatenation (G202) — verified not a real injection bug

**Completed in:** 0.6.9
**Original priority:** Highest-priority gosec item (potential real bug)

Triaged all three G202 findings in `internal/stats/db.go` (`Summary` at line 358, `ActivityAt` at line 429, `p95All` at line 500).

Verdict: **false positives.** In all three cases the concatenated `where` clause is built exclusively by `filter.where()`, which uses `?` placeholders for all user-supplied values (workspace, session ID, session name, tool name). The only things ever concatenated into the SQL string are fixed structural keywords (`GROUP BY tool ORDER BY calls DESC`, `ORDER BY called_at`, etc.) — no user data is ever interpolated.

Fix: extracted the static base query into a local variable and did the `where` concatenation on a single annotated line:
```go
// where is built by filter.where() using ? placeholders; no user values interpolated.
q := summaryBase + where + " GROUP BY tool ORDER BY calls DESC" //nolint:gosec // G202: see comment above
```

Remaining gosec findings (G306 file perms, G703 path traversal, G115 integer overflow, G602 slice index, G204 subprocess) are tracked in CQ-5 items 2–6 in `docs/todo.md`.

---

### CQ-3 — Decompose the monolithic `Execute()` methods

**Completed in:** 0.7.0 (non-TUI) and 0.7.1 (TUI)
**Original priority:** ⭐ P1

39 commits, one per decomposed function, each a pure behaviour-preserving refactor following the `parseArgs / validate / run / format` blueprint codified in CQ-6. Every first-party non-test function now passes gocyclo 15; `golangci-lint run --enable-only gocyclo ./...` returns zero violations.

Functions decomposed by version: **0.7.0** — `SearchInFiles.Execute` (74→≤15), `findReplaceTool.Execute` (58→≤15), `TransactionApply.Execute` (44→≤15), `handleConn` (38→1), `FindFiles.Execute` (35→≤15), `EditFile.Execute` (33→≤15), `(*Server).Serve` (32→6), `SessionStart.Execute` (31→≤15), `computeEditScript` (27→≤15), `(*WriteFile).Execute` (26→≤15), `applyEnv` (28→≤15), `runStats` (28→≤15), `Discover` (23→≤15), `runDiagOnWorkspace` (22→≤15), `ListFiles.Execute` (20→≤15), `ListDirectory.Execute` (20→≤15), `symbolKindName` (18→map), `ReadSymbol.Execute` (18→≤15), `executePartial` (18→≤15), `walkDir` (17→≤15), `runDaemon` (17→≤15), `RenameFile.Execute` (17→≤15), `runConfigShow` (17→≤15), `readContentMaybeRanged` (17→≤15), `groupHunks` (16→≤15), `(*CallHierarchy).Execute` (16→≤15), `(*RenameSymbol).Execute` (16→≤15), `(*TypeHierarchy).Execute` (16→≤15). **0.7.1** — `handleMainKey` (59→9), `updateInner` (41→14), `handleLogSectionKey` (31→14), `handlePopupKey` (25→15), `render` (24→12), `dashActivityGraphLines` (22→5), `renderPopup` (21→9), `popupRightAll` (18→13), `handleDashboardKey` (17→15), `handleMouseWheel` (17→14), `leftLines` (16→14).

---

## Bugs & known limitations

### `rename_symbol` stale LSP position index — clear error message

**Completed in:** 0.6.7
**Original priority:** medium

`rename_symbol` now detects "out of range" position errors from `applyWorkspaceEdit` and wraps them with a clear explanation:

> This usually means the LSP position index is stale after recent in-session edits. The language server computed edit positions against an older file version.

The error message includes three recovery options: calling `diagnostics` to confirm re-indexing, falling back to `find_replace` for the qualified name, or restarting the daemon.

Implementation: `internal/tools/rename_symbol.go` (`renameStaleIndexHint` constant + `strings.Contains` guard in `Execute`). Unit tests in `internal/tools/rename_symbol_test.go` cover the stale-index path and the empty-edit-set case.

The optional `textDocument/didOpen`/`didClose` flush (definition of fix item 2) was not implemented — the clear error message and recovery guidance are sufficient for practical use.

---

### `gofumpt` standalone vs `golangci-lint` embedded formatter mismatch documented

**Completed in:** 0.6.7
**Original priority:** low

Added an explicit note to AGENTS.md under "Build commands":

> `gofumpt -w` (standalone binary) may disagree with the `gofumpt` formatter embedded in `golangci-lint` v2.12.2 — the two can pin different versions. Always apply formatting via `golangci-lint run --fix ./...`, never via the standalone binary, to avoid phantom lint failures.

CQ-6's pre-commit hook item (ensuring the hook invokes `golangci-lint run --fix` rather than `gofumpt -l`) is tracked separately under CQ-6.

---

## Improvements

### Claude Desktop: plumb as the *only* tool surface

**Completed in:** 0.6.7
**Original priority:** high

Claude Desktop has no native filesystem or shell access. Plumb is its only interface to the codebase. Three concrete changes were made:

1. **`session_start` tool guidance block for Claude Desktop** (`internal/tools/session_start.go`): the `if isClaudeCode(...)` block was refactored to a `switch` with a new `case isClaudeDesktop(...)` branch. Claude Desktop receives a focused guidance block that lists all file-operation and LSP-semantic tools with the note "there is no fallback — if a plumb tool fails, retry or check `daemon_info`." Detection via `clientInfo.name == "claude-desktop"` (case-insensitive, prefix-tolerant).

2. **`delete_file` description** (`internal/tools/delete_file.go`): removed "use shell tools for recursive removal" — replaced with "to remove a directory tree, delete its files individually with repeated `delete_file` calls."

3. **`docs/mcp-tools.md` client capabilities table**: added a "Client capabilities and fallback behaviour" section at the top of the tool catalogue. A table lists Claude Desktop (no native filesystem/shell/git), Claude Code (`Read`/`Edit`/`Write` + `Bash`), Codex, and Gemini CLI. Two implication notes follow: one for tool error messages (do not suggest native tools), one for token savings (Claude Desktop savings are better expressed as "capabilities enabled" than "tokens saved vs alternative").

The savings-model profile for `claude-desktop` (Architecture item) was not implemented — that is part of the larger client-aware savings model tracked in the Architecture section.

---

### `edit_file` — opt-in partial apply mode

**Completed in:** 0.6.8
**Original priority:** low

`edit_file` now accepts `apply_partial: true`. When set, each edit in the `edits` array is applied independently in sequence. Failures are collected and reported per-edit rather than rolling back the entire batch. The response includes a per-edit result list with status (`applied` or `FAILED`), line range for successful edits, and the error message for failures. Post-write diagnostics are still appended at the end. If all edits fail, no file is written. LSP notification and cache invalidation only fire when at least one edit is applied.

Implementation: `internal/tools/edit_file.go` — new `apply_partial` schema field, `executePartial` method, `tryEditPartial` method, and `partialEditResult` struct. Tests in `internal/tools/edit_file_test.go` cover: all succeed, partial success (middle edit fails), all fail.

The atomicity guarantee is intentionally dropped when `apply_partial: true` is set — document clearly that this mode is incompatible with strict mode's "consistent state" assumption and is not valid inside `transaction_apply`.

---

### `search_in_files` — LSP-backed enclosing symbol for each match

**Completed in:** 0.6.8
**Original priority:** medium

`search_in_files` now accepts `include_enclosing_symbol: true`. When set and an LSP client is available, each actual match line (not context lines) is annotated with the deepest enclosing symbol from `textDocument/documentSymbol`:

```
internal/tools/transaction.go
  123:> uris = append(uris, uri)
  [in: Execute (method)]

1 hit(s) across 1 file(s).
```

One `DocumentSymbols` query per distinct matched file; results are re-used from the session's symbol cache when available (`symCache`). If the LSP is unavailable or the query fails, the annotation is silently omitted — the call never fails because of this feature.

Implementation: `internal/tools/search_in_files.go` — `include_enclosing_symbol` schema field and args struct field; `docSymbolsCached` method (LSP call with session cache); `deepestEnclosingSymbol` helper (recursive DFS, returns innermost symbol by range size); `fileMatch` struct extended with `absPath string` and `hitLineNums []int`; output loop injects `[in: Name (kind)]` line after each hit-line marker (`:> `). Constructor updated to accept `lsp.Client` and `*cache.Cache` alongside `WorkspaceFn`. Tests in `internal/tools/search_in_files_lsp_test.go`.

---

### `find_replace` — opt-in post-write formatter hook

**Completed in:** 0.6.8
**Original priority:** low

`find_replace` now accepts `format_after: true`. After writing all replacements (non-dry-run only), the appropriate source formatter is run on each modified file: `gofumpt` (falling back to `gofmt`) for `.go` files, `ruff format` (falling back to `black`) for `.py` files. If the formatter is not found the file is silently skipped. If the formatter errors, the failure is reported as a warning line in the response and does not fail the tool call. The response appends `formatted N file(s)` when any files were reformatted.

Implementation: `internal/tools/find_replace.go` — `format_after` schema field and args struct field; `runFormatterOnFiles` and `formatterCmd` package-level helpers; `fileChange` struct elevated from local to package scope to allow the helpers to reference it. Tests in `internal/tools/find_replace_test.go` cover: `.txt` file (no formatter → no "formatted" line) and `formatterCmd` extension dispatch.
