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

---

## Planning

### Plumb Topology — Implementation Plan (Phase 1)

**Status:** Planning (not yet implemented). Written 2026-05-21 against v0.7.5.
**Source:** `docs/todo.md` → Architecture → "Plumb Topology: Persistent Semantic Indexing"

---

#### 0. Scope

Covers **Phase 1** as defined in `docs/todo.md`: the SQLite/FTS5 schema, daemon-owned incremental indexer, Go and Python extractors, and three MCP tools (`topology_status`, `topology_search`, `topology_explore`). Phase 2 items (`topology_impact`, `topology_routes`, `topology_affected`, TypeScript extractor, LSP fallback) are not in scope here.

---

#### 1. Key Architectural Decisions

**1.1 Tree-sitter vs. native extractors**

The todo.md specifies "Go-native Tree-sitter integration." The official Go bindings (`github.com/tree-sitter/go-tree-sitter`) and the commonly used `github.com/smacker/go-tree-sitter` both require **CGo**. Adding CGo has non-trivial costs:

- The current binary is pure-Go (`modernc.org/sqlite` was specifically chosen to avoid a system SQLite dependency). Adding CGo breaks that.
- CI would need a C toolchain step.
- Cross-compilation becomes harder.
- Grammars must be vendored as C source and compiled.

**Recommendation for Phase 1:** Use native Go parsers for Phase 1 extractors.

| Language | Parser | Why |
|---|---|---|
| Go | `go/parser` + `go/ast` (stdlib) | Authoritative; used by gopls; no new deps; type-system-aware |
| Python | Regex heuristics | Matches `def`/`class`/`import`/`from`; sufficient for symbol discovery |

The Extractor interface is designed to accommodate a future Tree-sitter-backed implementation in Phase 2 without change. If the decision is to use Tree-sitter from the start, the project must accept CGo and the attendant build complexity — that is a valid choice but must be made deliberately before implementation begins.

**1.2 Per-workspace, not per-connection**

The `quality.Runner` is per-connection. Topology is fundamentally different: it is a persistent on-disk index shared across all connections to the same workspace. This follows the `workspacePool` pattern — the daemon owns one indexer per workspace root.

A new `topologyPool` type in `internal/cli/` manages one `*topology.Store` (indexer + SQLite) per workspace root, keyed by absolute path. The first connection to attach to a workspace starts the indexer; the daemon-level pool holds the reference for its lifetime. This mirrors how `workspacePool` manages one `gopls` process per workspace.

**1.3 Separate databases**

`topology.db` is **derived and rebuildable** — it can be dropped and recreated if the schema changes or the index is stale. It must never live in `stats.db` (global, durable).

Location: `<workspace>/.plumb/topology.db`. Project-scoped, gitignored, isolated per-workspace.

**1.4 WAL and read/write separation**

SQLite WAL mode (as in `stats.db`) is mandatory. MCP tool read queries must never block on the background indexer's write transactions. Short write transactions per file (not per full resync). `PRAGMA busy_timeout = 5000` on every connection open.

---

#### 2. New Dependencies

None for Phase 1. `go/parser`, `go/ast`, `go/token` are stdlib. No new entries in `go.mod`. This is one of the few features that can be shipped with zero `go.mod` changes.

`github.com/tree-sitter/go-tree-sitter` (CGo) is deferred to Phase 2.

---

#### 3. File and Package Structure

```
internal/topology/
  topology.go                 -- package doc, public types (Node, Edge, Status, SearchResult, etc.)
  db.go                       -- Open(), schema, WAL, FTS5 creation, close
  indexer.go                  -- Indexer struct: background worker, debounce, file-change queue
  extractor.go                -- Extractor interface + registry + dispatch
  extractors/
    go/
      extractor.go            -- Go stdlib AST-based extractor
    python/
      extractor.go            -- Python regex-based extractor
  search.go                   -- FTS5 query helpers for topology_search
  explore.go                  -- neighbourhood traversal for topology_explore
  status.go                   -- StatusReport(), DB size, file counts
  store.go                    -- Store: wraps db + indexer, public API surface

internal/tools/
  topology_status.go
  topology_search.go
  topology_explore.go
  topology_status_test.go
  topology_search_test.go
  topology_explore_test.go

internal/cli/
  topology_pool.go            -- topologyPool: daemon-level per-workspace Store registry
  (conn.go modified)          -- startTopologyIndexer() added to attachWorkspace/attachSynthetic
  (daemon.go modified)        -- topologyPool field + tool registration
```

