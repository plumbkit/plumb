# Plumb — Outstanding Work

Canonical index of known gaps, deferred work, and subtle footguns. Each entry carries enough context that another session can pick it up cold and execute.

Last reviewed against: **0.6.4** (2026-05-18).

When you complete a TODO entry: delete its section, add a `CHANGELOG.md` entry for the version that ships the fix, in the **same commit**. If new gaps surface during the work, add them here in the same commit.

## Organisation

This file is organised by **topic**, not strictly by priority. Within each topic, items are ordered by priority (highest first). A separate ["The next two hours"](#the-next-two-hours) recommended-priority section at the very top cross-cuts the topics.

Topics:

- [Architecture](#architecture) — deep design changes, new contracts, new infrastructure
- [Features](#features) — net-new user-facing capabilities
- [Improvements](#improvements) — refinements to existing behaviour
- [Testing & verification](#testing--verification) — proving things actually work end-to-end
- [Bugs & known limitations](#bugs--known-limitations) — existing footguns; behaviour to be aware of
- [Considered and deferred](#considered-and-deferred) — items decided against or postponed

---

## The next two hours

Run the **Claude Desktop end-to-end smoke test** checklist in `docs/claude-desktop-smoke.md` (~30 min, no code). After that, plumb is *proven* (not just claimed) production-ready against both supported LSPs and the primary client.

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

### Cross-file call graph by symbol name

**Priority:** medium.
**Effort:** Medium. `call_hierarchy` exists; the missing piece is stitching multiple callers recursively and returning a concise graph.
**Status:** Idea only.

**The problem.**

`call_hierarchy` returns the immediate callers and callees of one symbol, one level at a time. Getting a deep picture of "who calls `readProcessMetrics` and who calls those callers" requires iterating `call_hierarchy` manually across potentially many symbols — each at the cost of one tool call.

A `call_graph(path, symbol_name, depth)` tool would recursively expand the hierarchy up to `depth` levels and return a textual tree or adjacency representation.

**Definition of done:**

1. New tool `call_graph` in `internal/tools/call_graph.go`.
2. Inputs: `path`, `symbol_name`, `direction` (`callers | callees | both`), `max_depth` (default 3, max 6).
3. Output: indented tree with symbol names, files, and line numbers.
4. Cycle detection: mark already-visited nodes with `(seen)` and do not recurse further.
5. Result cap: if the expanded graph exceeds 200 nodes, truncate with a summary line and a recommendation to reduce depth.
6. Unit-tested with a mock LSP transport; integration-tested with `//go:build integration`.

**Watch out for:**

- Each level of expansion is one `call_hierarchy` LSP call per unique symbol. Depth 6 on a widely-called function can trigger hundreds of LSP calls. Enforce the node cap and add per-run timeout (configurable, default 10 s).
- The result must name source files, not just symbol names — callers in test files, generated code, or vendor paths may be noise. Consider an `exclude` option that mirrors `search_in_files`.

---

## Improvements

Refinements to existing behaviour. No new contracts, no new infrastructure — just better defaults or more flexibility.

---

### `list_symbols` — include method signatures and parameter types

**Priority:** high — agents routinely need to call a function they just found; without the signature they must read the file.
**Effort:** Small. The LSP `textDocument/documentSymbol` response does not include signatures; the text must be extracted from the source file using the returned line range.
**Status:** Not started.

**The problem.**

`list_symbols` returns names, kinds, and line ranges. It does not return the function or method signature. An agent that wants to call `refreshDashboard` must then read the file at the function's line to learn that the method takes no arguments, or read a different function to learn its parameter types. Each of these is an additional round-trip.

For Go specifically, the first line of every function/method definition is the complete signature. Extracting it requires reading the source line at the symbol's `start_line`.

**Desired output:**

```
Model.renderDashboard     method  L487–590  func (m Model) renderDashboard() string
dashBox                   func    L592–605  func dashBox(titleText string, innerWidth int, contentLines []string) []string
```

**Definition of done:**

1. `list_symbols` gains an optional `include_signatures bool` parameter (default false for backwards compatibility).
2. When true, for each symbol of kind `function` or `method`, extract the first source line of the range and append it as a `signature` field in the output.
3. For Go, the signature is the first non-blank non-comment line at `start_line`.
4. For Python, include the full `def` line.
5. Extraction requires one `read_file` for the file (already done for range resolution — no extra round-trip if cached).
6. Unit-tested; `docs/mcp-tools.md` updated.

**Watch out for:**

- Multi-line signatures (Go interface methods with line-wrapped parameters, Python decorators). Extract only the first line for Phase 1 and document the limitation.
- Performance: reading source lines for every symbol in a large file adds cost. Gate behind the `include_signatures` flag so callers opt in deliberately.

---

### `search_in_files` — performance and first-call cold-start latency

**Priority:** medium-high — first-call latency of 17 s+ disrupts agent flow, even though subsequent calls are faster.
**Effort:** Small–medium. Mostly diagnosis + tuning the ripgrep invocation parameters and process warm-up.
**Status:** Not started.

**The problem.**

First-call latency for `search_in_files` is consistently in the 17–22 second range on the plumb repository itself (a small Go codebase). Subsequent calls to the same or similar patterns are faster, suggesting the bottleneck is cold process startup or filesystem metadata fetching rather than the search itself.

By comparison, `workspace_symbols` with an LSP that has already indexed the workspace answers queries in under 1 second for the same class of "find all uses of this type" question.

**Investigation points:**

1. Measure whether the latency is inside the ripgrep process spawn itself (`exec.CommandContext` overhead) or in the Go wrapper marshalling results.
2. Check whether the daemon pre-spawns or caches the ripgrep path on first call; if not, consider a one-time lookup at daemon start.
3. For known-type and known-symbol lookups, prefer `workspace_symbols` over `search_in_files` in the tool descriptions. Make the guidance explicit so agents understand when each tool is appropriate.
4. Add a fast-path: if the query does not use regex and has no glob constraints, delegate to `workspace_symbols` instead of ripgrep.

**Definition of done:**

1. `search_in_files` first-call p95 latency drops to under 5 s on the plumb repo (or equivalent small Go workspace).
2. Tool descriptions for `search_in_files` and `workspace_symbols` include a guidance note: prefer `workspace_symbols` for symbol name lookups in indexed workspaces; use `search_in_files` for pattern/regex searches across file content.
3. Benchmarks in `internal/tools/search_in_files_test.go` measure first and warm call latency so regressions are visible.

**Watch out for:**

- ripgrep startup overhead is typically < 100 ms on warm systems. If the daemon is genuinely taking 17 s, the bottleneck may be outside ripgrep — check `.gitignore` traversal and whether the wrapper is walking the directory tree before spawning ripgrep, rather than passing the tree to ripgrep directly.
- Do not remove `search_in_files`; it is irreplaceable for regex/pattern searches. Only optimise and re-steer agents towards `workspace_symbols` for name-based queries.

---

### `session_start` orientation — richer entry-point guidance for agents

**Priority:** medium-high — first call shapes the entire session quality.
**Effort:** Small. Additive changes to `session_start.go` response text; no new infrastructure.
**Status:** Not started.

**The problem.**

`session_start` currently returns: workspace, language, branch, recent commits, recently-modified files, memories, top-5 tool stats, and active diagnostics. That is a solid orientation packet, but it leaves several gaps that cause agents to fumble through redundant tool calls in the first few turns:

1. **No suggested next tool.** An agent arriving at an unfamiliar codebase has no signal for "start with `workspace_symbols` to discover structure" vs "start with `list_symbols` on a specific file" vs "start with `search_in_files`". The session packet could include a short recommended-first-step suggestion based on what it knows: if recent commits modified specific files, suggest examining those files; if the language is resolved and LSP is up, suggest `workspace_symbols`; if LSP is unavailable, suggest `list_files`.
2. **No tool-choice guidance for the session client.** The packet does not tell the agent which tools are uniquely valuable for its context. A Claude Code session should be told: "prefer `workspace_symbols` over `Bash: grep`, prefer `find_references` over `Bash: rg -n`" — because without that, the agent defaults to native tools it already knows.
3. **No summary of available memory.** If the workspace has saved memories, the packet mentions them by name but does not surface their content. An agent that doesn't read memories in the first turn tends not to read them at all.
4. **No workspace scale signal.** File count, rough codebase size, and primary language file count would help agents calibrate whether to do broad workspace searches or narrow file-level exploration.

**Definition of done:**

1. `session_start` response includes a `recommended_start` field: one sentence explaining the best first move given current workspace state.
2. When `clientInfo.name == "claude-code"`, append a `tool_guidance` block listing: tools with no native equivalent (lead with these), tools where plumb adds value over the native equivalent, and tools that are redundant (skip these for Claude Code).
3. If the workspace has memories, append a summary of each memory's description (not full body) to prompt the agent to read relevant ones.
4. Add a `workspace_scale` field: approximate file count and primary-language file count (from `list_files` result or cached filesystem stat).
5. All new fields are additive — existing callers that ignore unknown fields are unaffected.
6. Unit-tested with mock workspace state; integration-tested against a real plumb session.

**Watch out for:**

- `session_start` is already the largest response in a typical session. New fields must be concise — no prose paragraphs, no redundant explanations. Structured lists, not narrative.
- The `tool_guidance` block should not be generated for every client type on every call — gate it behind `clientInfo.name` and make it suppressible via config for users who want a minimal response.

---

### Claude Code integration — reduce tool overlap and choice paralysis

**Priority:** medium — friction point unique to Claude Code contexts.
**Effort:** Small. Primarily documentation and tool description wording.
**Status:** Not started.

**The problem.**

When running inside Claude Code, the agent has access to both plumb's tools and Claude Code's native tools (`Read`, `Edit`, `Write`, `Bash`). Many tool capabilities overlap:

| Task | Plumb tool | Claude Code native |
|---|---|---|
| Read a file | `read_file` | `Read` |
| Search for text | `search_in_files` | `Bash: rg / grep` |
| Git status/log | `git` | `Bash: git` |
| Edit a file | `edit_file` | `Edit` |
| List files | `list_files` | `Bash: find` |

This creates **choice paralysis**: when both tools are available, the agent must decide which to use. Without clear guidance, it may choose inconsistently, or worse, choose the native tool when plumb's LSP-backed alternative would provide richer output (e.g., using `Bash: grep` instead of `workspace_symbols` to find a type, then needing two follow-up reads to understand the context).

**Improvements needed:**

1. **Tool descriptions should state explicitly when to prefer plumb over native.** For each tool that overlaps with Claude Code's native toolkit, the description should say "prefer this over `<alternative>` because <reason>". Example: `read_file` description should note that it records mtime for strict-mode compatibility and returns a binary-detection header; `search_in_files` should note it honours `.gitignore` and returns bounded structured output.
2. **Session orientation.** `session_start` already returns tool stats. Consider adding a "tool choice guidance" section for Claude Code clients specifically (detected via `clientInfo.name == "claude-code"`), listing: "for this workspace, prefer X over Y because Z".
3. **Uniquely-valuable tools should be prominent.** The tools with no native equivalent — `workspace_symbols`, `find_references`, `call_hierarchy`, `type_hierarchy`, `rename_symbol`, `replace_symbol_body`, `transaction_apply` — should have descriptions that open with "No native Claude Code equivalent." so agents learn to reach for them first.
4. **Redundant tools for Claude Code may emit a friendly suggestion.** When `read_file` is called and the `clientInfo` indicates Claude Code, the response could include a one-line note: "Consider using the native `Read` tool for files you only need to read; `read_file` is most valuable when strict mode or mtime tracking is needed." (Configurable; off by default.)

**Definition of done:**

1. Tool descriptions updated in `internal/tools/*.go` for the overlap cases above.
2. For tools with no native equivalent, descriptions lead with that fact.
3. `session_start` response includes a one-liner per "prefer plumb for X" guidance when `clientInfo.name == "claude-code"`.
4. No new code paths required; this is description text and one conditional block in `session_start.go`.

---

### Java adapter (jdtls) — multi-OS polish and CI hardening

**Priority:** medium — validated first version, but still needs cross-platform polish.
**Effort:** Small–medium. Mostly portability fixes and CI wiring.
**Status:** Adapter works with a real jdtls binary and Java 21+. Remaining work is portability, CI coverage, and write-tool integration polish.

**Cross-platform note:** current real-binary validation has only been exercised on macOS. Linux and Windows coverage are expected pre-v1 hardening work, not a blocker for the first validated Java adapter version.

Known gaps to address:

1. **`rootURI` construction.** `internal/cli/pool.go` builds `rootURI := "file://" + root`. On Unix absolute paths this is correct (`/project` → `file:///project`). On Windows it produces the wrong form (`C:\project` → `file://C:\project`). The fix is a proper `pathToFileURI(path string) string` helper in `internal/lsp/protocol/types.go` that uses `filepath.ToSlash` and prepends a leading `/` for Windows drive paths. All three adapters (gopls, pyright, jdtls) use the same construction and would benefit from the fix.

2. **CI integration test.** The `//go:build integration` test in `internal/lsp/adapters/jdtls/` skips silently in CI because no runner installs jdtls. Add a CI step (Ubuntu, using the Eclipse JDT LS release tarball or a package manager) and run `go test -tags=integration -timeout=3m ./internal/lsp/adapters/jdtls/`.

3. **Cold-start latency.** jdtls starts a JVM and loads Eclipse plugins on first run; the integration test allows 5 minutes for ServiceReady and a further 2 minutes for diagnostics after DidOpen. In CI on a cold runner this may be tight — monitor and raise the deadline if needed, or pre-warm the JVM cache in the CI step. Set `JDTLS_FRESH_DATA=1` to force a hermetic per-test data directory (slower; default reuses `.testcache/jdtls-data` for warm-cache local runs).

4. **`jdtls` binary name on non-Homebrew installs.** The compiled default is `command = "jdtls"`. On Linux/Windows the launcher may be named differently (e.g. `jdtls.sh`, `jdtls.bat`, or a full path). Document this in `docs/adding-an-lsp.md` and consider a `command` override example in the config docs. Users can already override via `[lsp.java] command = "..."` in config.toml.

5. **`plumb doctor` Java runtime version check.** The check calls `java --version` and parses the first output line. This covers OpenJDK and GraalVM. Confirm it also handles Eclipse Temurin, Microsoft Build of OpenJDK, and Amazon Corretto version strings; add test cases in `doctor_test.go` once that file exists.

6. **Write tools need `DidOpen`/`DidClose` for jdtls diagnostics.** Unlike gopls and pyright, jdtls only publishes diagnostics for open documents. When plumb's write tools call `DidChangeWatchedFiles` after modifying a Java file, jdtls updates its project model but may not emit diagnostics promptly. For the `diagnostics` tool to return up-to-date results after a Java write, the write path should call `DidOpen` (with the new content) + `DidClose` in addition to `DidChangeWatchedFiles`. This requires the write tools to know the current language server type, or a per-adapter hook in the LSP notification path.

**Definition of done:** CI integration test passes on Linux; rootURI helper lands in `internal/lsp/protocol`; write-tool diagnostics path handles Java's `DidOpen`/`DidClose` requirement.

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

### `client/registerCapability` response is null-accepted, not inspected

When gopls registers a watcher (e.g. `{"method": "workspace/didChangeWatchedFiles", "registerOptions": {"watchers": [{"globPattern": "**/*.go"}]}}`), plumb responds `null` (OK) and moves on. We don't track *which* globs were registered. We send `didChangeWatchedFiles` notifications for every file we touch, regardless of whether the server actually asked to watch that pattern.

**Why it's not fixed:** sending extra notifications is harmless — the server ignores files outside its registered globs. gopls in practice registers `**/*.go`, `**/go.mod`, `**/go.sum`, `**/*.work` — matching ~everything we'd write in a Go project anyway.

**When to fix:** if a future LSP server is sensitive to receiving notifications for unregistered files (logs a warning, terminates connection, etc.).

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

1. **Pick up an item:** read its section in full. The acceptance criteria (Definition of done) and the Where to start pointers should be enough to begin without re-deriving the problem.
2. **While working:** if you find a new gap, add it to this file in the same commit as your fix.
3. **When you finish:** delete the section from this file, add the corresponding entry to `CHANGELOG.md` under the version that ships the fix, and commit both changes together.
4. **If you can't finish:** leave the section in place but add a short "Status:" note describing how far you got and what's blocking, so the next person doesn't start from scratch.

The cost of *not* capturing a gap is high — months later, the gap turns into a mystery bug or a confused new contributor. The cost of writing it down is one paragraph. Always favour capturing.
