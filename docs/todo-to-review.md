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