Projected file sizes (all within the 600-line limit):

| File | Estimated lines |
|---|---|
| `topology.go` | 80–120 |
| `db.go` | 200–280 |
| `indexer.go` | 200–260 |
| `extractor.go` | 80–100 |
| `extractors/go/extractor.go` | 200–260 |
| `extractors/python/extractor.go` | 150–180 |
| `search.go` | 150–200 |
| `explore.go` | 180–220 |
| `status.go` | 100–130 |
| `store.go` | 120–160 |
| Three MCP tools + tests | 150–200 each |
| `topology_pool.go` | 100–140 |

---

#### 4. SQLite Schema

```sql
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
PRAGMA user_version = 1;

CREATE TABLE IF NOT EXISTS topology_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS topology_files (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    path             TEXT    NOT NULL UNIQUE,   -- workspace-relative
    language         TEXT    NOT NULL DEFAULT '',
    mtime_ns         INTEGER NOT NULL DEFAULT 0,
    content_hash     TEXT    NOT NULL DEFAULT '', -- sha256 hex
    extractor_ver    TEXT    NOT NULL DEFAULT '',
    indexed_at       INTEGER NOT NULL DEFAULT 0,  -- unix epoch ns
    error_msg        TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tf_path ON topology_files(path);

CREATE TABLE IF NOT EXISTS topology_nodes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id    INTEGER NOT NULL REFERENCES topology_files(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,  -- file, package, function, method, type, constant, variable, import, class, test
    name       TEXT    NOT NULL,
    qualified  TEXT    NOT NULL DEFAULT '',
    signature  TEXT    NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line   INTEGER NOT NULL DEFAULT 0,
    docstring  TEXT    NOT NULL DEFAULT '',
    language   TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tn_file ON topology_nodes(file_id);
CREATE INDEX IF NOT EXISTS idx_tn_kind ON topology_nodes(kind);
CREATE INDEX IF NOT EXISTS idx_tn_name ON topology_nodes(name);
CREATE INDEX IF NOT EXISTS idx_tn_qual ON topology_nodes(qualified);

CREATE TABLE IF NOT EXISTS topology_edges (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id    INTEGER NOT NULL REFERENCES topology_nodes(id) ON DELETE CASCADE,
    to_id      INTEGER NOT NULL REFERENCES topology_nodes(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL, -- calls, imports, references, defines, contains, inherits, implements
    confidence REAL    NOT NULL DEFAULT 1.0,
    source     TEXT    NOT NULL DEFAULT 'extractor'
);
CREATE INDEX IF NOT EXISTS idx_te_from ON topology_edges(from_id);
CREATE INDEX IF NOT EXISTS idx_te_to   ON topology_edges(to_id);
CREATE INDEX IF NOT EXISTS idx_te_kind ON topology_edges(kind);

-- FTS5 virtual table. name_tokens holds the camelCase/snake_case split form.
CREATE VIRTUAL TABLE IF NOT EXISTS topology_fts USING fts5(
    name,
    name_tokens,
    qualified,
    signature,
    docstring,
    path,
    kind,
    content='topology_nodes',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

-- Sync triggers on topology_nodes keep topology_fts current.
CREATE TRIGGER IF NOT EXISTS topology_nodes_ai AFTER INSERT ON topology_nodes ...
CREATE TRIGGER IF NOT EXISTS topology_nodes_ad AFTER DELETE ON topology_nodes ...
CREATE TRIGGER IF NOT EXISTS topology_nodes_au AFTER UPDATE ON topology_nodes ...
```

**Code-aware tokenisation** is handled at insert time by a `splitIdentifier(s string) string` helper: `workspacePool → workspace pool`, `snake_case → snake case`, `kebab-case → kebab case`. The `name_tokens` column stores the split form. FTS5 queries match either column. Triggers keep both in sync automatically.

---

#### 5. Public Types (`topology.go`)

