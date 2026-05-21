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

### Plumb Topology: Phase 2

**Status (0.7.6): PARTIALLY COMPLETE.** Shipped and verified: Go+Python call edges (Tier 1), `topology_impact`/`topology_affected`/`topology_routes` tools (Tier 2 — registered, tested, documented), TypeScript/JavaScript extractor (Tier 3), `drain` per-path coalescing + `isStale` content-hash + periodic-resync ticker (Tier 4), and the DoD-7 concurrency stress test (passes under `-race`). **NOT done — moved back to `docs/todo.md` → "Topology — remaining Phase 2 gaps":** the DoD-6 ">=5x faster than LSP" performance claim (measured ~1.2x warm / ~0.1x cold; the integration test was reframed to an honest latency check rather than a false >=5x assertion), LSP fallback for `list_symbols`/`find_symbol`/`workspace_symbols` (item 5), and TUI + `plumb doctor` topology visibility (item 6). The plan text below is retained verbatim for reference.

**Context.**

Phase 1 delivered: SQLite/FTS5 schema, background indexer, Go AST extractor, Python regex extractor, `topology_status`, `topology_search`, `topology_explore`. Everything below was explicitly deferred and is known-good design — it just needs implementation.

---

**1. Finish the Phase 1 Definition of Done.**

Two DoD items from the original spec were not completed in Phase 1:

- **DoD-6 — Benchmark:** Topology-based symbol listing must be >5× faster than LSP-based `list_symbols` on cold start. Write a `//go:build integration` benchmark in `internal/topology/` that: starts gopls against a real Go workspace, measures `list_symbols` cold time, runs a full `Store.Resync()` + `Search("workspacePool")`, and records the ratio. The test must fail if the ratio is below 5×.
- **DoD-7 — Concurrency stress test:** Prove MCP read queries do not fail or deadlock while the indexer is writing. Write a `//go:build integration` test that: opens a `Store`, starts `Start()` (which enqueues an initial resync), simultaneously fires 100 `Search()` goroutines with `-race`, and asserts zero `SQLITE_BUSY` errors and zero data races.

Both belong in `internal/topology/benchmark_test.go` (or similar). They gate the claim that topology is safe to enable by default.

**Definition of done:** `go test -tags=integration -race ./internal/topology/... -bench=.` passes with the >5× ratio and zero races.

---

**2. Extractor: missing unit test files.**

The Go and Python extractor packages (`internal/topology/extractors/golang/`, `internal/topology/extractors/python/`) have no `_test.go` files. This was the most significant testing gap left by Phase 1. Both need fixture-based tests before Phase 2 extractors are added.

Go extractor — `extractor_test.go` must cover:
- Empty file → zero nodes, zero errors.
- Package node is always produced with `KindPackage` and the correct name.
- Import nodes are produced with `KindImport`; containment edges connect package → import.
- Functions: `KindFunction`, correct name, `StartLine`/`EndLine` populated, doc comment captured.
- Methods: `KindMethod`, receiver type reflected in `Qualified`.
- Types: `KindType` for `type Foo struct{}`, `type Bar interface{}`.
- Constants and variables: `KindConstant` / `KindVariable`.
- Test functions: `Test*`, `Bench*`, `Example*` produce `KindTest`.
- Malformed Go source (syntax error) → `Extract` returns `(nil, nil, nil)` — never an error.

Python extractor — `extractor_test.go` must cover:
- `class Foo:` → `KindClass` node.
- `def run(self):` at indent depth 1 inside a class → `KindMethod` with a containment edge to the class with `confidence=0.8`.
- `def standalone():` at indent depth 0 → `KindFunction`.
- `async def background():` → `KindFunction`.
- `def test_foo():` → `KindTest`.
- `import os` and `from pathlib import Path` → `KindImport` nodes.
- Empty file → zero nodes, zero errors.
- File with only comments → zero nodes, zero errors.

**Definition of done:** `go test ./internal/topology/extractors/...` covers all cases above; `golangci-lint run` stays at 0 issues.

---

**3. New tools: `topology_impact`, `topology_routes`, `topology_affected`.**

These complete the original five-tool suite. All three follow the `parseArgs → validate → run → format` pattern, stay under gocyclo 15, and degrade gracefully when the store is nil.

**`topology_impact`** — transitive dependency and reference closure.
- Input: `{"name": string, "depth": 3, "max_nodes": 100, "edge_kinds": ["imports","calls","references"]}`
- Traverses outward edges (what does this symbol depend on?) and inward edges (what depends on this symbol?) separately, returning two sub-graphs.
- Hard caps: depth ≤ 4, nodes ≤ 200, bytes ≤ 100 000 (same as `topology_explore`).
- Output: two sections "depends on" and "depended on by", with per-edge kind labels.
- Primary use: assess blast radius before a refactor.

