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

## Improvements

Refinements to existing behaviour. No new contracts, no new infrastructure — just better defaults or more flexibility.

---

### Java adapter (jdtls) — multi-OS polish and validation

**Priority:** medium — acceptable as experimental, needed before "validated".
**Effort:** Small–medium. Mostly portability fixes and CI wiring.
**Status:** Adapter works on macOS with Homebrew + SDKMAN. Not yet validated on Linux or Windows.

Known gaps to address before promoting from experimental to validated:

1. **`rootURI` construction.** `internal/cli/pool.go` builds `rootURI := "file://" + root`. On Unix absolute paths this is correct (`/project` → `file:///project`). On Windows it produces the wrong form (`C:\project` → `file://C:\project`). The fix is a proper `pathToFileURI(path string) string` helper in `internal/lsp/protocol/types.go` that uses `filepath.ToSlash` and prepends a leading `/` for Windows drive paths. All three adapters (gopls, pyright, jdtls) use the same construction and would benefit from the fix.

2. **CI integration test.** The `//go:build integration` test in `internal/lsp/adapters/jdtls/` skips silently in CI because no runner installs jdtls. Add a CI step (Ubuntu, using the Eclipse JDT LS release tarball or a package manager) and run `go test -tags=integration -timeout=3m ./internal/lsp/adapters/jdtls/`. Promote the adapter to **validated** once this passes in CI.

3. **Cold-start latency.** jdtls starts a JVM and loads Eclipse plugins on first run; the integration test uses a 60-second deadline. In CI this may not be enough on cold runners. Monitor and raise the timeout if needed, or pre-warm the JVM cache in the CI step.

4. **`jdtls` binary name on non-Homebrew installs.** The compiled default is `command = "jdtls"`. On Linux/Windows the launcher may be named differently (e.g. `jdtls.sh`, `jdtls.bat`, or a full path). Document this in `docs/adding-an-lsp.md` and consider a `command` override example in the config docs. Users can already override via `[lsp.java] command = "..."` in config.toml.

5. **`plumb doctor` Java runtime version check.** The check calls `java --version` and parses the first output line. This covers OpenJDK and GraalVM. Confirm it also handles Eclipse Temurin, Microsoft Build of OpenJDK, and Amazon Corretto version strings; add test cases in `doctor_test.go` once that file exists.

**Definition of done:** CI integration test passes on Linux; rootURI helper lands in `internal/lsp/protocol`; adapter doc.go says "Validated".

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