```go
type NodeKind string
const (
    KindFile, KindPackage, KindFunction, KindMethod, KindType,
    KindConstant, KindVariable, KindImport, KindClass, KindTest NodeKind
)

type EdgeKind string
const (
    EdgeCalls, EdgeImports, EdgeContains, EdgeDefines,
    EdgeInherits, EdgeImplements EdgeKind
)

type Node struct {
    ID, FileID             int64
    Kind                   NodeKind
    Name, Qualified        string
    Signature, Docstring   string
    StartLine, EndLine     int
    Language, Path         string
}

type Edge struct {
    ID, FromID, ToID int64
    Kind             EdgeKind
    Confidence       float64
    Source           string
}

type SearchResult struct {
    Node    Node
    Score   float64 // FTS5 bm25 rank
    Field   string  // matched field name
    Snippet string  // FTS5 snippet
}

type Neighbourhood struct {
    Centre    Node
    Nodes     []Node
    Edges     []Edge
    Truncated bool
}

type Status struct {
    IndexedFiles, SkippedFiles, StaleFiles int
    TotalNodes, TotalEdges                 int
    DBSizeBytes                            int64
    LastSync                               time.Time
    IndexerState                           string // idle | running | error
    Languages                              []string
    LastError                              string
}
```

---

#### 6. Extractor Interface

```go
type Extractor interface {
    Language()   string
    Extensions() []string
    // Extract parses src (file content at workspace-relative path).
    // Returns (nil, nil) for files it cannot handle — never an error for normal skips.
    Extract(ctx context.Context, path string, src []byte) ([]Node, []Edge, error)
}
```

**Go extractor** uses `go/parser` with `ParseComments | SkipObjectResolution`. Extracts: package, imports, functions/methods (name, signature, doc, line range), types, constants/variables, tests (`Test*`, `Bench*`, `Example*`). Contains edges: file→package, package→symbol, type→method. No call-edge extraction in Phase 1 (requires go/types; too expensive for background indexing).

**Python extractor** uses regex line-by-line scan. Extracts: `class`, `def`/`async def`, imports, docstrings (first triple-quoted string). Method vs. function distinguished by indentation depth. Containment edges by indentation. Confidence `0.8` for heuristically inferred containment.

Both extractors wrap `Extract()` call sites in `defer recover()` inside the indexer — a malformed file must never panic the daemon.

---

#### 7. Background Indexer (`indexer.go`)

Modelled on `quality.Runner`:

```go
type Indexer struct {
    workspace  string
    db         *sql.DB
    extractors []Extractor
    queue      chan indexOp   // capacity 256
    done       chan struct{}
    mu         sync.RWMutex
    state      string        // "idle" | "running" | "error"
    lastSync   time.Time
    lastErr    string
}

type indexOp struct { kind opKind; path string }
// opKind: opUpsert | opDelete | opResync
```

**Worker loop:** wait on queue or done → `drain()` to coalesce rapid writes → dispatch:
- `opUpsert`: stat file, compare mtime+hash vs `topology_files`, skip if unchanged, extract, replace file's nodes/edges in one short transaction (DELETE old + INSERT new).
- `opDelete`: delete from `topology_files` (CASCADE removes nodes, edges, FTS5 via triggers).
- `opResync`: full workspace walk, enqueue `opUpsert` per source file, then delete rows for files that no longer exist.

**File-change detection (Phase 1):** mtime+hash comparison at upsert time. Write tools call `Enqueue(path, opUpsert)` via `WriteDeps.TopologyNotify func(path string)` (nil-safe, added alongside `QualityReport`). Covers all daemon-driven changes. For external changes, the initial resync on attach covers the case; periodic resync is Phase 2.

**One indexer per workspace:** enforced by `topologyPool`. Late-attaching connections get the existing shared instance.

---

#### 8. Config Section

New `TopologyConfig` added to `internal/config/config.go` and `Config`:

```go
type TopologyConfig struct {
    Enabled               bool     `toml:"enabled"`
    ResyncOnAttach        bool     `toml:"resync_on_attach"`
    ExcludePatterns       []string `toml:"exclude_patterns"`
    MaxFileSizeBytes      int64    `toml:"max_file_size_bytes"`
    ResyncIntervalMinutes int      `toml:"resync_interval_minutes"`
}
```

