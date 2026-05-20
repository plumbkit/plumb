# Plumb — Outstanding Work

Canonical index of known gaps, deferred work, and subtle footguns. Each entry carries enough context that another session can pick it up cold and execute.

Last reviewed against: **0.6.6** (2026-05-20). A full code-quality pass was added on 2026-05-20 — see [Code quality & engineering practices](#code-quality--engineering-practices).

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

### `edit_file` — opt-in partial apply mode

**Priority:** low — the current all-or-nothing behaviour is correct for most cases.
**Effort:** Small. New boolean parameter; separate apply-and-collect-errors loop.
**Status:** Idea only.

When an `edit_file` call includes multiple edits and one fails (`old_str` not found, or ambiguous), the entire batch is rolled back. This is the right default. However, for large refactor batches where most edits are independent, an agent may prefer to apply the successful edits and receive a per-edit error report, then retry only the failures.

**Proposed API:** `apply_partial: true` on the `edit_file` input. When set:
1. Apply each edit independently in sequence.
2. On failure, record the error and continue with remaining edits.
3. Return a per-edit result list: `{edit_index, status: "applied"|"failed", error?, line_range?}`.
4. Still append post-write diagnostics at the end.

**Watch out for:** partial application breaks the atomicity guarantee that makes `edit_file` safe for concurrent agents. Document clearly that `apply_partial` is incompatible with strict mode's "consistent state" assumption and is never valid inside `transaction_apply`.

---

### `find_replace` — opt-in post-write formatter hook

**Priority:** low — most bulk replacements do not need formatting.
**Effort:** Small. Run the workspace's configured formatter on modified files after replacement.
**Status:** Idea only.

`find_replace` rewrites files with raw text substitution. If the replacement changes indentation or import grouping (e.g. renaming `lsp.LSPClient` → `lsp.Client` affects gofumpt's import block layout), the modified files may fail subsequent lint checks. Today the agent must manually run `golangci-lint run --fix` after a bulk `find_replace`.

**Proposed API:** `format_after: true` on the `find_replace` input. When set, run the workspace's configured formatter (`gofumpt` for Go, `ruff format` for Python, etc.) on each modified file. Append a "formatted N files" note to the response.

**Watch out for:** detect the formatter from the workspace language config and `.golangci.yml` — do not hardcode `gofumpt`. If the formatter is not found or errors, report the formatting failure as a warning and still return the replacement results.

---

### `search_in_files` — LSP-backed enclosing symbol for each match

**Priority:** medium — especially valuable for clients without native filesystem access (Claude Desktop).
**Effort:** Medium. One LSP `textDocument/documentSymbol` query per matched file; cache per file per search call.
**Status:** Idea — not started.

When `search_in_files` returns a match inside a source file, it shows the matched line and surrounding context. That context is often insufficient to understand *where* the match sits — the agent must read further up to find which function contains it. For Claude Desktop, which has no native `Read` tool, that costs a full additional `read_file` round-trip.

**Proposed API:** `include_enclosing_symbol: true` (default false) on the `search_in_files` input. When set, for each matched file, call `list_symbols` and find the deepest symbol whose range contains the match line. Include the symbol name and kind in the result:

```
internal/tools/transaction.go:123:  uris = append(uris, uri)
  [in: func (*TransactionApply).Execute]
```

This is most valuable for:
- **Claude Desktop** — no filesystem access; reading the file to find the enclosing method costs a full `read_file`.
- **Any client** — understanding "this call is inside `handleConn`" without reading 832 lines of `daemon.go`.

**Watch out for:** one LSP round-trip per distinct matched file — keep opt-in. If the LSP is unavailable, silently omit the enclosing symbol. Never re-query for multiple matches in the same file within one search call.

---

### Claude Desktop: model plumb as the *only* tool surface, not the best one

**Priority:** high — affects documentation, the savings model, error messages, and `session_start` tool guidance.
**Effort:** Small (documentation and session_start); medium (savings model update — see Architecture item).
**Status:** Gap identified. Not started.

Claude Desktop has no access to OS tools. It cannot run `grep`, `find`, `git`, `rg`, or any shell command. It has no native `Read`, `Edit`, or `Write` tools. For Claude Desktop, plumb tools are not a *better* alternative — they are the *only* interface to the filesystem and codebase.

This has implications not yet reflected in documentation, the savings model, or error messages:

1. **Savings model** (see Architecture): the current model computes "tokens saved vs alternative" but for Claude Desktop there is no alternative. For this client, savings are better expressed as **capabilities enabled** (zero capability without plumb, not expensive capability). The `claude-desktop` profile should be modelled accordingly.
2. **`session_start` tool guidance**: the block generated for Claude Code should have a distinct Claude Desktop variant — simpler: "all file, search, git, and symbol operations must go through plumb; there are no native alternatives."
3. **Error messages in write tools**: messages that suggest "use your native file tools" as a recovery path are wrong for Claude Desktop. Errors should guide the agent to retry, check the daemon, or use a different plumb tool.
4. **Documentation**: `docs/mcp-tools.md` should note which clients have no native fallbacks. This helps tool authors reason correctly: if plumb is unavailable, Claude Desktop is completely blocked, not just slower.

**Definition of done:**
1. `session_start` generates a `tool_guidance` block for `claude-desktop` clients (detect via `clientInfo.name`).
2. Write-tool error messages audited for "use your native tools" suggestions; replaced with daemon-focused recovery.
3. `docs/mcp-tools.md` documents the Claude Desktop no-fallback constraint.
4. The `claude-desktop` savings profile in the Architecture item is modelled as "capability enabled" rather than "tokens saved vs alternative."

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

8 non-test source files exceed the project's own ~400-line guidance: `internal/cli/daemon.go` (832), `internal/stats/db.go` (705), `internal/mcp/server.go` (600), `internal/cli/setup.go` (556), `internal/lsp/protocol/types.go` (535), `internal/tools/edit_file.go` (528), `internal/cli/doctor.go` (518), `internal/tui/dashboard.go` (508). Several more are 480–503.

**Root cause (the engineering problem, not just the symptom).** The recurring anti-pattern is the monolithic `Tool.Execute()`: a single method that decodes raw args, validates them, performs LSP/filesystem work, formats output, and maps errors — all inline. That is why 36 functions blow the complexity gate and why the files are huge. The fix is a *structural standard*, not piecemeal nibbling. The local enforcement gap compounds it: `.git/hooks/` does not exist in this clone, so `make install-hooks` was never run and unlinted/non-compiling code has reached the tree.

This section is ordered by priority. P0 items are mechanical, low-risk, and should land first to stop the bleeding; P1 is the real refactor; P2 makes regressions impossible.

---

### CQ-3 — Decompose the monolithic `Execute()` methods (P1, the core refactor)

**Priority:** ⭐ this is the actual "good software engineering" ask. Discuss the standard, then execute per-tool.
**Effort:** Significant. Phased, one tool per commit.
**Status:** Not started. Needs the CQ-6 standard agreed first.

**Problem.** 36 functions exceed gocyclo 15. The worst are tool `Execute()` methods that interleave five concerns:

| Function | Cyclomatic complexity |
|---|---|
| `(*SearchInFiles).Execute` | **74** |
| `(Model).handleMainKey` (TUI) | **59** |
| `(*findReplaceTool).Execute` | **58** |
| `(*TransactionApply).Execute` | 44 |
| `(Model).updateInner` (TUI) | 41 |
| `handleConn` (daemon) | 38 |
| `(*FindFiles).Execute` | 35 |
| `(*EditFile).Execute` | 33 |
| `(*Server).Serve` | 32 |
| `(*SessionStart).Execute` | 31 |
| `(Model).handleLogSectionKey` | 30 |
| `applyEnv` (config) | 28 |
| `runStats` (cli) | 28 |
| `computeEditScript` (diff) | 27 |
| `(*WriteFile).Execute` | 26 |
| …20 more between 16 and 24 | |

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
6. `internal/cli/doctor.go` (518), `internal/tui/dashboard.go` (508), `internal/tui/model_logs.go` (498), `internal/tools/search_in_files.go` (483) → split along the seams CQ-3 introduces.
7. **Documented exception list** in AGENTS.md for files where a single unit is correct: `internal/lsp/protocol/types.go` (535) is a protocol type catalogue mirroring the LSP spec — splitting it harms readability. The ~400-line rule gets an explicit, short allowlist rather than being silently ignored.

**Watch out for.** Pure file moves only — no logic edits in the same commit (those are CQ-3). Keep package-private symbols package-private; do not export something just to move it. One file per commit so `git log --follow` and bisect stay useful.

---

### CQ-5 — Triage and resolve the 13 gosec findings (P1, security)

**Priority:** high — security posture is part of "good practices", and plumb is an agent-facing daemon (prompt-injection → tool-call is a real threat model).
**Effort:** Medium — mostly analysis; a few are real fixes.
**Status:** Not started.

**Problem.** 13 gosec findings. Some are real, some are taint-analysis heuristics on already-validated paths. Each must end as *fixed* or *justified-and-annotated*, never unexplained.

**Definition of done — per finding, decide fix vs. justified suppression:**

1. **SQL string concatenation — `internal/stats/db.go:357, 428, 499` (G202).** Verify each concatenation interpolates only internal constants (column names, sort keys, bucket counts) and never a user/agent-supplied value. If constants only: replace with a small allowlisted column/order builder and add a comment explaining the invariant. **If any concatenates a filter value (workspace, session id, tool name): that is a real SQL-injection bug — fix with `?` placeholders.** This is the highest-priority gosec item; treat as a potential bug, not a nit.
2. **File permissions G306 — `internal/memory/store.go:121`, `internal/session/session.go:128`, `internal/tools/edit_apply.go:85`.** `edit_apply` writes user source files where 0644 is correct (preserve perms). Memory/session files are plumb-owned metadata — decide whether 0600 is appropriate. Fix where it's metadata; document + annotate where 0644 is intentional.
3. **Path traversal G703 — `internal/cli/setup.go:415`, `internal/tools/file_write_helpers.go:387`, `internal/tools/txlog/txlog.go:213`.** Plumb's entire write model is path validation + per-path locks + symlink resolution. Confirm each flagged path passes through the existing validation, then add a one-line `//nolint:gosec // path validated by <fn> — see safeWrite contract` referencing the guarantee. (Cross-check with cli-and-core-review-plan.md §6 symlink-lock-key item.)
4. **Integer overflow G115 — `internal/lsp/protocol/types.go:363` (int→int32), `internal/tools/symbol_edits.go:67` (int→uint32).** LSP positions in pathological huge files. Add explicit bounds checks before conversion; return a clear error instead of silently wrapping. Small, real correctness fix.
5. **G602 slice index — `internal/cli/setup.go:551`** and **G204 subprocess — `internal/lsp/supervisor.go:231`** (LSP command comes from resolved config, expected). Verify bounds / input provenance and annotate with justification.
6. End state: `golangci-lint run ./...` shows zero unexplained gosec findings; every remaining suppression has a one-line reason pointing at the safety invariant.

**Watch out for.** Do not bulk-`//nolint` the gosec linter. The whole point is that #1 might be a real injection bug hiding among heuristic noise — each one gets eyes.

---

### CQ-6 — Codify and enforce the engineering standard (P2, anti-regression)

**Priority:** medium-high — without this, CQ-1…CQ-5 decay back.
**Effort:** Small (documentation + hook), but it is the keystone.
**Status:** Not started.

**Problem.** AGENTS.md already states the policy (≈400 lines/file, gofumpt, lint-before-commit, no globals, context-first, error wrapping, comments only when non-obvious). The gap is **enforcement and a concrete pattern**, not policy text. New code keeps reproducing the monolithic-`Execute` anti-pattern because there is no documented blueprint and no local gate.

**Definition of done.**

1. AGENTS.md: add a "Tool implementation pattern" subsection with the `parseArgs / validate / run / format` blueprint from CQ-3 and one real before/after example. New tools must follow it; PRs/commits that add a monolithic `Execute` are non-conforming.
2. AGENTS.md: state the gocyclo-15 contract explicitly and the file-size rule with its short exception allowlist (CQ-4).
3. Pre-commit hook (`scripts/pre-commit`, installed via `make install-hooks`) runs `go build ./...`, `gofumpt -l` (fail if non-empty), `golangci-lint run`. Document `make install-hooks` as a **required** first step after clone in AGENTS.md "Build commands".
4. Add `make verify` (CQ-1) and reference it as the definition of "ready to commit".
5. Optional: a tiny CI check that fails if any non-test, non-allowlisted `.go` file exceeds N lines, so the size rule is mechanically enforced rather than honour-system.

**Watch out for.** Keep the hook fast enough that contributors do not bypass it with `--no-verify`. If `golangci-lint` cold runs are slow, scope the hook to changed packages and leave the full `./...` sweep to `make verify`/CI.

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

### `rename_symbol` fails with stale LSP position index after in-session edits

**Priority:** medium — silent failure mode causes a confusing error and wastes a tool call; the fallback (manual `find_replace`) is slow.
**Status:** Known; no fix attempted.

When `rename_symbol` is called after earlier edits in the same session, the LSP's position index may be stale. The language server has received `workspace/didChangeWatchedFiles` but has not finished re-indexing. The tool fails with a cryptic message such as:

```
applying edits to foo.go: edit start position out of range: line N char M
```

The underlying cause (position drift due to unsynchronised edits) is not surfaced. The agent must fall back to `find_replace` for the qualified name plus manual fixes for bare-name references in comments.

**Workaround:** after editing files that contain the symbol, wait for a `diagnostics` call to confirm the LSP has re-indexed before calling `rename_symbol`.

**Definition of fix:**
1. Detect "position out of range" results from `textDocument/rename` and return a clear error: "LSP position index may be stale after recent edits — check `diagnostics` to confirm re-indexing, then retry `rename_symbol`."
2. Optionally: send a `textDocument/didOpen` + `textDocument/didClose` pair on the target file before the rename request to flush the LSP's in-memory position cache.
3. Unit test: mock LSP returns an out-of-range edit after a prior edit; verify the error message is surfaced cleanly.

---

### `gofumpt` standalone binary and `golangci-lint` embedded formatter disagree silently

**Priority:** low — only affects contributors who run `gofumpt -w` directly; CI and `golangci-lint run --fix` agree.
**Status:** Known; documented here for future contributors.

The standalone `gofumpt` binary (e.g. v0.10.0 from `go install`) and the `gofumpt` formatter embedded in `golangci-lint` v2.12.2 produce different output on the same files. Running `gofumpt -w <file>` on a file that `golangci-lint run` flags as unformatted does not resolve the finding; a subsequent `golangci-lint run` may flag a *different* set of files.

**Root cause:** `golangci-lint` pins its own formatter version internally and the two can drift independently.

**Workaround:** always use `golangci-lint run --fix ./...` to apply formatting — never the standalone `gofumpt -w`.

**Definition of fix:**
1. Add a note to AGENTS.md under "Build commands": "`gofumpt -w` may disagree with the formatter embedded in `golangci-lint` — use `golangci-lint run --fix ./...` to apply formatting reliably."
2. CQ-6's pre-commit hook should invoke `golangci-lint run --fix` (not `gofumpt -l`) so contributors get the right formatter automatically.

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