**`topology_routes`** — framework-aware entry points.
- Input: `{"framework": string (optional), "path_prefix": string (optional), "limit": 20}`
- Phase 2 scope: scan `KindFunction` nodes whose names or signatures match common patterns:
  - Go: functions registered with `http.HandleFunc`, `r.GET`, `r.POST`, `mux.Handle`, Cobra `cmd.Run`/`cmd.RunE`.
  - Python: functions decorated with `@app.route`, `@router.get`, FastAPI path decorators.
- Output: list of entry points with path pattern, method, handler symbol, file/line, source (heuristic confidence).
- Confidence annotation is mandatory: these are pattern-matched, not type-resolved.
- Returns empty with a clear message when no patterns match — never silent zero results.

**`topology_affected`** — given changed files or symbols, return likely affected files and tests.
- Input: `{"files": []string, "symbols": []string, "max_results": 50}`
- Traverses inward edges from each named file/symbol to find dependents.
- Adds a second pass collecting `KindTest` nodes that transitively reference any affected node.
- Output: two sections "affected files" and "likely affected tests", each with confidence scores.
- Primary use: after writing, suggest which tests to run without a full `go test ./...`.

File locations: `internal/tools/topology_impact.go`, `topology_routes.go`, `topology_affected.go`, each with a `_test.go` covering nil-store degradation, basic traversal, and budget truncation.

---

**4. New extractors: TypeScript/JavaScript.**

Add `internal/topology/extractors/typescript/extractor.go`. JavaScript and TypeScript are the most common languages after Go and Python in mixed repos.

