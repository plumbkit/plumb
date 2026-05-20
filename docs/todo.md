# Plumb — Outstanding Work

Canonical index of known gaps, deferred work, and subtle footguns. Each entry carries enough context that another session can pick it up cold and execute.

Last reviewed against: **0.7.5** (2026-05-21). A full code-quality pass was added on 2026-05-20 — see [Code quality & engineering practices](#code-quality--engineering-practices). Topology Phase 1 completed and moved to `docs/todo-to-review.md`.

When you complete a TODO entry: **move its section to `docs/todo-to-review.md`** (do not just delete it), add a `CHANGELOG.md` entry for the version that ships the fix, in the **same commit**. If new gaps surface during the work, add them here in the same commit.

## Organisation

This file is organised by **topic**, not strictly by priority. Within each topic, items are ordered by priority (highest first). A separate ["The next two hours"](#the-next-two-hours) recommended-priority section at the very top cross-cuts the topics.

Topics:

- [Architecture](#architecture) — deep design changes, new contracts, new infrastructure
- [Features](#features) — net-new user-facing capabilities
- [Improvements](#improvements) — refinements to existing behaviour
- [Code quality & engineering practices](#code-quality--engineering-practices) — file size, complexity, lint hygiene, security findings, enforcement
- [Testing & verification](#testing--verification) — proving things actually work end-to-end
- [Bugs & known limitations](#bugs--known-limitations) — existing footguns; behaviour to be aware of
- [Considered and deferred](#considered-and-deferred) — items decided against or postponed

---

## The next two hours

Run `go test -tags=integration -timeout=3m ./cmd/smoke/` to verify the smoke checklist passes against your local setup (~30 min cold, < 10 s warm). After that, plumb is *proven* (not just claimed) production-ready against the primary supported LSP and client.

---

## Architecture

Deep design changes, contract changes, and new infrastructure. These are the items most likely to need design discussion before implementation.

### Plumb Topology: Phase 2

**Priority:** High — Phase 1 foundation is in production; Phase 2 completes the original vision.
**Effort:** Significant (multi-week, can be split across several PRs).
**Status:** Planning. Phase 1 shipped in 0.7.5. See `docs/todo-to-review.md` for the full Phase 1 completion record and the canonical list of what was explicitly deferred.

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

1. DoD-6 and DoD-7 benchmarks/stress tests pass under `-race`.
2. Extractor unit tests exist for Go and Python (all cases listed in item 2 above).
3. `topology_impact`, `topology_routes`, `topology_affected` implemented, tested, registered, and documented in `AGENTS.md` and `docs/mcp-tools.md`.
4. TypeScript/JavaScript extractor implemented and tested against fixtures.
5. LSP fallback wired for `list_symbols`, `find_symbol`, `workspace_symbols`; fallback is explicit in response.
6. TUI sessions panel shows topology state row; `plumb doctor` topology check passes.
7. Periodic resync wired when `resync_interval_minutes > 0`.
8. `drain` coalescing fixed to retain the last op per unique path across all queued files.
9. `isStale` extended to compare content hash in addition to mtime.
10. `make verify` stays green; golangci-lint 0 issues; all extractor packages have `_test.go` files.

**Watch out for.**

- TypeScript regex extraction is noisier than Python. Keep confidence lower and test against real `.ts` files from a known project (e.g. plumb's own `package.json`-free test fixture) before shipping.
- The LSP fallback timeout must be tuned carefully. Too short and it triggers unnecessarily on busy CI machines; too long and it defeats the purpose.
- `topology_routes` framework patterns are heuristic and can silently miss routes or false-positive on similarly shaped functions. Keep the confidence annotation visible in every result and document the limitations clearly.
- Adding TypeScript to `buildExtractors()` means the initial resync on any mixed-language workspace will take longer. Surface indexing progress in `topology_status` so agents don't assume the index is complete when it's still running.
- Periodic resync interacts with write-triggered resyncs. Make sure the ticker does not enqueue while a resync is already running — check `state == "running"` before enqueuing the periodic op.

---

### Advanced Memory Engine

**Priority:** Medium
**Effort:** Medium
**Status:** Planning

**The pitch — smarter memory retrieval without heavy infrastructure.**

Plumb's current memory system is deterministic (grep/glob over markdown files). While reliable, it forces the agent to explicitly search and remember to check for context. We can implement a more useful memory engine using the tools Plumb already has — SQLite, stats tracking, tool intercept hooks, and, later, Topology — without introducing a heavy vector database or always-on AI summariser.

The important distinction: memory must be **grounded, private, and budgeted**. An unbounded "remember everything" system will leak secrets, stale context, and noisy summaries into agent sessions. Plumb should stay conservative.

1. **Episodic Summaries via `stats.db` (Rule-based):**
   Currently, `session_start` gives generic repo orientation. Plumb already tracks every tool call in `stats.db`. When a session goes idle, the daemon should generate a lightweight "Episodic Summary" based on modified files and tools used.
   - **Mechanism:** A background task in the daemon that runs after a session hasn't seen a tool call for N minutes. It queries the `tool_calls` table for that session, extracts the list of touched files and high-level tool types (Reads vs Writes), and persists a 1-2 sentence summary using rule-based templates.
   - **Outcome:** `session_start` output appends: *"In your last session, you heavily modified `internal/auth/login.go` and used `find_references` on `UserSession`."*
   - **No LLM required:** This is strictly deterministic in 1.0; LLM-based "compression" of memories is a 2.0 research item.

2. **Association Memory (Co-occurrence):**
   Track temporal associations between files and symbols to surface implicit relationships that static analysis (LSP) might miss.
   - **Mechanism:** When a session touches file X and then file Y within a short window, or reads file X and then searches for symbol S, increment an association score in a new `associations` table.
   - **Outcome:** `read_file` on X can hint: *"Note: Working on this file often involves symbol S in `internal/utils`."* This provides "procedural memory" based on actual usage patterns.

3. **FTS5 Semantic Search & Background Indexing:**
   `search_memories` is currently a basic grep. We should index the Markdown memories into a SQLite table using the **FTS5** (Full-Text Search) extension.
   - **Mechanism:** The `internal/memory` package maintains an `fts_index` virtual table. On `write_memory`, `delete_memory`, or daemon start, a background worker crawls `.plumb/memories/*.md` and keeps the index in sync.
   - **Code-Aware Tokenisation:** Use a custom tokeniser (or regex-based preprocessing) to split `CamelCase`, `snake_case`, and `kebab-case` so `UserSession` matches `user` and `session`. This significantly improves relevance without needing embeddings.
   - **Indexed fields:** memory name, description/frontmatter, body, path globs, source paths/symbols, provenance labels, and generated-vs-user-authored confidence.
   - **Benefit:** Gives us ranking (relevance), stemming (matches "running" to "run"), prefix/phrase/proximity matching, and snippets/highlights out-of-the-box, making memory retrieval more resilient to exact keyword misses.
   - **Dependency:** this is the memory backend for [Workspace Search Engine](#workspace-search-engine-exact-scan--indexed-discovery). Memory FTS should feed ranked discovery, while `search_memories` must keep a deterministic grep fallback when FTS5 is unavailable or stale.

3. **Lifecycle Hooks for Proactive Context Injection:**
   Agents often forget to call `relevant_memories` when reading a file. We can inject this knowledge proactively at the tool-response layer.
   - **Mechanism:** Add an `OnAfterTool` interceptor that checks the `paths:` frontmatter globs for all workspace memories.
   - **Triggers:** If an agent calls `read_file`, `edit_file`, or `find_symbol` on a path that matches a memory, append a `[Hint: ...]` block to the response.
   - **Outcome:** `[Hint: There is a relevant memory 'auth-gotchas' attached to this file. Use read_memory to view it.]`

4. **Privacy and redaction before storage:**
   Any generated memory, episodic summary, or captured observation must pass a redaction layer before it is persisted.
   - Strip likely API keys, tokens, private keys, credentials, auth headers, cookie values, and `.env`-style secrets.
   - Do not store raw tool output by default; store compact, grounded summaries plus provenance.
   - Add a config kill-switch for automatic summaries.

5. **Deduplication and lifecycle:**
   Memory needs lifecycle metadata so it does not become stale clutter.
   - Content hash (`sha256`) for deduplication.
   - `created_at`, `updated_at`, `last_used_at`.
   - `source_session_id`, source tool call IDs, and touched file paths.
   - `confidence`: generated, user-authored, imported, inferred.
   - `supersedes` / `superseded_by` for replacing old decisions.
   - Optional `stale_after` for memories tied to fast-moving code.

6. **Budgeted injection:**
   `session_start` and tool-response hints must have strict byte/token budgets.
   - Default to short hints with memory names, not full memory bodies.
   - Include at most N memories unless the caller explicitly requests more.
   - Prefer high-confidence, recently-used, path/symbol-relevant memories.

7. **Topology-backed memory retrieval:**
   Once Plumb Topology exists, memory should attach to code entities, not only paths.
   - A memory can reference files, symbols, routes, tests, or packages.
   - `topology_explore` can include relevant memories for returned nodes.
   - `read_file`/`edit_file` can hint at symbol-level memories when the edited region overlaps a known symbol.
   - `session_start` can retrieve memories related to recently touched topology nodes.

**Definition of done — Phase 1.**

1. **FTS5 implementation:** SQLite FTS5 virtual table added to `internal/memory/store.go`. `write_memory` and `delete_memory` trigger incremental index updates. `search_memories` uses `MATCH` queries with relevance ranking, snippets/highlights, index freshness reporting, and a grep fallback if FTS5 is unavailable or stale.
2. **Privacy/redaction:** Add a small redaction package used before writing generated episodic memories. Unit tests cover common secret shapes.
3. **Episodic logic:** New `episodic_memories` table in `stats.db`. Daemon implements an idle-session listener that generates bounded summaries. `session_start` reads the most recent summary for the workspace and appends it only within a configured budget.
4. **Provenance:** Generated memories store source session/tool-call IDs and touched file paths. `read_memory` displays provenance metadata.
5. **Context injection:** `internal/mcp/server.go` or `internal/cli/daemon.go` gains a hook to inject memory hints into tool responses. Hint logic must be cheap: cache compiled glob patterns and memory metadata.
6. **Tests:** Unit tests verify FTS5 ranking, redaction, deduplication, provenance formatting, and budget caps. Integration tests verify that `read_file` on a tagged path includes the hint.

**Phase 2.**

1. Hybrid retrieval: combine FTS5 rank, path-glob relevance, recency, confidence, and usage count. Feed the same ranked memory hits into `workspace_search` once the search engine exists. Keep embeddings optional; do not add a vector database unless FTS5 quality is clearly insufficient.
2. Memory lifecycle commands/views: list stale memories, show unused memories, mark superseded, and prune generated memories older than a configured threshold.
3. Topology-backed retrieval once Topology is available.
4. TUI Memory section consumes the same store: memory list, details, provenance, source paths/symbols, and stale/superseded state.

**Watch out for.**

- Do not store secrets. Automatic memory is only acceptable if redaction and opt-out exist from day one.
- Do not inject full memories into every session. Hints should point to memories; agents can call `read_memory` when needed.
- Generated summaries must be visibly generated and lower-confidence than user-authored memories.
- Summaries can become stale after refactors. Provenance and topology links make stale detection possible.
- Keep memory retrieval deterministic and explainable. If an agent sees a memory, it should be able to tell why it was shown.

---

### Workspace Search Engine: Exact Scan + Indexed Discovery

**Priority:** High
**Effort:** Medium/significant. Depends on Topology and Advanced Memory storage pieces for the full version.
**Status:** Planning.
**Dependencies:** [Plumb Topology Phase 2](#plumb-topology-phase-2) provides indexed code entities; [Advanced Memory Engine](#advanced-memory-engine) provides indexed memories. This item defines the user-facing search contract so overlapping search tools do not confuse MCP clients.

**Problem.**

`search_in_files` is intentionally grep-like: it scans current files, supports regex/smart-case/context lines, honours `.gitignore`, and can annotate hits with enclosing LSP symbols. That is the right tool for exact text checks, audits, and replacement prep.

What it lacks as a discovery engine:

- No relevance ranking: every hit is roughly equal, even if one result is clearly the best answer.
- No persistent index: repeated broad searches rescan the workspace and produce large tool output.
- No fuzzy/token-aware matching: `workspacePool`, `workspace pool`, `workspace_pool`, and path segments are different unless the caller writes a careful regex.
- No unified corpus: source files, symbols, docs, memories, comments, routes, tests, and paths are searched through different tools.
- No first-class snippets/highlights or field labels that say *why* a result matched.
- Broad grep output is token-expensive and often forces follow-up `read_file` calls.
- Literal search is not explicit in the MCP API: `pattern` is currently compiled as a Go/RE2 regular expression, so clients must escape regex metacharacters themselves to search for a literal like `foo.bar`.

**Design rule — two lanes, not one overloaded tool.**

Do **not** replace `search_in_files` with FTS5. Keep two explicit mechanisms with different names, descriptions, and output metadata:

| Tool | Contract | Use when |
|---|---|---|
| `search_in_files` | Exact scan of current filesystem contents. Literal or regex matching, context lines, exhaustive-ish result set bounded by `max_results`. | The agent needs every occurrence, exact verification, audits, or safe replacement prep. |
| `workspace_search` | Ranked indexed discovery across code, docs, symbols, paths, routes, tests, and memories. FTS5-backed, freshness-aware, approximate by design. | The agent asks conceptual questions like "where is daemon locking handled?" or needs likely starting points. |
| `topology_search` | Code-structure search over Topology entities only. | Internal/advanced code-map queries, and as one backend feeding `workspace_search`. |
| `search_memories` | Memory-only search. | Direct memory lookup, with FTS5 ranking plus deterministic grep fallback. |

MCP tool descriptions must include the decision rule in plain language:

```text
Use search_in_files for exact literal or regex matches from current file contents.
Use workspace_search for ranked discovery across indexed code, docs, symbols, and memories.
```

**Exact search API alignment.**

Before building indexed discovery, align `search_in_files` with `find_replace` so MCP clients have one mental model for text patterns:

```json
{
  "pattern": "foo.bar",
  "use_regex": false
}
```

Target contract:

- Add `use_regex` to `search_in_files`, matching `find_replace`'s parameter name and semantics.
- `use_regex: false` means literal text search; internally it should use a fast literal path (`bytes.Contains`/literal counting) rather than compiling a regex.
- `use_regex: true` means Go/RE2 regex search; document that RE2 does not support lookbehind or backreferences in the pattern.
- Keep smart-case behaviour for both literal and regex searches unless `case_sensitive` is set.
- For compatibility, decide the default deliberately:
  - safest short-term default: keep current behaviour by treating an omitted `use_regex` as `true`, then let clients opt into literal search with `false`;
  - cleaner long-term pre-1.0 default: match `find_replace` by making plain text the default and regex opt-in with `use_regex: true`.
- If the default changes, call it out in `CHANGELOG.md`, `AGENTS.md`, `README.md`, and `docs/mcp-tools.md`; because plumb is pre-1.0 this is acceptable if the migration is explicit.
- Error messages should distinguish regex compile failures from literal searches; literal patterns should never fail because of regex syntax.

Agent-facing guidance should be blunt: agents usually want literal search unless they are intentionally using regex operators. This is better for MCP clients because `foo.bar`, `internal/cli`, `plumb.daemon.lock`, and `workspacePool` behave the way users expect without manual escaping.

**User-facing output contract.**

Every indexed search result should make its semantics visible so agents do not mistake ranked discovery for exact proof:

```text
source: fts5|topology|memory|hybrid
mode: ranked
index_status: fresh|stale|building|missing
exact_match: true|false
field: symbol|signature|path|comment|body|memory|route|test
score: <rank or normalised score>
path: <workspace-relative path>
line: <best-known line, optional>
snippet: <short highlighted excerpt>
why: <short reason, e.g. "symbol name + memory path glob">
```

For exact scans, `search_in_files` should continue to advertise `source=filesystem` / `mode=exact-regex` in docs and, if useful later, in compact output metadata.

**Search ladder for agents.**

Document the recommended order clearly in `AGENTS.md`, `README.md`, and `docs/mcp-tools.md` once implemented:

1. Use `workspace_search` for broad conceptual discovery.
2. Use `topology_explore` or LSP semantic tools for structure around promising hits.
3. Use `search_in_files` to verify exact text or regex matches.
4. Use `read_file(start_line, end_line)` for bounded inspection.
5. Use `find_replace` only after exact verification when editing by text pattern.

**Implementation plan.**

1. First align `search_in_files` and `find_replace` around `use_regex`, literal-vs-regex semantics, smart-case behaviour, and docs. This can ship independently of Topology/FTS5.
2. Keep `search_in_files` behaviour-preserving for regex mode. Do not route regex searches through FTS5.
3. Add `workspace_search` only after at least one indexed backend exists. Minimum viable backend can be memory-only FTS5 or topology-only FTS5, but the response must say which corpus was searched.
4. Add a small search broker package that queries available backends in parallel with per-backend timeouts and merges ranked results:
   - topology FTS: symbols, paths, routes, tests, signatures, comments/docstrings.
   - memory FTS: memory name, description, body, path/symbol refs, provenance.
   - docs/source text FTS: markdown docs and optionally source-file body/comment chunks.
5. Use daemon-owned background indexing. Index updates are triggered by attach, file writes, deletes, renames, memory writes, and manual resync.
6. Track freshness per result: indexed file hash/mtime, backend generation, extractor version, last error.
7. Use continuation handles for large result sets. `workspace_search` should return top N plus `next_page_token`, not dump the full ranked list.
8. Add query options:
   ```json
   {
     "query": "daemon lock",
     "corpus": ["code", "docs", "memory"],
     "mode": "ranked",
     "limit": 20,
     "include_snippets": true,
     "freshness": "allow_stale"
   }
   ```
9. Add exact-search handoff hints: when `workspace_search` returns a high-confidence source-code hit, include a compact suggestion for the exact verification call, e.g. `search_in_files(pattern="plumb.daemon.lock", path="internal/cli", use_regex=false)`.

**Definition of done — Phase 1.**

1. `search_in_files` and `find_replace` expose aligned `use_regex` semantics, smart-case handling, and docs.
2. `search_in_files` has tests for literal patterns containing regex metacharacters (`foo.bar`, `a+b`, `path/to/file.go`, `plumb.daemon.lock`) and regex patterns when `use_regex=true`.
3. Tool naming and descriptions make the exact-vs-ranked distinction unambiguous.
4. `workspace_search` exists with at least one FTS5-backed corpus and reports corpus coverage/freshness in every response.
5. `search_in_files` remains the exact filesystem scanner and its docs explicitly say it is not ranked discovery.
6. Indexed results include field labels, score/rank, source, freshness, and snippet/why metadata.
7. Missing/stale/building index states degrade clearly and suggest `search_in_files` when exact current contents matter.
8. Tests cover ranking order, stale-index reporting, fallback behaviour, and MCP descriptions so tool selection stays clear for agents.

**Watch out for.**

- FTS5 is not regex. Do not promise exhaustive exact matching from indexed search.
- Literal-vs-regex defaults are an agent UX decision, not just an implementation detail. Prefer the API that avoids accidental regex for common user strings, while documenting any compatibility transition clearly.
- FTS5 can be stale. Exact edits and replacement prep must verify against current files.
- Tool naming matters. Avoid a vague tool named just `search`; it will blur semantics for MCP clients.
- Code tokenisation is product-critical. If names are not split and preserved correctly, ranked search will feel worse than grep.
- Ranking can become opaque. Include a short `why` field so agents can reason about whether a result is worth opening.
- Index DBs can grow. Surface DB size and corpus counts in `topology_status` / future search status views.

---

### Shared Topology + Memory Opportunity

The best version of these two features is not two separate systems:

- Topology knows code entities: files, symbols, routes, tests, packages.
- Memory knows decisions and history: why something exists, what was tried, what failed, what the user prefers.
- Joining them lets Plumb answer: "What code matters here, and what do we already know about it?"

**Storage and relationship model.**

Memory and Topology should be related, but they do **not** need to share one database. They have different lifecycles and failure modes:

- `topology.db` is a derived, rebuildable, high-churn code index. It can be dropped and recreated if the schema changes or the index becomes stale.
- `memory.db` / the memory index is durable project knowledge: user-authored notes, generated summaries, provenance, stale/superseded state, and redaction-sensitive history. Migration failure must never silently destroy it.
- Keeping them separate reduces lock contention and corruption blast radius. Topology indexing should not block memory reads, and memory writes should not depend on a topology rebuild being healthy.
- Both should live under the project `.plumb/` directory because both are project-scoped. A plausible layout is:

  ```text
  <workspace>/.plumb/
    memories/
      *.md              # existing user-authored memory source
    memory.db           # FTS/provenance/generated-memory index
    topology.db         # rebuildable semantic graph
  ```

The relationship should be a shared reference contract, not shared storage. Memory may reference topology entities using stable `CodeRef` fields such as file path, package/module, symbol name, kind, signature hash, and optional content hash/range. Do **not** store topology row IDs inside memory: topology is rebuildable, and row IDs can change after reindexing.

Example resolver shape:

```go
type CodeRef struct {
    Kind          string
    File          string
    SymbolName    string
    Package       string
    SignatureHash string
}

type MemoryResolver interface {
    MemoriesForRefs(ctx context.Context, refs []CodeRef) ([]MemoryHit, error)
}
```

The join happens in Go code: `topology_explore` resolves current code nodes from `topology.db`, turns them into stable `CodeRef`s, asks memory for matching notes from `memory.db`, and merges the results in the response. Architectural rule: **memory may reference topology, but topology should not depend on memory**. Topology remains a clean derived code index; Memory remains the higher-level knowledge layer.

What to watch for:

- Generated episodic memory will use data from the global `stats.db`, but the resulting memory should be written to the project-local memory store. The flow is: global stats -> bounded project/session summary -> project `.plumb` memory.
- If a daemon session touches multiple workspaces, generated summaries must be partitioned by workspace so one project's context never leaks into another project's memory DB.
- Project-local DBs make backup/export simple, but also make accidental commits more dangerous if `.plumb/` is ever unignored. Keep generated/private data clearly marked and redacted.
- Topology schema and extractor versions need rebuild policy; memory schema migrations need preservation and backup behaviour.
- Expose user controls: disable generated memory, prune generated memory, rebuild topology, show DB size/status, and exclude paths from indexing.

Concrete combined behaviours:

1. `topology_explore(symbol)` returns related memories alongside code neighbours.
2. `session_start` retrieves memories for recently edited topology nodes.
3. `edit_file` hints at memories attached to the function/class being edited, not just the file path.
4. `topology_affected(files)` can include memories about test strategy or known risky areas.
5. Memory stale checks can use topology: if a referenced symbol disappears or moves, mark the memory as potentially stale.

---


### Features

Net-new user-facing capabilities. Lower architectural risk than the Architecture section — these mostly compose existing primitives.

---

### Implement Memory TUI section

**Priority:** Medium.
**Effort:** Medium.
**Status:** Planning.
**Description:** Implement the Memory section in the TUI (index 2 in the section menu). 
- **Layout:** Similar to the Sessions section (left panel: list of memories; right panel: details).
- **Details Panel:** Show YAML frontmatter metadata (name, description, creation date, updated date), the memory body (markdown content), and a list of paths the memory is relevant to (from the frontmatter `paths` field).
- **Navigation:** Support `j/k` to navigate the memory list, `tab` to switch panels, and enter to toggle detail/list view or search as in other sections.
- **Tools:** Hook into existing memory tools (`list_memories`, `read_memory`).
**Watch out for:** Ensure long memory content in the right panel is scrollable and handled consistently with the Log Detail and Call Detail panels.

### Implement Color Scheme Picker

**Priority:** Medium.
**Effort:** Medium.
**Status:** Planning.
**Description:** Create a pop-up panel in the TUI that allows users to browse and switch between available color schemes.
- **Layout:** A centered modal/pop-up showing a grid or list of available themes with a live preview.
- **Navigation:** Arrow keys to select a theme, Enter to apply.
- **Persistence:** Ensure the selected theme is saved to `~/.config/plumb/config.toml` (or appropriate config location) and applied immediately without daemon restart.
**Watch out for:** Ensure the modal doesn't break the existing TUI layout and that the color palette transition is smooth.

---

## Improvements

Refinements to existing behaviour. No new contracts, no new infrastructure — just better defaults or more flexibility.

### Clean up legacy database migration logic

**Priority:** Low (Cleanup for v0.9)
**Effort:** Small
**Status:** Planning.

**The problem.**
The `internal/stats/db.go` file contains a hand-rolled SQLite migration system (the `migration` struct, `migrations` slice, `migrate` function, and `hasColumn` check) that walks older schemas forward from `user_version` 1 up to the current version. Since backward compatibility with these older database versions is not required until version 0.9, this logic is obsolete and adds unnecessary complexity.

**Definition of done:**
1. **Remove migration logic in `internal/stats/db.go`**:
   - Delete the `migration` struct and `migrations` slice.
   - Delete the `migrate` function.
   - Delete the `hasColumn` function.
   - Delete `ErrReadOnlySchemaUpgradeRequired`.
   - Update `Open()` to execute the base `CREATE TABLE` schema and stamp `PRAGMA user_version = SchemaVersion`, removing the conditional migration check.
   - Update or remove `checkReadOnlySchema` and simplify `OpenReadOnly()`.
2. **Remove migration tests in `internal/stats/db_test.go`**:
   - Delete `TestOpenReadOnlyOldSchemaTellsUserToDeleteDB`.
   - Update `TestOpenCreatesCurrentGlobalSchema` to remove `hasColumn` assertions (since the function is gone).
   - Remove any other tests specifically asserting migration from v1/v2/v3/v4 to v5.
3. **Update Documentation**:
   - Remove or simplify references to schema migrations and `user_version` logic in `docs/architecture.md`.
   - Review `AGENTS.md` (and symlinks) and remove details about how migrations are applied if they exist outside of historical logs.

### Configurable TUI Shortcuts

**Priority:** Medium.
**Effort:** Medium.
**Status:** Planning.
**Description:** Allow users to define custom keyboard shortcuts for the TUI within `config.toml` (e.g., mapping navigation or section switching to different keys). This will improve accessibility and support custom user workflows.
**Watch out for:** Ensure key conflicts are detected and reported clearly, and that the default bindings remain intuitive if no configuration is provided.

### Dashboard alerts — telemetry-backed follow-ups

**Priority:** Low-medium.
**Effort:** Medium. Requires daemon/stat telemetry changes before the TUI can present this accurately.
**Status:** Deferred follow-up. The Dashboard already ships sparse actionable alerts for daemon availability, stale metrics, session-load errors, stats DB open failures, unresolved or auto-attached workspaces, LSP-unavailable workspaces, daemon version mismatches, and recent uptime tool-error spikes.

**Remaining ideas:**

1. Surface config parse/load errors only when the daemon records the exact failing config source and error.
2. Surface write-safety warnings for strict-mode friction, rate-limit exhaustion, and recent write rejections once those events are counted in stats or daemon metrics.
3. Keep these alerts thresholded and actionable; do not warn merely because strict mode or rate limiting is enabled.

**Definition of done:**

1. Daemon or stats telemetry records config-load failures and write-safety rejection counts with enough context for a concise Dashboard message.
2. Dashboard alerts show only non-zero, recent, actionable conditions with a clear next step.
3. Tests cover quiet/default state and each alert threshold.

---

### Review and Sanitise Color Schemes

**Priority:** Medium.
**Effort:** Medium.
**Status:** Planning.
**Description:** Perform a comprehensive review of the current color palette and themes (`internal/tui/theme.go`, `internal/tui/styles.go`).
- **Goal:** Ensure all color definitions are solid, consistent, and accessible.
- **Action:** Sanitise existing color variables, eliminate any hard-coded colors in UI components, and define a central, well-typed theme interface.
- **Outcome:** A robust and scalable theming system that is easy to extend and maintain.
**Watch out for:** Ensure that changes maintain consistency across light/dark mode and that all UI elements (borders, text, accents, alerts) remain legible under the new scheme.

### Security: Tool path restrictions (Jailing) and temp aliases

**Priority:** High.
**Effort:** Medium.
**Status:** Planning.
**Description:** To improve security for LLM-driven environments, all MCP tools should be restricted to a safe set of directories. This prevents the agent from reading or writing sensitive system files (e.g., `/etc/passwd`) while still allowing legitimate workspace and configuration management.
- **Goal:** Implement a "Safe Path" allowlist for all filesystem tools.
- **Action:**
  - Update filesystem tools (`read`, `write`, `edit`, `delete`, `rename`, `copy`) to validate paths against an **Allowlist**:
    1. The resolved workspace root and its subdirectories.
    2. The system temporary directory.
    3. Plumb's own configuration and cache directories (`~/.config/plumb`, `~/.cache/plumb`).
    4. Optional: Global personal memory locations (e.g., `~/.gemini/`).
  - Support temporary directory expansion safely using `os.ExpandEnv` or a dedicated pseudo-prefix (like `@temp/` or `temp://`) to resolve `$TEMP`, `$TMP`, `%TEMP%`, or `%TMP%` correctly across platforms.
- **Definition of done:** Shared path-validation helper implemented. Tests verify that attempts to access unauthorized paths return a clear error (e.g., *"Access denied: path is outside allowed directories"*), while workspace and temp operations proceed normally. TUI remains unaffected as it bypasses the MCP tool layer for its internal data.

---

### Java adapter (jdtls) — multi-OS polish and CI hardening

**Priority:** medium — validated first version, but still needs cross-platform polish.
**Effort:** Small–medium.
**Status:** Remaining: cold-start tuning, binary naming docs, doctor version-check coverage. (`rootURI` Windows safety, CI integration step, and write-tool `DidOpen`/`DidClose` shipped in 0.6.5; see `docs/todo-to-review.md`.)

**Cross-platform note:** current real-binary validation has only been exercised on macOS. Linux and Windows coverage are expected pre-v1 hardening work, not a blocker for the first validated Java adapter version.

Known gaps to address:

1. **Cold-start latency.** jdtls starts a JVM and loads Eclipse plugins on first run; the integration test allows 5 minutes for ServiceReady and a further 2 minutes for diagnostics after DidOpen. In CI on a cold runner this may be tight — monitor and raise the deadline if needed, or pre-warm the JVM cache in the CI step. Set `JDTLS_FRESH_DATA=1` to force a hermetic per-test data directory (slower; default reuses `.testcache/jdtls-data` for warm-cache local runs).

2. **`jdtls` binary name on non-Homebrew installs.** The compiled default is `command = "jdtls"`. On Linux/Windows the launcher may be named differently (e.g. `jdtls.sh`, `jdtls.bat`, or a full path). Document this in `docs/adding-an-lsp.md` and consider a `command` override example in the config docs. Users can already override via `[lsp.java] command = "..."` in config.toml.

3. **`plumb doctor` Java runtime version check.** The check calls `java --version` and parses the first output line. This covers OpenJDK and GraalVM. Confirm it also handles Eclipse Temurin, Microsoft Build of OpenJDK, and Amazon Corretto version strings; add test cases in `doctor_test.go` once that file exists.

**Definition of done:** binary naming documented; doctor version-check covers major JDK distributions.

---

### Java adapter (jdtls) — daemon performance and memory budget

**Priority:** medium-high before enabling Java by default or calling it production-grade.
**Effort:** Medium. Requires measurement on real Java projects and a small daemon lifecycle design pass.
**Status:** Planning only. No implementation started.

The current Java adapter follows plumb's existing daemon architecture: one long-lived daemon, one shared language-server process per detected workspace root, and one cache/invalidator pair per pool entry. That is the right first implementation because it preserves the same contract as Go and Python: multiple MCP sessions against the same workspace reuse one LSP process instead of each conversation spawning its own server.

jdtls changes the resource profile materially, though. Unlike gopls and pyright, it starts a JVM, loads Eclipse plugins, creates an Eclipse workspace data directory, imports Maven/Gradle projects, indexes source and dependencies, and can keep substantial heap and index state resident. In a single-daemon process model, that means Java support can turn plumb from "small background helper plus a few language servers" into "a daemon supervising one or more JVMs". That is acceptable, but it needs explicit budgets and lifecycle rules.

**Current expected impact.**

- **Cold start latency:** jdtls startup is dominated by JVM launch, plugin load, project import, and index warmup. First useful diagnostics can take seconds on small projects and much longer on large Maven/Gradle workspaces. `plumb doctor` and the integration test already hint at this by treating jdtls version/startup as potentially slow.
- **Resident memory:** each Java workspace can keep a separate JVM alive. Even if the plumb daemon itself remains small, the total background footprint is daemon memory plus every supervised LSP process. Multiple Java workspaces can therefore multiply memory use quickly.
- **Disk cache growth:** plumb computes a per-workspace `jdtls-data` directory under the cache dir. That isolates Eclipse state correctly, but the cache can grow with every Java project ever opened unless there is retention/cleanup policy.
- **CPU spikes:** project import, dependency resolution, background indexing, and diagnostics can continue after the initial LSP handshake. In the singleton daemon model this background work can coincide with active tool calls from unrelated sessions or workspaces.
- **User-perceived responsiveness:** the first Java tool call that causes `workspacePool.acquireLang` to start jdtls can block until the supervisor's `OnStart` completes. That is the correct correctness boundary, but it may feel much slower than Go/Python.

**Questions to answer with measurement.**

1. Measure cold and warm startup on at least:
   - tiny Maven fixture,
   - medium Maven or Gradle project,
   - large multi-module project.
2. Record:
   - time to process start,
   - time to `initialize` response,
   - time to first `publishDiagnostics`,
   - peak and steady resident memory for the jdtls process,
   - size of the per-workspace `jdtls-data` directory,
   - CPU load during import/indexing.
3. Compare against gopls and pyright using the same daemon path so the numbers describe plumb's actual architecture, not standalone LSP behaviour.
4. Test multiple Java workspaces attached to the same daemon. The important metric is aggregate memory/CPU, not only single-project startup.

**Daemon design options.**

- **Keep the current in-daemon supervisor model, but add lifecycle policy.** This is the smallest change. Add idle shutdown for expensive LSPs, configurable per language:

  ```toml
  [lsp.java]
  enabled = true
  idle_timeout = "20m"
  max_workspaces = 2
  memory_budget_mb = 2048
  ```

  The daemon would stop idle jdtls processes while preserving cached metadata where safe. The next Java request restarts the server.

- **Add per-language process budgets.** Keep one daemon, but enforce "only N Java language servers active at once". If a new Java workspace attaches while the budget is full, either evict the least-recently-used idle entry or return a clear "Java LSP capacity reached" diagnostic.

- **Introduce an independent worker process for heavyweight language services.** The daemon remains the MCP endpoint and session authority, but Java LSP supervision moves into a worker process. This is worth considering if jdtls proves unstable, memory-heavy, or prone to long blocking lifecycle operations. Benefits:
  - a crashed or wedged Java worker cannot destabilise the main daemon;
  - memory accounting and process cleanup are clearer;
  - Java-specific logs, cache cleanup, and restart policy can be isolated;
  - future heavyweight features (quality analysis, Gradle import, code actions) can share the worker boundary.

  Costs:
  - new IPC contract between daemon and worker;
  - more moving parts during setup/debugging;
  - duplicated lifecycle and health reporting unless designed carefully;
  - harder cross-language features if workers own different pieces of state.

Recommendation: **do not introduce workers yet**. First add measurement and idle/budget controls to the existing pool. Revisit workers only if real data shows jdtls memory or failure modes are too costly for the main daemon to supervise directly.

**Definition of done.**

1. Add a benchmark/smoke document or command that records startup, first diagnostics, RSS, and cache size for Java, Go, and Python workspaces.
2. Add `plumb doctor` or `plumb status` visibility for active LSP processes: language, workspace, PID, uptime, state, and approximate RSS where the OS supports it.
3. Add configurable idle shutdown for Java LSP processes, defaulting to a conservative timeout while Java remains experimental.
4. Add a cache-retention policy for `jdtls-data` directories so old project state does not grow forever.
5. Decide, based on measured data, whether per-language worker processes are justified. If yes, design the worker IPC contract as a separate architecture TODO before implementing.

**Watch out for.**

- The singleton daemon is a feature: it prevents each MCP conversation from spawning its own jdtls. Do not lose that sharing property when experimenting with workers.
- Memory belongs mostly to child LSP processes, not the Go daemon heap. Any performance report should separate daemon RSS from supervised-process RSS.
- Maven/Gradle dependency resolution can make first-run measurements noisy. Record warm-cache and cold-cache numbers separately.
- A short startup timeout that works for gopls may be wrong for jdtls. Make Java-specific timeouts explicit rather than raising global limits for every language.
- Idle shutdown saves memory but can make the next Java query slow. Surface this state in `doctor`/TUI so users understand why the next request is warming up.

---


## Code quality & engineering practices

This section tracks the ongoing quality standard for the plumb codebase. CQ-1 through CQ-8 are all complete — see `docs/todo-to-review.md` for history.

**Current baseline (golangci-lint v2.12.2, post CQ-8, 2026-05-21):**

```
0 issues on ./...
```

**Standing rules** (enforced by CI and pre-commit hook):
- `make verify` must be green before every commit (`build + test + lint`).
- No first-party non-test function may exceed gocyclo 15. Decompose before merging.
- No non-test source file over ~600 lines. Exception: `internal/lsp/protocol/types.go` (LSP spec type catalogue).
- Every gosec finding must be fixed or have a one-line justification annotation.
- Format via `golangci-lint run --fix ./...`, never the standalone `gofumpt` binary.

When new findings appear, add them as a numbered CQ item here, fix them, then move the item to `docs/todo-to-review.md` with a `CHANGELOG.md` entry.

---

## Bugs & known limitations

Footguns and behaviour to be aware of. None of these are urgent — they are documented here so anyone touching the relevant subsystem can make an informed decision (fix it, work around it, or leave it alone).

### `expected_mtime` is voluntary; strict mode is opt-in

Agents can ignore the mtime header. Strict mode (which forces the check) is off by default. For a hostile or buggy agent, the per-path lock is the only real defence — and it only catches *concurrent* corruption, not "agent edits stale content because it didn't bother to re-read".

**Why it's not fixed:** strict mode is too noisy as a default. Most legitimate workflows would hit "must read first" rejections constantly during the first session against a new project.

**Recommendation:** for projects where this matters, set `[edits].strict = true` in `.plumb/config.toml` at the project root. Per-project config is the right knob.

---

### `ReadTracker` is per MCP connection, not per agent identity

`NewReadTracker()` is called once per `handleConn` in the daemon. If one Claude Desktop instance opens N tabs that each spawn separate `plumb serve` processes (which connect as separate sessions), each gets its own `ReadTracker`. Strict mode's "you must have read this *in this session*" is per MCP connection, not per human-meaningful "user activity".

**Why it's not fixed:** there's no reliable per-agent identity exposed to the daemon today. Client info is captured (`OnClientInfo`) but multiple connections from the same client are common and expected.

**When to fix:** when you have a strong notion of "this is the same agent across reconnects" — typically would require the MCP client to send a stable session-id header.

---

### Daemon-version mismatch warns but doesn't enforce

After a `make build`, `plumb serve` reads `~/Library/Caches/plumb/plumb.version` and warns to stderr: "connected daemon is X but this binary is Y — run `plumb stop`". It does **not** auto-restart the daemon. The warning is informational; nothing changes until the user runs `plumb stop && plumb serve`.

**Why it's not auto-fixed:** killing a daemon mid-session disrupts every other open conversation. Auto-restart would be hostile to multi-conversation use. The user needs to know to restart, but the timing is theirs.

**Recommendation:** if the warning appears in your workflow regularly, add `plumb stop` to your `make build` chain.

---

### Symlink resolution falls through on broken symlinks

`safeWrite` calls `filepath.EvalSymlinks` to resolve the target before writing. If the symlink is broken (points at a non-existent path), `EvalSymlinks` returns an error and `safeWrite` falls back to using the original symlink path. Then `os.Stat(path)` returns `IsNotExist`, the file is treated as new, and `os.Rename` replaces the broken symlink with a real file containing the new content.

**Why it's probably the right behaviour:** if the symlink target doesn't exist, the user's intent is likely to *create* the file (perhaps writing the target through the link's location). Treating the write as a new-file create is the most user-friendly outcome.

**When this could surprise someone:** if they expected plumb to refuse to write to broken symlinks. It doesn't.

---

## Considered and deferred

Items raised in past reviews and decided against (or deferred deliberately). Listed so future sessions don't re-litigate.

- **`WriteDeps` refactor** — done in 0.5.4. No longer pending.
- **Push to `origin/main`** — explicit per-session user decision. Kept local; user pushes when ready.
- **Style nits across the codebase** (`for range n` modernisation, `errors.AsType[T]`, `WaitGroup.Go`) — applied opportunistically in files touched in 0.5.x. Not chasing across the rest of the codebase; if you touch a file, modernise it; otherwise leave it.
- **Bigger TUI features** — out of scope for 0.5.x. Worth a dedicated 0.6 line. Three distinct ideas were deferred:

  **Filterable panels** — the Tools and Recent Tools tables currently show everything. Filtering would let you type something like `edit` and only see `edit_file` rows. Useful when you have many distinct tools and want to focus on one category of activity.

  **Search box** — free-text search across the call history in the popup. Right now you navigate the timestamp list with j/k/pgdn/pgup and there's no way to jump to a specific call except scrolling. A search box would let you type a date, tool name, or fragment of args and jump directly to matching calls.

  **Write-targets visualisation** — the most unique to plumb. Tools that mutate files (`edit_file`, `write_file`, `delete_file`, `rename_file`) touch specific paths. A write-targets view would show *which files* the agent has been editing — a per-file activity summary rather than a per-tool summary (e.g. `model.go — 14 edits`, `db.go — 3 edits`). It answers "what has the agent actually been touching in my project?" rather than "which tools did it use?".
- **Native Windows support** — `safeWrite`'s atomic rename relies on POSIX rename-over-existing semantics. Windows handles this differently across Go versions. Full Windows support would also require: (a) resolving `%LocalAppData%` and other Windows environment variables in path parameters, and (b) `find_files`/`search_in_files` awareness of Windows-specific naming discrepancies (e.g. product brand names vs underlying technology names in install paths). Not on the roadmap unless someone asks.
- **`run_command` / shell execution tool** — requested for running scripts (PowerShell validation, unit tests) directly against the workspace without a user round-trip. Deferred: exposing arbitrary shell execution from the daemon is a significant security surface, especially when plumb is connected to a cloud LLM. Prompt-injection → shell execution is a real threat. If revisited: start with a locked-down command runner that only allows commands listed in `.plumb/allowed-commands.toml`, signed by the workspace owner.
- **Per-agent identity for rate limiting and read tracking** — see Bugs & known limitations entries above. Requires upstream MCP support for a stable client-session header.

---

## How to use this file

> **IMPORTANT — completed item workflow:** when a todo is done, **move its section to `docs/todo-to-review.md`** rather than deleting it. This preserves the context and rationale for future reference. Add a `CHANGELOG.md` entry in the same commit.

1. **Pick up an item:** read its section in full. The acceptance criteria (Definition of done) and the Where to start pointers should be enough to begin without re-deriving the problem.
2. **While working:** if you find a new gap, add it to this file in the same commit as your fix.
3. **When you finish:** move the section from this file to `docs/todo-to-review.md`, add the corresponding entry to `CHANGELOG.md` under the version that ships the fix, and commit all three changes together.
4. **If you can't finish:** leave the section in place but add a short "Status:" note describing how far you got and what's blocking, so the next person doesn't start from scratch.

The cost of *not* capturing a gap is high — months later, the gap turns into a mystery bug or a confused new contributor. The cost of writing it down is one paragraph. Always favour capturing.
