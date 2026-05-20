# Plumb — Completed Work (Pending Review)

Items that appeared in `todo.md` at commit `3728b3ef` and are no longer in the current version because they were implemented. Each entry notes the commit(s) that completed it and what was actually shipped.

---

## Architecture

### Plumb Topology: Persistent Semantic Indexing

**Completed in:** 0.7.5 — commits `7a40cc0`–`ef7cf0c` (config → schema → types → extractor interface → Go extractor → Python extractor → indexer → search → explore → store → topologyPool → wiring → MCP tools)
**Original priority:** ⭐ top architectural priority

Phase 1 ships a SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`, a daemon-owned background indexer, two language extractors (Go and Python), and three MCP tools (`topology_status`, `topology_search`, `topology_explore`). Disabled by default (`[topology] enabled = false`).

**What was shipped:**

- `internal/topology/` — `NodeKind`/`EdgeKind` constants, `Node`, `Edge`, `SearchResult`, `SearchOpts`, `ExploreOpts`, `Neighbourhood`, `Status` types; `Store` façade (`Open`, `Close`, `Enqueue`, `EnqueueDelete`, `Resync`, `Search`, `Explore`, `Status`); SQLite schema with WAL mode, `busy_timeout=5000`, foreign keys, FTS5 virtual table (`topology_fts`); background `Indexer` with coalescing queue (cap 256), mtime-based staleness, `safeExtract` panic guard, all-or-nothing per-file transaction; FTS5 BM25-ranked `Search` with kind/language filters and snippet extraction; BFS `Explore` with hard caps (depth ≤ 4, nodes ≤ 200, bytes ≤ 100 000) and `Truncated` flag; `FormatStatus` report with file count, entity count, DB size, indexed languages, last sync, last error.
- `internal/topology/extractors/golang/` — Go extractor using `go/parser`+`go/ast` (stdlib, no CGo). Extracts: package, imports (with containment edges), functions, methods (with signature and docstring), types, constants/variables, tests. Edge `FromID`/`ToID` use 0-based indices into the returned `[]Node` slice; the indexer remaps these to actual DB rowIDs.
- `internal/topology/extractors/python/` — Python extractor using line-by-line `bufio.Scanner` with regex. Extracts: classes, functions, async functions, methods (by indentation), imports, test functions (`test_`/`Test` prefix). Containment edges link methods to their class with `confidence=0.8`, `source="heuristic"`.
- `internal/cli/topology_pool.go` — `topologyPool`: daemon-level lazy per-workspace `Store` registry. `Acquire` is safe for concurrent callers; per-root mutex prevents double-open. `StopAll` called on daemon shutdown.
- `internal/config/config.go` — `TopologyConfig` struct added; `Topology` field added to `Config`; defaults: `Enabled=false`, `MaxFileSizeBytes=512*1024`; `ExcludePatterns` slice deep-copied in `cloneConfig`.
- `internal/cli/conn.go` — `topologyPool`/`topologyStore` fields on `connSession`; `startTopologyIndexer` (called after `startQualityRunner` in `attachWorkspace` and `attachSynthetic`); `buildWriteDeps` wires `TopologyNotify`; `registerAllTools` registers the three topology tools via a `topoFn` closure.
- `internal/cli/daemon.go` — `topoPool` created alongside `workspacePool`; `StopAll` deferred; `topoPool` threaded through `runDaemonAcceptLoop` → `handleConn` → `newConnSession`.
- `internal/tools/write_deps.go` — `TopologyNotifyFn` type alias; `TopologyNotify` field on `WriteDeps`; `notifyTopology` nil-safe helper.
- `internal/tools/write_file.go`, `edit_file.go`, `transaction.go` — call `notifyTopology(path)` after each successful write.
- `internal/tools/topology_status.go`, `topology_search.go`, `topology_explore.go` — three new MCP tools following `parseArgs → validate → run → format` pattern; nil-store graceful degradation.
- Tests: `internal/topology/db_test.go` (schema, WAL, FTS5), `internal/topology/search_test.go` (ranking, kind/language filters), `internal/tools/topology_*_test.go` (nil-store degradation), `internal/topology/explore_test.go` (BFS depth, truncation, clampOpts), `internal/topology/indexer_e2e_test.go` (end-to-end resync + FTS search), `internal/topology/extractors/golang/extractor_test.go`, `internal/topology/extractors/python/extractor_test.go`.

**Bugs found and fixed during review (commit `659adce`):**

- **Python extractor — blank lines reset class context.** Empty lines inside a class body have indent=0, which satisfied `indent <= classIndent (0)`, causing `classIdx` to reset to -1. Methods after any blank line were classified as functions. Fixed by skipping blank lines before the indent check.
- **`topology_explore` nil-store returned an error, not a message.** `topology_status` and `topology_search` return a human-readable "disabled" message when topology is off; `topology_explore` was inconsistently returning an error. Fixed to return a message via `formatTopologyNeighbourhood(nil, a)`.
- **`Indexer.Stop()` was non-blocking.** It closed the `done` channel but did not wait for the background goroutine to exit. When `Store.Close()` or test teardown called `db.Close()` immediately after `Stop()`, the goroutine was still mid-resync and received "database is closed". Fixed by adding `sync.WaitGroup` to `Indexer`; `Stop()` now blocks until the worker goroutine exits.
- **`search.go` N+1 queries.** `collectSearchResults` called `nodeByID` for each FTS result row. Fixed by joining `topology_nodes` in the FTS query (commit from prior review pass).
- **`matchField` always returned "name".** Since every node has a non-empty name, the field heuristic was always "name" regardless of which column matched. Fixed by checking query-term substring presence against each field in priority order (commit from prior review pass).

**Design decisions (intentional divergences from the original plan):**

- **FTS5 standalone table, not a content table.** The original plan described `content='topology_nodes'` with `content_rowid='id'` and SQL triggers to keep the FTS index in sync. The shipped implementation uses a plain FTS5 virtual table (`topology_fts`) with Go managing all inserts and deletes explicitly inside the per-file all-or-nothing transaction. Reasons: (1) no hidden trigger behaviour — the indexer controls exactly when and what is written; (2) no content-table sync hazards when rows are deleted or updated out of order; (3) simpler schema migration path. The rowid correspondence (`topology_fts.rowid = topology_nodes.id`) is maintained correctly by the indexer. This is not a bug or an omission — it is a deliberate simplification.

**What is NOT in Phase 1 (deferred to Phase 2):**

- `topology_impact`, `topology_routes`, `topology_affected` tools.
- TypeScript/JavaScript, Java, Rust, Ruby, Swift extractors.
- Tree-sitter integration (Phase 1 uses `go/parser`+`go/ast` and regex; Tree-sitter adds CGo).
- Topology-backed fallbacks to `list_symbols`, `find_symbol`, `workspace_symbols`.
- TUI/doctor topology health section.
- Formal benchmark (DoD item 6) and concurrency stress test (DoD item 7).
- `drain()` coalescing retains only the last enqueued op, discarding intermediate ops for different files (see Phase 2 item 8 in `docs/todo.md`).
- `isStale` uses mtime only; content-hash comparison deferred (see Phase 2 item 9 in `docs/todo.md`).

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

---

## Architecture

### Client-aware token-savings model

**Completed in:** 0.7.6
**Original priority:** High (stats credibility)

Replaced the static `altCost` table in `internal/stats/savings.go` with per-client profiles for `claude-desktop`, `claude-code`, `codex`, `gemini`, and `unknown`. Each profile assigns conservative per-tool fallback token estimates based on the client's native capabilities (e.g. Claude Code can run `rg`/`grep` directly, so `search_in_files` scores near-zero savings for it; Claude Desktop has no filesystem access, so the same tool scores higher).

**Key design decisions for reviewers:**

- **`normaliseClient(name string) string`** — single canonicalisation point. Lowercases and prefix-matches so `"claude-code 0.2.48"` or `"claude-code-ide"` all resolve to `clientClaudeCode`. All future client aliases must go here; tests cover the known aliases.
- **Codex profile = Claude Code profile** — both clients have full local file/shell access. Kept as separate map entries rather than aliased so the profiles can diverge if Codex adds or drops capabilities.
- **`TokensSaved` (no-client) preserved** for callers that predate client identity; internally routes to `clientUnknown` profile.
- **`HasSavingsModel(tool)` helper** — returns true when any profile has a non-zero estimate for a tool; used to suppress the savings widget for tools with universally zero estimates. Avoids adding UI conditionals at each call site.
- **Schema migration v5→6→7** — two separate `ALTER TABLE ADD COLUMN` steps, each idempotent via `hasColumn` check. Avoids the "partially applied migration" footgun where a crash between the two steps left the schema in an unrecoverable state.
- **`Call.ClientName` wired in `onAfterTool`** — captured under `stateMu` alongside the existing workspace/session fields. The guard ensures client identity is never read from a partially-initialised connection.
- **Phase 1 only** — shape modifiers (output byte scaling, per-call result counts), user-configurable `[savings.<client>]` config block, and calibration reports are all deferred to Phase 2/3 as originally planned. The numbers are conservative by design; they should understate rather than overstate.

Files changed: `internal/stats/savings.go` (rewritten), `internal/stats/db.go` (schema + Call struct + migrations), `internal/stats/db_query.go` (client-aware aggregation queries), `internal/cli/conn.go` (onAfterTool wiring), `internal/stats/savings_test.go` (new), `internal/stats/db_test.go` (schema columns + expected values updated).

---

### Code-quality differential after edits

**Completed in:** 0.7.6
**Original priority:** ⭐ Top architectural priority

Plumb write tools (`write_file`, `edit_file`, `transaction_apply`) now append a compact "code quality" section to their response when golangci-lint finds issues in the written file. This closes the loop from "write → compile-error feedback" to "write → compile-error + style/quality feedback", letting agents self-correct lint regressions in the same turn without waiting for CI.

**Key design decisions for reviewers:**

- **`internal/quality` package** — language-agnostic `Analyser` interface with `Name()`, `Supports(path)`, `Analyse(ctx, files)`. Adding a new analyser (ruff, eslint) requires only a new sub-package; the runner and config are unchanged.
- **`golangci-lint` subprocess, not library** — shells out to the binary so it picks up the workspace's checked-in `.golangci.yml` without plumb owning the config. The analyser silently returns `nil, nil` when `golangci-lint` is absent from PATH so writes always succeed.
- **`Runner` per MCP connection, not per workspace** — each connection has an isolated `*quality.Runner` (started in `attachWorkspace`/`attachSynthetic`, stopped in `close()`). Per-connection isolation matches the existing session model and avoids cross-session finding bleed.
- **Background mode default** — `golangci-lint` cold-start can exceed 30 s on large repos. Background mode enqueues the file and returns any already-cached findings immediately; the next write or tool call will surface fresh findings once the worker completes. Sync mode is opt-in via `[quality] mode = "sync"` for strict workflows where the response must include findings from the current write.
- **mtime-based cache invalidation** — `cachedResult.mtime` is the file's `ModTime()` at analysis time. `stale(path, cachedAt)` returns true when the file has been modified since analysis, ensuring agents always see findings for the current file state.
- **Queue coalescing (`drain`)** — the background worker reads the queue until empty before running the analyser. Rapid writes to the same file result in one lint run rather than N, bounding subprocess spawning.
- **`WriteDeps.QualityReport func(ctx, path) string`** — nil-safe; tools call `t.deps.reportQuality(ctx, path)` which is a no-op when the runner is disabled or absent. No write tool has a conditional dependency on quality.
- **`[quality] enabled = false` default** — opt-in until real-world performance on diverse repos is known. The daemon wires the runner only when `cfg.Quality.Enabled` is true; the zero value of `WriteDeps.QualityReport` silently produces empty strings.
- **Phase 1 only (Go / golangci-lint)** — Python/ruff analyser, `analysers.Composite` parallel runner, per-finding "explain why", `quality_ok: true` suppression param, and severity filtering are all deferred to Phase 2/3 as planned.

Files added: `internal/quality/quality.go`, `internal/quality/runner.go`, `internal/quality/golangcilint/analyser.go`, `internal/quality/runner_test.go`, `internal/quality/golangcilint/analyser_test.go`.

Files changed: `internal/config/config.go` (`QualityConfig` + `[quality]` defaults + validation), `internal/tools/write_deps.go` (`QualityReportFn` type + `QualityReport` field + `reportQuality` helper), `internal/tools/write_file.go`, `internal/tools/edit_file.go`, `internal/tools/transaction.go` (append quality report to response), `internal/cli/conn.go` (`qualityRunner` field + `startQualityRunner` + `buildAnalysers` + lifecycle wiring).