Default: `Enabled = false`, `MaxFileSizeBytes = 512*1024`. Opt-in default matches `quality.Enabled = false` — Phase 1 is not production-default until performance on diverse workspaces is verified.

---

#### 9. Daemon Integration

**`internal/cli/topology_pool.go`**

```go
type topologyPool struct {
    mu     sync.Mutex
    stores map[string]*topology.Store
    cfg    config.TopologyConfig
}
func (p *topologyPool) Acquire(root string) *topology.Store { ... }
func (p *topologyPool) StopAll() { ... }
```

**`internal/cli/daemon.go`** — add `topologyPool *topologyPool` field; start in `runDaemon`; `StopAll()` on shutdown.

**`internal/cli/conn.go`** — add `topologyStore *topology.Store` to `connSession`. In `attachWorkspace` and `attachSynthetic`, call `startTopologyIndexer(root)` after `startQualityRunner(root)`:

```go
func (s *connSession) startTopologyIndexer(root string) {
    if !s.cfg.Topology.Enabled { return }
    s.stateMu.Lock()
    s.topologyStore = s.pool.topologyPool.Acquire(root)
    s.stateMu.Unlock()
}
```

**`internal/tools/write_deps.go`** — add `TopologyNotify func(path string)`. Write tools call it after each successful write. Daemon sets it to `s.topologyStore.Enqueue(path, opUpsert)` (nil when topology is disabled).

**Tool registration** — three topology tools receive a `topologyFn func() *topology.Store` from a closure over `s.topologyStore`.

---

#### 10. MCP Tools

All three follow `parseArgs → validate → run → format`; all stay under gocyclo 15.

**`topology_status`**
- Input: `{ "workspace": string (optional) }`
- Returns: index health, file counts, node/edge counts, DB size, last sync, indexer state, language coverage, last error.
- Degrades clearly when topology is disabled or store is nil.

**`topology_search`**
- Input: `{ "query": string, "kinds": []string, "language": string, "limit": 20, "include_snippets": true }`
- FTS5 MATCH on `name`, `name_tokens`, `qualified`, `signature`, `docstring`, `path`. BM25 ranking. Filters on kind/language post-query.
- Output per result: path+line, kind, score, field matched, snippet, `source=topology | mode=ranked | index=fresh|stale`.
- Always states index_status — agents must not mistake ranked discovery for exact proof.

**`topology_explore`**
- Input: `{ "name": string, "depth": 2, "max_nodes": 50, "max_bytes": 30000, "include_source": "signatures", "edge_kinds": []string }`
- Resolves name to a node (exact → FTS5 fallback), BFS up to `depth` hops, stops at `max_nodes` or `max_bytes`. Reports truncation.
- Output: tree-formatted neighbourhood with depth indentation, edge labels, path+line, `source=topology`, truncation notice.

**Context-budget contract (enforced):**

| Parameter | Default | Hard cap |
|---|---|---|
| `depth` | 2 | 4 |
| `max_nodes` | 50 | 200 |
| `max_bytes` | 30 000 | 100 000 |
| `include_source` | `signatures` | `full` is opt-in |

---

#### 11. Testing Plan

**Unit tests:**

| File | Coverage |
|---|---|
| `db_test.go` | Schema creation, WAL, FTS5 table, busy_timeout |
| `extractor_go_test.go` | Fixtures: empty file, functions, interface, struct+methods, tests, imports |
| `extractor_python_test.go` | Fixtures: class+methods, bare functions, imports, async def |
| `search_test.go` | Insert 3 nodes; exact name match; token-split match; docstring match; rank order |
| `indexer_test.go` | Upsert+read-back; delete cascade; stale detection on mtime change; double-upsert idempotent |
| `topology_status_test.go` | Disabled message when store nil; populated status otherwise |
| `topology_search_test.go` | Token-split search; kind filter; stale index reported |
| `topology_explore_test.go` | BFS to depth; max_nodes truncation; max_bytes truncation; unknown name error |

**Integration test** (`//go:build integration`): creates a temp workspace with `internal/cli/` Go files, runs full resync, asserts `topology_search("workspacePool")` returns ≥1 result, records resync timing for the >5× benchmark.

**Concurrency test:** goroutine enqueuing ops in a loop + goroutine running 100 FTS5 queries; assert no `SQLITE_BUSY` or data race. Run with `-race`.

