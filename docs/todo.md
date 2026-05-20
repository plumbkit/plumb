# Plumb — Outstanding Work

Canonical index of known gaps, deferred work, and subtle footguns. Each entry carries enough context that another session can pick it up cold and execute.

Last reviewed against: **0.6.8** (2026-05-20). A full code-quality pass was added on 2026-05-20 — see [Code quality & engineering practices](#code-quality--engineering-practices).

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

### Code-quality differential after edits

**Priority:** ⭐ top architectural priority. Discuss before implementing.
**Effort:** Significant (multi-day, possibly multi-week). Phased delivery makes sense.
**Status:** Idea captured. Not started.
**Discussion:** See [Real-time Code-quality Feedback for Agents](ideas.md#real-time-code-quality-feedback-for-agents) for the product shape, tradeoffs, and open questions.

**The pitch — what makes plumb genuinely different.**

Today plumb writes code and tells the agent two things: *what changed* (line-range summary in the response) and *what's broken* (post-write diagnostics from the LSP). That matches what every other "edit a file" tool does.

The differential — the thing no other MCP tool on the market does — is to **return code-quality findings alongside the edit**. Not just compile errors from gopls; *style and idiomatic-quality* findings from offline analysers, the kind of thing GoLand or `golangci-lint` would tell you. After every edit, plumb runs the relevant analyser(s) for the file's language and appends a "code quality" section to the response:

```
applied 1 edit to internal/foo.go (412 bytes)
mtime: 2026-05-11T15:42:01...
lines changed: L34-38

diagnostics after write: (none)

code quality (golangci-lint, ruff, ...):
  L37 ineffassign: ineffectual assignment to `x`
  L42 gocritic:   if-else block can be a switch statement
  L65 prealloc:   slice `results` is never reallocated; consider make([]T, 0, expectedSize)
```

Why this is architecturally important, not just a nice feature:

- It elevates plumb from "a better way to edit files" to "a code-review-loop in the inner agent loop". The agent learns *before its next turn* that its edit, while syntactically valid and type-safe, introduced a style regression. It can self-correct without the user noticing.
- It's a clean composition of existing tools (`golangci-lint`, `ruff`, `eslint`, …) — plumb is just orchestrating them. No new analyser code to write.
- It scales linguistically: every language plumb supports already has a mature offline analyser. The contract is the same shape per language.
- It dovetails with `[edits].strict` and rate-limiting: agents that *care* about quality get richer feedback; those that don't can disable.

**Why `golangci-lint` is the right first analyser for plumb.**

This repo already has `.golangci.yml`, `make lint`, and CI wiring. Integrating the same analyser into plumb's daemon would bring those checks into the agent's inner loop instead of leaving them as a final CI gate.

- It catches issues LSP diagnostics often miss: unchecked errors, ineffective assignments, security footguns, formatting/import drift, dead parameters, complexity spikes, and style regressions.
- It is especially valuable for plumb's risk profile: daemon concurrency, filesystem writes, symlink-aware path handling, rollback paths, and error propagation are all places where lint findings catch real bugs rather than cosmetic nits.
- It gives agents the same standard as human contributors. The repo already says `golangci-lint` runs before every commit; surfacing those findings through plumb makes that contract visible to agents before review.
- It makes CI failures more local. Instead of "write code, push, wait for CI, then discover lint", plumb can tell the agent soon after the edit that the change violated the project's lint policy.
- It keeps quality rules project-specific. `golangci-lint` reads the workspace's checked-in config, so plumb does not need to invent its own Go style policy.

**Daemon-aware design direction.**

Plumb is not a one-shot CLI. The daemon is long-lived, already owns per-workspace state, and can do useful background work without blocking every tool response. That should shape this feature.

- Default design should be **background analysis**: write tools enqueue changed files after a successful write; the daemon coalesces rapid edits and runs the analyser shortly after.
- Tool responses should stay bounded. If fresh findings are already available, append them; if analysis is still running, say so briefly and let the next orientation/status/tool response surface the result.
- Optional **synchronous mode** can exist for users who want strict immediate feedback, but it should be opt-in because `golangci-lint` can be slow on cold caches.
- Findings should be cached per workspace and invalidated by file mtime/content hash so the TUI, `session_start`, and future status/quality views can show the latest known quality state.
- The daemon can warm analyser caches opportunistically after workspace attach, but this must be low-priority and cancellable so it never competes with active tool calls.

**Definition of done — Phase 1 (Go, minimum viable):**

1. New abstraction: `internal/quality/Analyser` interface:
   ```go
   type Finding struct {
       File     string
       Line     int    // 1-based
       Column   int
       Severity Severity // info | warning | error
       Code     string   // e.g. "ineffassign"
       Message  string
       Source   string   // e.g. "golangci-lint"
   }
   type Analyser interface {
       Name() string
       Supports(path string) bool // by extension / project markers
       Analyse(ctx context.Context, files []string) ([]Finding, error)
   }
   ```
2. First analyser: `internal/quality/golangcilint/` — shells out to `golangci-lint run --out-format=json <files>`, parses the JSON, returns Findings. Skips silently if `golangci-lint` isn't on PATH.
3. New config layer (in `[edits]` or new `[quality]` block):
   ```toml
   [quality]
   enabled = false                     # opt-in until proven in real use
   mode = "background"                 # "background" | "sync"
   analysers = ["golangci-lint"]       # opt-in list
   timeout_ms = 2000                   # bound each analyser run
   max_findings_per_file = 5           # don't overwhelm responses
   ```
4. The daemon owns a per-workspace quality runner: a small queue, one active analyser process per workspace, coalescing repeated writes to the same file.
5. `WriteDeps` gains a quality reporter/enqueuer (may be nil). `write_file` / `edit_file` / `transaction_apply` enqueue changed Go files after the post-write-diagnostics poll; sync mode may also wait for the bounded result and append findings immediately.
6. Findings are formatted as a compact "code quality" section and capped per file. Background findings are available to `session_start`, the TUI, and a future `plumb quality` / status view.
7. `plumb config show` displays the resolved `[quality]` block with provenance.
8. Tests:
   - Unit test the parser with a captured `golangci-lint --out-format=json` output sample.
   - Unit test daemon queue coalescing and stale-result invalidation without shelling out.
   - Unit test response formatting and max-findings caps.
   - Integration test (`//go:build integration`): write a file with a known style issue, assert the matching code appears in the response.

**Phase 2 (Python and beyond):**

- `internal/quality/ruff/` for Python (`ruff check --output-format=json`).
- Adapter selection by detected language (already done by `workspacePool.Detect`).
- `analysers.Composite` runs multiple analysers in parallel and merges results.

**Phase 3 (advanced):**

- Async / background: don't block the tool response on slow analysers. Return findings via a follow-up notification or store them for the next call.
- Severity filtering by config.
- Suppression mechanism: agents can pass `quality_ok: true` to silence the section for cases where they intentionally broke a rule (e.g. unused function on a placeholder).
- Per-finding "explain why" — `golangci-lint`'s explanation strings plumbed through.

**Where to start — the discussion to have first:**

- Background vs sync should be a config choice. Recommendation: background by default, sync opt-in for strict workflows.
- How wide is "code quality"? Strict linters (`govet`, `staticcheck`) only, or include style (`gofumpt`, `golines`)? Recommendation: start strict, add style behind a flag.
- Does `[quality].enabled = true` by default? Recommendation: false initially, flip to true in 0.6.0 once it's been used in anger.
- Same severity scale as LSP diagnostics, or distinct? Recommendation: distinct labels (`quality.warn`, `quality.suggestion`) so the agent can tell "I broke the build" apart from "I introduced a style nit".
- Should `transaction_apply` analyse the union of all files (more useful, slower) or each file individually (faster, less context-aware)?
- Where should background findings surface first? Recommendation: append fresh findings to write responses when available, include latest cached findings in `session_start`, then add a dedicated TUI/status view later.

**Watch out for:**

- `golangci-lint` is heavy. First-run on a fresh project can be 30+ seconds. The 2-second `timeout_ms` is a starting point; users with cold caches will hit it. Consider warming the cache on daemon start.
- Do not spawn unbounded lint processes. One active run per workspace is enough; coalesce queued files while a run is active.
- Avoid stale findings after rapid edits. Store the file mtime or content hash with each result and discard findings for older revisions.
- Missing `golangci-lint` should be a quiet, explainable skip, not a failed write.
- `ruff` is fast (~10ms typical). Don't apply gopls-tier timeouts to ruff and vice versa.
- Findings can be very noisy in legacy codebases. Without per-file caps (`max_findings_per_file`) the response will balloon.
- This is the kind of feature that's transformative when it works and infuriating when it doesn't (false positives = agent corrects perfectly good code into worse code). Roll out behind a feature flag.

---

### Plumb Topology: Persistent Semantic Indexing

**Priority:** ⭐ top architectural priority.
**Effort:** Significant (multi-week).
**Status:** Planning.
**Discussion:** Derived from `codegraph` research. This feature adds a "structural map" layer to Plumb to solve startup latency, language breadth, and context-density gaps.

**The pitch — speed, breadth, and instant context.**

Plumb currently relies on live Language Servers (LSPs) for all semantic queries. While precise, LSPs are heavy, slow to boot, and limited to a few languages. **Plumb Topology** implements a persistent, disk-based semantic graph using **Tree-sitter + SQLite + FTS5**.

Why this is a priority:
- **Instant discovery:** Agents can query the project structure immediately on attach, without waiting for LSP indexing.
- **Universal breadth:** Tree-sitter grammars cover far more languages than Plumb's current validated LSP adapters.
- **Efficient outline replacement:** `list_symbols`, `find_symbol`, and workspace symbol discovery can use Topology when LSP is unavailable or still warming up.
- **Context density:** `topology_explore` can return a bounded symbol neighbourhood — callers, callees, imports, related tests, routes, and source snippets — in one tool call.
- **Bridge to memory:** Topology gives memory a stable entity layer. Memories can attach to symbols/routes/tests, not only path globs.

**The balance: speed vs. authority.**

Topology must be fast and memory-efficient, but its contract is different from LSP:

- Topology is broad, persistent, and approximate. It answers "what exists?", "how is it connected?", "what might be affected?", and "what context should I inspect first?"
- LSP remains authoritative for surgical semantics: precise definitions, references, renames, type-aware edits, and diagnostics.
- When both sources are available, tool responses should expose the source/confidence: `source=topology`, `source=lsp`, or `source=merged`.
- Topology must degrade cleanly: if the index is absent, stale, or partial, return a clear status and a bounded partial answer rather than pretending to be complete.

**Implementation plan.**

1. **Storage:** SQLite backend in `<workspace>/.plumb/topology.db`.
   - `nodes`: files, packages/modules, symbols, routes, tests, config entry points.
   - `edges`: defines, imports, calls, references, inherits/implements, contains, route-to-handler, test-covers.
   - FTS5 virtual table for fuzzy symbol/path/search queries, with separate indexed fields for symbol names, signatures, paths, comments/docstrings, routes, and test names.
   - Code-aware tokenisation: split `camelCase`, `snake_case`, `kebab-case`, package/path segments, and preserve exact names so `workspacePool`, `workspace pool`, and `workspace_pool` can all be discovered.
   - Dependency: this is the code-structure backend for [Workspace Search Engine](#workspace-search-engine-exact-scan--indexed-discovery); Topology owns indexed code entities, while the search engine owns the user-facing exact-vs-ranked tool contract.
   - Metadata table: schema version, index generation, indexed file hash/mtime, language, extractor version, last error.
   - WAL mode and short write transactions so MCP read queries do not block on indexing.
2. **Extraction:** Go-native Tree-sitter integration. Use checked-in `.scm` queries per language.
   - Phase 1: Go and Python.
   - Phase 2: TypeScript/JavaScript, Java, Rust, Ruby, Swift based on user demand.
   - Store file content hash/mtime with extracted rows so stale data can be discarded.
3. **Resolution:** Pragmatic, confidence-scored resolution.
   - Import tracing + local name matching first.
   - Framework-specific patterns later: Express/FastAPI routes, test naming conventions, CLI command registration, Cobra command trees.
   - Every inferred edge carries a confidence/source marker so agents can distinguish "known" from "likely".
4. **Incremental sync:** daemon-owned background indexer.
   - Debounced file watcher queue.
   - Handles create/update/delete/rename.
   - Cleans stale nodes and edges when files disappear.
   - Manual resync command/tool for recovery.
   - Per-workspace one-indexer-at-a-time lock; coalesce repeated writes.
5. **Tools:**
   - `topology_status`: index health, indexed/skipped/stale file counts, DB size, last sync, watcher state, language coverage, last errors.
   - `topology_search`: fuzzy global symbol/file/route search over indexed code structure. This is a dependency for `workspace_search`, not a replacement for exact `search_in_files` scans.
   - `topology_explore`: bounded neighbourhood around a symbol/file/route/test.
   - `topology_impact`: transitive dependency and reference closure.
   - `topology_routes`: framework-aware entry points.
   - `topology_affected`: given changed files/symbols, return likely affected files and tests.

**Context-budget contract.**

Topology tools must not accidentally dump a huge graph into the conversation. `topology_explore` and `topology_impact` should require or default these controls:

```json
{
  "depth": 2,
  "max_nodes": 50,
  "max_bytes": 30000,
  "include_source": "snippets",
  "budget": "compact"
}
```

Supported `include_source`: `none`, `signatures`, `snippets`, `full` (full should be opt-in and capped). Supported `budget`: `compact`, `normal`, `deep`. Responses should say when results were truncated and how to narrow.

**Definition of done — Phase 1.**

1. `internal/topology` package with SQLite schema for nodes, edges, FTS5 search, and index metadata.
2. Daemon-owned incremental indexer with debounce, stale cleanup, delete/rename handling, and manual resync.
3. Go and Python extractors functional and tested against fixtures.
4. `topology_status`, `topology_search`, and `topology_explore` exposed as MCP tools and documented with clear `source=topology`, `mode=ranked`, and index-freshness semantics.
5. `topology_explore` enforces `max_nodes`/`max_bytes` and reports truncation.
6. Benchmark: Topology-based symbol listing is >5x faster than LSP-based `list_symbols` on cold start.
7. Concurrency tests prove MCP read queries do not fail while indexing is active.

**Phase 2.**

1. Add `topology_impact`, `topology_routes`, and `topology_affected`.
2. Add TypeScript/JavaScript extractor and route patterns.
3. Add topology-backed fallbacks to `list_symbols`, `find_symbol`, and `workspace_symbols` when LSP is unavailable.
4. Add status visibility in TUI/doctor: index health, stale state, and last indexing error.

**Watch out for.**

- SQLite write contention can show up as `database is locked` if index writes hold transactions too long. Use WAL mode, short writes, context timeouts, and retryable reads.
- Tree-sitter resolution is not type checking. Do not use it for semantic rename or edit correctness.
- Framework inference can become a swamp. Start with simple, confidence-scored patterns and keep them optional.
- Keep extractors deterministic. Index output should not change unless source files or extractor versions change.
- Index DBs can grow quietly. Track size in `topology_status` and plan retention/compaction before large workspaces become painful.

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
**Dependencies:** [Plumb Topology](#plumb-topology-persistent-semantic-indexing) provides indexed code entities; [Advanced Memory Engine](#advanced-memory-engine) provides indexed memories. This item defines the user-facing search contract so overlapping search tools do not confuse MCP clients.

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

### Client-aware token-savings model

**Priority:** high for TUI/stats credibility.
**Effort:** Medium. Mostly stats modelling, documentation, and UI wording; no protocol change required.
**Status:** Current implementation is a rough static estimate. Needs redesign before treating the number as a product metric.

**The problem.**

The current `Tokens Saved` widget is useful as a lightweight directional signal, but the model is too simple:

```go
tokens_saved = alternative_tokens - (output_bytes / 4)
```

`alternative_tokens` is currently a static per-tool constant in `internal/stats/savings.go`, and several tools are hard-coded to zero savings (`list_files`, `find_files`, `search_in_files`, `file_diff`, `git`). This means the widget moves for semantic/LSP tools like `list_symbols`, `explain_symbol`, and `workspace_symbols`, but not for filesystem/search tools even when those tools may have saved a specific client from dumping much more context.

The deeper issue: **fallback cost depends on the client.** A Claude Desktop user, a Claude Code user, a Gemini CLI user, and a Codex user do not have the same baseline capabilities.

Examples:

- **Claude Desktop** often has weaker direct repo/file/shell ergonomics. Plumb filesystem and semantic tools can save substantial context versus broad file/resource dumps.
- **Claude Code** and **Codex CLI** can operate in a local development environment, read files, propose or apply edits, and run commands depending on approval mode. For these clients, `read_file`, `git`, and `search_in_files` may save fewer tokens than they do for Claude Desktop, while semantic/LSP tools (`list_symbols`, `find_references`, `call_hierarchy`) still save context because the alternative is often multiple shell/file calls plus reasoning over raw text.
- **Gemini CLI** needs its own observed profile rather than inheriting Claude Desktop or Codex assumptions.
- The same tool can have different value by argument/result shape: `list_symbols` on a 40-symbol file is not the same as `list_symbols` on a tiny file; `search_in_files` with 2 matches is not the same as a broad grep that avoids multiple follow-up reads.

**Codex-specific notes.**

OpenAI's Codex CLI is a local coding agent that can read and modify files and, depending on approval mode, run commands in the terminal. That means the Codex fallback profile should assume strong local file/shell access, not a Claude Desktop-style weak filesystem baseline. The savings model for Codex should reward plumb most for semantic compression and structured context, not for merely replacing `cat`, `rg`, or `git`.

Starting Codex profile recommendation:

| Tool family | Codex fallback assumption | Initial savings stance |
|---|---|---|
| `read_file` / `list_files` / `git` | Codex can usually do these locally with low overhead. | Zero or near-zero. |
| `search_in_files` / `find_files` | Codex can often use shell search, but plumb still adds bounded, structured, ignored-file-aware output. | Low, shape-dependent. |
| `list_symbols` / `workspace_symbols` | Shell fallback requires reading/parsing files or combining multiple commands. | Medium/high. |
| `find_references` / `get_definition` / `explain_symbol` | Shell fallback is approximate and often needs follow-up reads. | High when output is compact. |
| `call_hierarchy` / `type_hierarchy` | Hard to reproduce with plain shell/file access. | High. |
| write tools | No clear token alternative; value is safety, not token savings. | Usually zero; maybe track separately as "safety actions". |

**Claude Code-specific notes.**

Claude Code has a `Read` tool, `Edit`, `Write`, and a `Bash` tool that can run `grep`, `find`, `git`, `rg`, `go vet`, and arbitrary shell commands. This makes it closer to Codex than to Claude Desktop for filesystem/VCS tools, but LSP-backed tools are still highly valuable because the shell alternative requires multiple round-trips and in-context reasoning.

Claude Code sends `{"name": "claude-code", "version": "X.Y.Z"}` in the MCP `initialize` clientInfo. Match on the name prefix `"claude-code"` (version suffix varies by release).

Starting Claude Code profile recommendation:

| Tool | CC alternative | Suggested savings stance |
|---|---|---|
| `read_file` | CC has a native `Read` tool — direct equivalent | Zero |
| `list_files` / `find_files` | `Bash: find .` or `rg --files` | Near-zero (plumb adds gitignore bounds) |
| `search_in_files` | `Bash: rg` / `grep` | Low — gitignore-aware bounded output avoids some follow-up reads |
| `git` | `Bash: git` directly | Zero |
| `file_diff` | `Bash: diff` | Zero |
| `diagnostics` | `Bash: go vet`, `golangci-lint` | Near-zero (plumb gives structured output but CC can run the same commands) |
| `list_symbols` | Must `Read` full file, then reason about structure | Medium-high — scales with file size |
| `find_symbol` | Must `Read` full file | Medium |
| `workspace_symbols` | Multiple `Bash: grep` + several `Read` calls across workspace | High — hard to replicate accurately |
| `get_definition` | `Bash: grep -n` + `Read` surrounding lines | Medium |
| `explain_symbol` | `Read` file + in-context reasoning | Medium |
| `find_references` | `Bash: grep -rn` + `Read` context around each hit | High — scales with reference count |
| `call_hierarchy` / `type_hierarchy` | Many `grep` + `Read` + substantial in-context reasoning | Very high — essentially irreplaceable without LSP |
| `rename_symbol` | CC could do cross-file find-replace, but semantic safety is the value | Zero tokens; track separately as "safety action" if at all |
| Write tools | No token alternative | Zero |

**Design direction.**

Replace the single static `altCost` table with a client-aware model:

```go
type SavingsModel interface {
    TokensSaved(call stats.Call) int
}

type ClientProfile struct {
    Name string // claude-desktop, claude-code, codex, gemini, unknown
    ToolCosts map[string]ToolCostModel
}
```

The model should use `stats.Call.ClientName` / `ClientVersion` (or the existing client fields on session rows if call rows need extending) to select a profile. Unknown clients should use a conservative default, not the most flattering numbers.

Recommended formula:

```text
tokens_saved =
  estimated_fallback_tokens(client_profile, tool, args, output, workspace_context)
  - estimated_plumb_tokens(output)
```

Where:

- `estimated_plumb_tokens` should still be simple initially (`output_bytes / chars_per_token`), but centralise `chars_per_token` and document it.
- `estimated_fallback_tokens` should be per-client and per-tool, with optional shape modifiers.
- If the result is negative, clamp to zero.
- If confidence is low, either report zero or mark the estimate as low-confidence in docs/diagnostics.

**Shape modifiers worth considering.**

- `output_bytes`: already stored; larger output reduces savings.
- Tool arguments: line ranges, query text, `max_results`, `context_lines`, glob/path narrowness.
- Result shape: number of symbols, references, diagnostics, files, matches, hierarchy nodes.
- Workspace scale: file count / language / first-party vs dependency path.
- Follow-up avoidance: a `find_references` response with source lines may avoid several `read_file` calls; a `list_symbols` response may avoid reading the entire file just to discover structure.
- Cache effects: repeated identical calls should probably not claim full savings every time unless they prevented repeated context dumps.

**Definition of done — Phase 1 (honest model + docs).**

1. Add `client_name` and `client_version` to `stats.Call` and the `tool_calls` table. This is the prerequisite for every other item — without client identity at the row level, the model can only use the static fallback for all calls. Concrete steps: add fields to `stats.Call`; add them to the `INSERT` in `Record()`; wire `clientName` from the `stateMu`-guarded copy into `OnAfterTool`'s `Call` literal; add migration v6 (`ALTER TABLE tool_calls ADD COLUMN client_name TEXT NOT NULL DEFAULT ''` and `client_version`). Do not rely on session JSON for historical rows — the join is fragile.
2. Rename UI/documentation wording to **Estimated Tokens Saved** wherever practical. Keep the compact TUI label if space is tight, but docs and help text must say "estimated".
3. Document the calculation in README or `docs/mcp-tools.md`: formula, current profiles, zero-savings tools, and caveats. Document `charsPerToken = 4` with its basis (rough English/code average, GPT-3 tokeniser).
4. Add a `SavingsModel` abstraction under `internal/stats/` and move the current static table behind a default profile.
5. Add client profiles for at least:
   - `claude-desktop`
   - `claude-code`
   - `codex`
   - `gemini`
   - `unknown`
6. Keep profile constants conservative and explain their basis. A lower but defensible estimate is better than a large number users cannot trust.
7. Update `plumb stats`, TUI footer, and top `Tokens Saved` widget to use the same model.
8. Unit tests:
   - Same tool/output produces different estimates for Claude Desktop vs Codex where expected.
   - Zero/negative savings clamps to zero.
   - Unknown client uses conservative default.
   - Filesystem/search tools can be low/zero for Codex but non-zero for Claude Desktop if the profile says so.
   - TUI windowed total and all-time stats total agree with the same model over the same rows.

**Phase 2 (shape-aware estimates).**

- Parse stored `input_json` / `output_text` for result counts where cheap and stable.
- Add per-tool shape functions, e.g.:
  - `list_symbols`: fallback scales with approximate source lines or symbol count.
  - `find_references`: fallback scales with reference count and whether source lines are included.
  - `search_in_files`: fallback can be non-zero for clients without strong local search, but low for Codex/Claude Code.
  - `diagnostics`: fallback differs by client and language because some clients can run build/test/lint locally.
- Add tests with representative stored calls rather than only output byte counts.

**Phase 3 (calibration and user-configurable profiles).**

- Add a debug/report command or stats view that shows savings by client profile and tool so bad estimates are visible.
- Compare estimates against real session transcripts: how many `read_file` / shell calls did plumb actually avoid after semantic calls?
- Expose profile constants as a `[savings]` block in the global config so users can override built-in estimates for any client they use. This allows teams with strong intuitions about their own workflow (e.g. "we know our Claude Code sessions never use Bash search, so `search_in_files` is worth more for us") to tune the numbers without forking the binary. Example shape:

  ```toml
  [savings.claude-code]
  list_symbols   = 1200   # override built-in estimate
  find_references = 900
  search_in_files = 0     # or non-zero if the team avoids Bash search
  ```

  The `[savings.<client>]` key should match the normalised client name. Missing keys fall back to the built-in profile; a missing profile block falls back to the `unknown` default. This makes the model fully auditable: `plumb config show` can print the resolved savings table alongside the rest of the config.

**Watch out for.**

- Do not present the number as billing telemetry. It is an estimate of avoided context, not actual model tokens billed.
- Be careful with marketing pressure: inflated numbers will make the TUI feel untrustworthy.
- Client names may vary (`codex`, `Codex CLI`, `claude-code`, versioned identifiers, etc.). Normalise names by lowercasing and matching on the name prefix before version in one place; test all known aliases. Claude Code sends `"claude-code"` as `clientInfo.name`; Codex typically sends `"codex"`.
- Codex and Claude Code can both run local commands, but their approval/sandbox modes change fallback cost. If mode is not observable through MCP client info, keep the profile conservative.
- `charsPerToken = 4` is already a named constant in `internal/stats/savings.go`. When adding the `SavingsModel` abstraction, expose it as a documented package-level constant rather than burying it. The number is a rough average; if a user-configurable override is added later, this is the knob.
- Historical rows may lack client identity (they will have empty `client_name` after the migration). Treat empty `client_name` as `"unknown"` and apply the conservative default profile; do not backfill from session files unless the join is trivially cheap.
- Keep the model deterministic. The TUI should not fluctuate because of background heuristics changing without new calls.

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

---

### `session_start` orientation — richer entry-point guidance for agents

**Priority:** medium-high — first call shapes the entire session quality.
**Effort:** Small. Additive changes to `session_start.go` response text; no new infrastructure.
**Status:** Partial. Items 1, 3, 4 remain. (Item 2 — Claude Code tool guidance — shipped in 0.6.5; see `docs/todo-to-review.md`.)

**The problem.**

`session_start` currently returns: workspace, language, branch, recent commits, recently-modified files, memories, top-5 tool stats, active diagnostics, and (for Claude Code) a tool guidance section. That is a solid orientation packet, but it leaves several gaps:

1. **No suggested next tool.** An agent arriving at an unfamiliar codebase has no signal for "start with `workspace_symbols` to discover structure" vs "start with `list_symbols` on a specific file" vs "start with `search_in_files`". The session packet could include a short recommended-first-step suggestion based on what it knows: if recent commits modified specific files, suggest examining those files; if the language is resolved and LSP is up, suggest `workspace_symbols`; if LSP is unavailable, suggest `list_files`.
3. **No summary of available memory.** If the workspace has saved memories, the packet mentions them by name but does not surface their content. An agent that doesn't read memories in the first turn tends not to read them at all.
4. **No workspace scale signal.** File count, rough codebase size, and primary language file count would help agents calibrate whether to do broad workspace searches or narrow file-level exploration.

**Definition of done:**

1. `session_start` response includes a `recommended_start` field: one sentence explaining the best first move given current workspace state.
2. If the workspace has memories, append a summary of each memory's description (not full body) to prompt the agent to read relevant ones.
3. Add a `workspace_scale` field: approximate file count and primary-language file count (from `list_files` result or cached filesystem stat).
4. All new fields are additive — existing callers that ignore unknown fields are unaffected.
5. Unit-tested with mock workspace state; integration-tested against a real plumb session.

**Watch out for:**

- `session_start` is already the largest response in a typical session. New fields must be concise — no prose paragraphs, no redundant explanations. Structured lists, not narrative.
- The `tool_guidance` block should not be generated for every client type on every call — gate it behind `clientInfo.name` and make it suppressible via config for users who want a minimal response.

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

This section is the authoritative plan for raising plumb's own code quality to the standard the project claims (AGENTS.md: "best code engineer", good Go practices, ~400 lines/file, gocyclo, gofumpt, lint-before-commit). It **supersedes** items 14 and 15 of [`docs/cli-and-core-review-plan.md`](cli-and-core-review-plan.md) (shared-helper extraction and large-file splitting) — track that work here.

**Objective baseline (golangci-lint v2.12.2). CQ-1 and CQ-2 shipped in 0.6.6 — see `docs/todo-to-review.md` for detail.**

```
Post CQ-1 + CQ-2 (2026-05-20):
  51 findings on ./...
  gocyclo: 37   (functions over the configured min-complexity 15 gate — deferred to CQ-3)
  gosec:   14   (SQL concat, path traversal, int overflow, file perms, subprocess — deferred to CQ-5)

Original baseline (pre CQ-1/CQ-2, 2026-05-20):
  79 total — gocyclo 36, gosec 13, unused 5, staticcheck 8, prealloc 5,
             errcheck 3, gofumpt 3, unparam 3, ineffassign 2, revive 1
```

8 non-test source files exceed the project's own ~400-line guidance: `internal/tui/dashboard.go` (892), `internal/cli/daemon.go` (832), `internal/stats/db.go` (705), `internal/mcp/server.go` (600), `internal/cli/setup.go` (556), `internal/lsp/protocol/types.go` (535), `internal/tools/edit_file.go` (528), `internal/cli/doctor.go` (518). Several more are 480–503.

**Root cause (the engineering problem, not just the symptom).** The recurring anti-pattern is the monolithic `Tool.Execute()`: a single method that decodes raw args, validates them, performs LSP/filesystem work, formats output, and maps errors — all inline. That is why 36 functions blow the complexity gate and why the files are huge. The fix is a *structural standard*, not piecemeal nibbling. The local enforcement gap compounds it: `.git/hooks/` does not exist in this clone, so `make install-hooks` was never run and unlinted/non-compiling code has reached the tree.

This section is ordered by priority. P0 items are mechanical, low-risk, and should land first to stop the bleeding; P1 is the real refactor; P2 makes regressions impossible.

---

### CQ-3 — Decompose the monolithic `Execute()` methods (P1, the core refactor)

**Priority:** ⭐ this is the actual "good software engineering" ask. Discuss the standard, then execute per-tool.
**Effort:** Significant. Phased, one tool per commit.
**Status:** In progress (0.7.0). CQ-6 standard agreed. `SearchInFiles.Execute` decomposed (2026-05-20). `findReplaceTool.Execute` decomposed (2026-05-20). `TransactionApply.Execute` decomposed (2026-05-20). `FindFiles.Execute` decomposed (2026-05-20). `EditFile.Execute` decomposed (2026-05-20). `WriteFile.Execute` decomposed (2026-05-20). `SessionStart.Execute` decomposed (2026-05-20). `computeEditScript` + `groupHunks` in diff.go decomposed (2026-05-20). `ListFiles.Execute` decomposed (2026-05-20). `ListDirectory.Execute` decomposed (2026-05-20). `symbolKindName` map lookup (2026-05-20). `ReadSymbol.Execute` decomposed (2026-05-20). `executePartial` in edit_file.go decomposed (2026-05-20). `readContentMaybeRanged` in read_file.go decomposed (2026-05-20). `RenameFile.Execute` decomposed (2026-05-20). `(*CallHierarchy).Execute` decomposed (2026-05-20). `(*RenameSymbol).Execute` decomposed (2026-05-20). `(*TypeHierarchy).Execute` decomposed (2026-05-20). `walkDir` decomposed (2026-05-20). All `internal/tools` violations resolved — zero gocyclo findings in that package. `(*Server).Serve` decomposed (2026-05-20). `applyEnv` decomposed (2026-05-20). `runStats` decomposed (2026-05-20). `Discover` decomposed (2026-05-20). `runDiagOnWorkspace` decomposed (2026-05-20). `runDaemon` decomposed (2026-05-20). `runConfigShow` decomposed (2026-05-20). All non-TUI violations resolved — zero gocyclo findings outside `internal/tui/`. `handleMainKey` decomposed (2026-05-20). `updateInner` decomposed (2026-05-20). `handleLogSectionKey` decomposed (2026-05-20). `handlePopupKey` decomposed (2026-05-20). `render` decomposed (2026-05-20). `dashActivityGraphLines` decomposed (2026-05-20). `renderPopup` decomposed (2026-05-20).

**Problem.** 36 functions exceed gocyclo 15. The worst are tool `Execute()` methods that interleave five concerns:

| Function | Cyclomatic complexity |
|---|---|
| `(*SearchInFiles).Execute` | ~~74~~ **done (0.7.0)** |
| `(Model).handleMainKey` (TUI) | ~~59~~ **done (0.7.1)** |
| `(*findReplaceTool).Execute` | ~~58~~ **done (0.7.0)** |
| `(*TransactionApply).Execute` | ~~44~~ **done (0.7.0)** |
| `(Model).updateInner` (TUI) | ~~41~~ **done (0.7.1)** |
| `handleConn` (daemon) | ~~38~~ **done (0.7.0)** |
| `(*FindFiles).Execute` | ~~35~~ **done (0.7.0)** |
| `(*EditFile).Execute` | ~~33~~ **done (0.7.0)** |
| `(*Server).Serve` | ~~32~~ **done (0.7.0)** |
| `(*SessionStart).Execute` | ~~31~~ **done (0.7.0)** |
| `(Model).handleLogSectionKey` | ~~30~~ **done (0.7.1)** |
| `(Model).handlePopupKey` | ~~25~~ **done (0.7.1)** |
| `(Model).render` | ~~24~~ **done (0.7.1)** |
| `(Model).dashActivityGraphLines` | ~~22~~ **done (0.7.1)** |
| `(Model).renderPopup` | ~~21~~ **done (0.7.1)** |
| `applyEnv` (config) | ~~28~~ **done (0.7.0)** |
| `runStats` (cli) | ~~28~~ **done (0.7.0)** |
| `computeEditScript` (diff) | ~~27~~ **done (0.7.0)** |
| `(*WriteFile).Execute` | ~~26~~ **done (0.7.0)** |
| `groupHunks` (diff) | ~~16~~ **done (0.7.0)** |
| `(*ListFiles).Execute` | ~~20~~ **done (0.7.0)** |
| `(*ListDirectory).Execute` | ~~20~~ **done (0.7.0)** |
| `symbolKindName` (find_symbol) | ~~18~~ **done (0.7.0)** |
| `(*ReadSymbol).Execute` | ~~18~~ **done (0.7.0)** |
| `executePartial` (edit_file) | ~~18~~ **done (0.7.0)** |
| `readContentMaybeRanged` (read_file) | ~~17~~ **done (0.7.0)** |
| `(*RenameFile).Execute` | ~~17~~ **done (0.7.0)** |
| `walkDir` (walk) | ~~17~~ **done (0.7.0)** |
| `(*CallHierarchy).Execute` | ~~16~~ **done (0.7.0)** |
| `(*RenameSymbol).Execute` | ~~16~~ **done (0.7.0)** |
| `(*TypeHierarchy).Execute` | ~~16~~ **done (0.7.0)** |

**The standard to adopt (see CQ-6).** Every `Tool.Execute()` becomes a thin orchestrator over four named, individually testable steps:

```go
func (t *Foo) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
    args, err := parseFooArgs(raw)        // decode + shape validation only
    if err != nil { return "", err }
    if err := args.validate(); err != nil { return "", err }
    res, err := t.run(ctx, args)          // the actual domain logic, no formatting
    if err != nil { return "", err }
    return formatFooResult(res), nil      // presentation only
}
```

**Definition of done.**

1. Agree the decomposition pattern in AGENTS.md (CQ-6) with one worked before/after example.
2. Refactor worst-first, **behaviour-preserving**, one function per commit, existing tests must stay green (strengthen them where the split exposes a seam): `SearchInFiles.Execute` → `findReplaceTool.Execute` → `TransactionApply.Execute` → `FindFiles.Execute` → `EditFile.Execute` → `WriteFile.Execute` → `SessionStart.Execute` → the remaining `internal/tools` Execute methods → `daemon.handleConn` → `mcp.Server.Serve` → `config.applyEnv` → `cli.runStats`/`runConfigShow`/`runDiagOnWorkspace`/`Discover`.
3. TUI key handling (`handleMainKey` 59, `updateInner` 41, `handleLogSectionKey` 30, `handlePopupKey` 24, `handleMouseWheel` 17): replace the giant `switch msg.String()` chains with a table-driven keymap (`map[key]func(*Model) tea.Cmd` or a small dispatch slice) per focus context. This collapses complexity and makes keybindings self-documenting.
4. After each refactor, the touched function passes gocyclo 15. End state: **zero gocyclo findings, zero exceptions list** for first-party code (test helpers may stay if justified).

**Watch out for.** This is the high-value, high-risk item. Each commit must be a pure refactor with no observable change — diff the tool's response on representative inputs before/after. Do not bundle the split with bug fixes; if the split reveals a bug (likely in `transaction.go`/`find_replace.go` rollback paths), file it as a separate item and fix it in its own commit. Tool responses are an API contract for agents — exact output bytes matter.

---

### CQ-4 — Split oversized files by responsibility (P1)

**Priority:** high — directly answers "I got source code with > 3000 lines". (The 2947-line `internal/tui/model.go` was split on 2026-05-20; this item finishes the job for the rest.)
**Effort:** Medium. Mechanical extraction, separate commits, no behaviour change.
**Status:** Not started.

**Definition of done — split each by clear responsibility:**

1. `internal/cli/daemon.go` (832) → daemon lifecycle / `handleConn` connection handler / control socket / spawn + singleton-lock logic.
2. `internal/stats/db.go` (705) → schema + migrations / read queries / write path. (Coordinate with CQ-5's SQL-concat fix.)
3. `internal/mcp/server.go` (600) → stdio transport+framing / request dispatch / lifecycle. (Coordinate with the `bufio.Scanner` limit item in cli-and-core-review-plan.md §7.)
4. `internal/cli/setup.go` (556) → one file per client (claude-desktop, claude-code, codex, gemini) + shared config-merge helper.
5. `internal/tools/edit_file.go` (528) and `internal/tools/file_write_helpers.go` (503) → naturally fall out of CQ-3.
6. `internal/tui/dashboard.go` (892), `internal/cli/doctor.go` (518), `internal/tui/model_logs.go` (498), `internal/tools/search_in_files.go` (483) → split along the seams CQ-3 introduces.
7. **Documented exception list** in AGENTS.md for files where a single unit is correct: `internal/lsp/protocol/types.go` (535) is a protocol type catalogue mirroring the LSP spec — splitting it harms readability. The ~400-line rule gets an explicit, short allowlist rather than being silently ignored.

**Watch out for.** Pure file moves only — no logic edits in the same commit (those are CQ-3). Keep package-private symbols package-private; do not export something just to move it. One file per commit so `git log --follow` and bisect stay useful.

---

### CQ-5 — Triage and resolve the 13 gosec findings (P1, security)

**Priority:** high — security posture is part of "good practices", and plumb is an agent-facing daemon (prompt-injection → tool-call is a real threat model).
**Effort:** Medium — mostly analysis; a few are real fixes.
**Status:** Item 1 (G202 SQL concat) resolved in 0.6.9 — confirmed not a real injection bug, annotated with justification. Items 2–6 not started.

**Problem.** 13 gosec findings (16 as of 0.6.8 due to new code). Some are real, some are taint-analysis heuristics on already-validated paths. Each must end as *fixed* or *justified-and-annotated*, never unexplained.

**Definition of done — per finding, decide fix vs. justified suppression:**

1. ~~**SQL string concatenation — `internal/stats/db.go:357, 428, 499` (G202).**~~ **RESOLVED in 0.6.9.** All three are false positives — `filter.where()` always uses `?` placeholders. Annotated with `//nolint:gosec` + explanatory comment.
2. **File permissions G306 — `internal/memory/store.go:121`, `internal/session/session.go:128`, `internal/tools/edit_apply.go:85`.** `edit_apply` writes user source files where 0644 is correct (preserve perms). Memory/session files are plumb-owned metadata — decide whether 0600 is appropriate. Fix where it's metadata; document + annotate where 0644 is intentional.
3. **Path traversal G703 — `internal/cli/setup.go:415`, `internal/tools/file_write_helpers.go:387`, `internal/tools/txlog/txlog.go:213`.** Plumb's entire write model is path validation + per-path locks + symlink resolution. Confirm each flagged path passes through the existing validation, then add a one-line `//nolint:gosec // path validated by <fn> — see safeWrite contract` referencing the guarantee. (Cross-check with cli-and-core-review-plan.md §6 symlink-lock-key item.)
4. **Integer overflow G115 — `internal/lsp/protocol/types.go:363` (int→int32), `internal/tools/symbol_edits.go:67` (int→uint32), `internal/tools/search_in_files.go:471` (int→uint32), `internal/tui/dashboard.go:760` (int→rune).** LSP positions in pathological huge files. Add explicit bounds checks before conversion; return a clear error instead of silently wrapping. Small, real correctness fix.
5. **G602 slice index — `internal/cli/setup.go:551`**, **G204 subprocess — `internal/lsp/supervisor.go:231`** and **`internal/tools/find_replace.go:357`** (LSP command and formatter cmd come from resolved config/extension lookup, expected). Verify bounds / input provenance and annotate with justification.
6. End state: `golangci-lint run ./...` shows zero unexplained gosec findings; every remaining suppression has a one-line reason pointing at the safety invariant.

**Watch out for.** Do not bulk-`//nolint` the gosec linter. The whole point is that #1 might be a real injection bug hiding among heuristic noise — each one gets eyes.

---

### CQ-7 — De-duplicate CLI/TUI presentation helpers (P2)

**Priority:** medium. Subsumes cli-and-core-review-plan.md §14.
**Effort:** Medium.
**Status:** Not started.

**Problem.** Path contraction, age/duration formatting, padding, diagnostic-box rendering, and table styling are reimplemented across `internal/cli/{stats,config,sessions,diagnostics,doctor}.go` and partially duplicated in `internal/tui`. Divergent copies drift (e.g. age formatting already differs subtly between CLI and TUI).

**Definition of done.**

1. Introduce `internal/render` (or `internal/textui`) with the shared, pure helpers: path contraction, human-age, padding/truncation, diagnostic box, common table style.
2. Migrate CLI commands first, then TUI, to the shared implementation. No behaviour change beyond intended unification; add snapshot tests where output stability matters (CLI is a UX contract).
3. Do not couple CLI to TUI internals beyond shared style constants already intentionally shared.

**Watch out for.** Respect the layering rule in AGENTS.md — presentation helpers must not pull domain/transport packages upward. Keep the new package leaf-level.

---

### Suggested sequencing

1. **CQ-2** (delete dead code) and the mechanical half of **CQ-1** (gofumpt/ineffassign/prealloc/errcheck) — fast, stops the bleeding, makes diffs clean for everything after.
2. **CQ-5 #1** (SQL concat) early — it may be a real injection bug.
3. **CQ-6** (write the standard) before CQ-3 — the refactor needs an agreed target shape.
4. **CQ-3** worst-first, one function per commit (search_in_files → find_replace → transaction → …).
5. **CQ-4** file splits, falling naturally out of CQ-3's seams.
6. Remaining **CQ-5** gosec triage; **CQ-1** finish (gocyclo reaches zero as CQ-3 lands); **CQ-7** dedup.
7. Flip CI lint to blocking and the pre-commit hook to mandatory once `make verify` is green.

**Whole-section definition of done:** `make verify` green on `./...`; zero gocyclo findings (no first-party exceptions); no non-test file over ~400 lines except the documented allowlist; every gosec finding fixed or justified; pre-commit hook installed and CI lint blocking. Each completed CQ item moves to `docs/todo-to-review.md` with a `CHANGELOG.md` entry, per the workflow at the bottom of this file.

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