Phase 2 scope (no Tree-sitter, no CGo):
- Line/block regex scanner similar to the Python extractor.
- Extracts: `function`, `async function`, `const foo = () =>`, `class`, `interface`, `type Foo =`, `export default`, `import`/`require`, `export { }`.
- Framework patterns: Express `app.get/post/put/delete/use`, Next.js `export default function Page`, React component functions (PascalCase, returns JSX-like content).
- Test functions: `describe`/`it`/`test` from Jest/Mocha/Vitest — classify as `KindTest`.
- Confidence 0.7 on all heuristic edges (lower than Python's 0.8 because JS/TS is syntactically noisier without a grammar).
- Extensions: `.ts`, `.tsx`, `.js`, `.jsx`, `.mjs`, `.cjs`.

Add `extractor_test.go` with fixtures for: ES module import, CommonJS require, class with methods, arrow function component, Express route handler, Jest `describe`/`it` test.

Register in `topology_pool.go`'s `buildExtractors()`.

**Watch out for:** `.min.js` files, generated bundles, and `node_modules` must be excluded. Add `*.min.js`, `*.bundle.js`, and `dist/**` to the default `shouldSkipDir` / skip-file logic in the indexer.

---

**5. LSP fallback integration.**

When LSP is unavailable or still warming up, `list_symbols`, `find_symbol`, and `workspace_symbols` should transparently fall back to topology results, annotated with `source=topology` and `mode=indexed-approximate`.

Approach:
- Each tool checks whether its LSP call returns an error or times out.
- On fallback, calls `store.Search(ctx, query, SearchOpts{Kinds: []string{...}})` and formats results in the same shape as LSP output, with an extra `[topology fallback — index may be stale]` line prepended.
- The fallback is silent on success and explicit on degradation — agents can tell whether they are looking at authoritative LSP data or approximate indexed data.

This requires `connSession` to expose the topology store to the LSP tool constructors, or the tools to accept an optional `TopologyFallbackFn` in their constructor (consistent with `WriteDeps` pattern).

**Watch out for:** do not silently substitute topology for LSP when LSP is available and healthy. The substitution must only trigger on LSP error or timeout, not on "LSP is slow but will respond". Set a conservative timeout (e.g. 500 ms) for the LSP attempt before falling back.

---

**6. TUI and `plumb doctor` topology visibility.**

Users need to know the index is healthy without calling `topology_status` from an agent session.

TUI additions:
- In the Sessions section right panel, add a "Topology" row in the workspace detail block: `Topology: idle | 1,234 nodes | 4.2 MB | last sync 2 m ago`.
- If `IndexerState == "error"`, colour the row red and show the first line of `LastError`.

`plumb doctor` additions:
- New check `"topology"`: runs `Store.Status()` for the resolved workspace. Passes if `Enabled=false` (disabled is valid) or `Enabled=true` and `IndexedFiles > 0`. Fails if enabled but `TotalNodes == 0` after 30 s (indexer never completed). Detail line: `"Topology: disabled"` / `"Topology: 1,234 nodes, 4.2 MB, last sync 2 m ago"` / `"Topology: ERROR — <first line of LastError>"`.
- `plumb doctor --json` must include the topology check in its JSON output array.

---

**7. Periodic resync.**

Phase 1 resyncs only on daemon attach (`Start()` enqueues `opResync`). External file changes (git checkout, external editor, `make generate`) are not picked up until the next attach.

Add `ResyncIntervalMinutes` config support (the field already exists in `TopologyConfig` but is not wired):
- If `ResyncIntervalMinutes > 0`, the indexer's background worker fires a periodic `opResync` on a ticker.
- Default: 0 (disabled) — only on-attach resync. Periodic resync is opt-in.
- The indexer already coalesces, so a periodic resync enqueued while another is running is a no-op.

---

**8. Indexer: drain coalesces across different files (known trade-off).**

`drain()` discards all queued ops and returns only the last one. Within the same file this is correct (only the latest write matters). Across different files it is an unintentional drop — if three different files are written in rapid succession, only the last file's upsert is processed; the other two wait until the next write to those files or the next attach-time resync.

Fix: replace `drain` with a per-path coalescing map that keeps the last op per unique path, then processes all of them before returning:

```go
func (idx *Indexer) drain(initial indexOp) []indexOp {
    seen := map[string]indexOp{initial.path: initial}
    for {
        select {
        case op := <-idx.queue:
            seen[op.path] = op // last write per path wins
        default:
            ops := make([]indexOp, 0, len(seen))
            for _, op := range seen {
                ops = append(ops, op)
            }
            return ops
        }
    }
}
```

`backgroundWorker` then iterates the returned slice. Phase 2 item — not urgent because the on-attach resync recovers the index, and the current coalescing is correct for the common single-file write case.

**9. isStale: uses mtime only, not hash (known trade-off).**

`isStale` compares `mtime_ns` but ignores `content_hash` despite it being stored. A file restored from backup with an earlier mtime and different content won't be re-indexed until its mtime changes.

Fix: change the staleness check to `dbMtime != info.ModTime().UnixNano() || dbHash != hash`. This requires computing the hash before the staleness check (currently it's computed after). Restructure `processUpsert` to: `stat → read → hash → isStale(mtime || hash) → extract → persist`.

Phase 2 item — low impact for typical developer workflows where mtime is a reliable proxy for content changes.

---

**Definition of done — Phase 2.**

1. DoD-6 and DoD-7 benchmarks/stress tests pass under `-race` (integration tests, not yet written).
2. Extractor unit tests exist for Go and Python — `_test.go` files are present; remaining gap is async/def and confidence-value assertions in the Python extractor.
3. `topology_impact`, `topology_routes`, `topology_affected` implemented, tested, registered, and documented in `AGENTS.md` and `docs/mcp-tools.md`.
4. TypeScript/JavaScript extractor implemented and tested against fixtures.
5. LSP fallback wired for `list_symbols`, `find_symbol`, `workspace_symbols`; fallback is explicit in response.
6. TUI sessions panel shows topology state row; `plumb doctor` topology check passes.
7. Periodic resync wired when `resync_interval_minutes > 0`.
8. `drain` coalescing fixed to retain the last op per unique path across all queued files (todo item 8).
9. `isStale` extended to compare content hash in addition to mtime (todo item 9).
10. `make verify` stays green; golangci-lint 0 issues.

**Watch out for.**

- TypeScript regex extraction is noisier than Python. Keep confidence lower and test against real `.ts` files from a known project (e.g. plumb's own `package.json`-free test fixture) before shipping.
- The LSP fallback timeout must be tuned carefully. Too short and it triggers unnecessarily on busy CI machines; too long and it defeats the purpose.
- `topology_routes` framework patterns are heuristic and can silently miss routes or false-positive on similarly shaped functions. Keep the confidence annotation visible in every result and document the limitations clearly.
- Adding TypeScript to `buildExtractors()` means the initial resync on any mixed-language workspace will take longer. Surface indexing progress in `topology_status` so agents don't assume the index is complete when it's still running.
- Periodic resync interacts with write-triggered resyncs. Make sure the ticker does not enqueue while a resync is already running — check `state == "running"` before enqueuing the periodic op.

---

**Partially completed in:** 0.7.6 (Topology Phase 2)
**Original priority:** high

Shipped: Go+Python `EdgeCalls`, TypeScript/JavaScript extractor, `topology_impact`/`topology_affected`/`topology_routes` (registered, tested, documented in `AGENTS.md` + `docs/mcp-tools.md`), `drain` per-path coalescing, `isStale` content-hash check, periodic-resync ticker, and the DoD-7 concurrency stress test (passes under `-race`). `make verify` is green; unit + integration tests pass.

NOT shipped — tracked in `docs/todo.md` → "Topology — remaining Phase 2 gaps": DoD-6's ">=5x faster" claim is unmet (topology is ~1.2x warm, slower cold; `TestDoD6_TopologyQueryLatency` now records latency honestly instead of asserting a false gate); LSP fallback (item 5); TUI + `plumb doctor` topology visibility (item 6).