---

#### 12. Phase 1 Definition of Done — Checklist

From `docs/todo.md`:

1. `internal/topology` package with SQLite schema for nodes, edges, FTS5, and index metadata → §4
2. Daemon-owned incremental indexer with debounce, stale cleanup, delete handling, manual resync → §7, §9
3. Go and Python extractors tested against fixtures → §6, §11
4. `topology_status`, `topology_search`, `topology_explore` as MCP tools with source/freshness metadata → §10
5. `topology_explore` enforces `max_nodes`/`max_bytes` and reports truncation → §10
6. Benchmark: topology symbol listing >5× faster than LSP cold start → §11
7. Concurrency test: MCP reads don't fail during indexing → §11

---

#### 13. Suggested Commit Sequence

Each commit must leave `make verify` green.

| # | Commit message | Contents |
|---|---|---|
| 1 | `feat(config): add [topology] config section` | `TopologyConfig`, defaults, validation, tests |
| 2 | `feat(topology): SQLite schema, Open(), WAL, FTS5 setup` | `db.go`, `db_test.go` |
| 3 | `feat(topology): public types and Store interface` | `topology.go`, `store.go` |
| 4 | `feat(topology): Extractor interface and registry` | `extractor.go` |
| 5 | `feat(topology/go): Go AST extractor` | `extractors/go/extractor.go` + fixture tests |
| 6 | `feat(topology/python): Python regex extractor` | `extractors/python/extractor.go` + fixture tests |
| 7 | `feat(topology): background Indexer with resync and coalescing` | `indexer.go`, `indexer_test.go` |
| 8 | `feat(topology): FTS5 search queries` | `search.go`, `search_test.go` |
| 9 | `feat(topology): BFS neighbourhood traversal with budget limits` | `explore.go`, `explore_test.go` |
| 10 | `feat(topology): StatusReport()` | `status.go` |
| 11 | `feat(cli): topologyPool — daemon-level per-workspace Store registry` | `topology_pool.go` |
| 12 | `feat(cli): wire topology into attachWorkspace, WriteDeps.TopologyNotify` | `conn.go`, `write_deps.go`, daemon registration |
| 13 | `feat(tools): topology_status MCP tool` | `topology_status.go` + tests |
| 14 | `feat(tools): topology_search MCP tool` | `topology_search.go` + tests |
| 15 | `feat(tools): topology_explore MCP tool` | `topology_explore.go` + tests |
| 16 | `test(topology): integration benchmark against real Go workspace` | `//go:build integration` test |
| 17 | `docs: topology in mcp-tools.md, AGENTS.md, CHANGELOG.md` | Docs + move todo.md section to todo-to-review.md |

---

#### 14. Risks and Watch-outs

**SQLite `database is locked`** — WAL + `busy_timeout = 5000` handles most contention. Keep write transactions short: one transaction per file, not per full resync batch.

**FTS5 is not regex** — `topology_search` descriptions must say "ranked indexed discovery — use `search_in_files` for exact verification." Never advertise it as exhaustive.

**Go AST parser panics on malformed files** — `go/parser` can panic on pathological inputs. Wrap `Extract()` in `defer recover()` in the indexer. Record error in `topology_files.error_msg`; never let an extractor panic reach the daemon.

**Resync on large workspaces** — a full walk of a monorepo is done entirely in the background. Report `state = "running"` in `topology_status`. Skip `vendor/`, `node_modules/`, `.git/`, `testdata/` in default exclude patterns.

**DB size growth** — FTS5 index is typically 1–2× the raw text of indexed fields. On a 10,000-file Go workspace this can reach hundreds of MB. Surface DB size in `topology_status`. Limit indexed docstring length (first 500 chars). Phase 2 should add compaction/vacuum strategy.

**`go/ast` is Go-only** — Python is heuristic; other languages are unsupported in Phase 1. Tool descriptions must state which languages are indexed. `topology_status` must list language coverage. Agents expecting TypeScript coverage will get empty results and must fall back to `search_in_files`.

**Config opt-in** — `Enabled = false` is correct for Phase 1. `topology_status` and `plumb doctor` must both clearly state when topology is disabled and how to enable it (`[topology] enabled = true` in `.plumb/config.toml`).
