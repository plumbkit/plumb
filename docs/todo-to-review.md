# Plumb — Completed Work (Pending Review)

Completed items awaiting a review pass. Each notes the commit(s) that shipped it and what was actually delivered.

**Once an item has been reviewed, remove it from this file.** Its canonical record lives in `CHANGELOG.md` and the relevant docs (`docs/architecture.md`, `AGENTS.md`, `docs/mcp-tools.md`, …); any unfinished follow-ups belong in `docs/todo.md`. Keeping reviewed entries here would duplicate that documentation and blur what still needs review — so this file should only ever list work that has *not* yet been reviewed.

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
