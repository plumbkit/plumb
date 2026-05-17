# Plumb — Outstanding Work

Canonical index of known gaps, deferred work, and subtle footguns. Each entry carries enough context that another session can pick it up cold and execute.

Last reviewed against: **0.5.29** (2026-05-16).

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

If you have ~2 hours of work to invest, do these in this order:

1. **Pyright integration smoke test** (~20 min, once `pyright-langserver` is installed) — same shape as the gopls test that landed in 0.5.6. Highest-value confidence boost. See [Testing & verification > Pyright integration smoke test](#pyright-integration-smoke-test).

2. **Claude Desktop end-to-end test** (~30 min, no code) — connect real Claude Desktop to current binary, run the `orient` prompt, write a file via `edit_file`, check that diagnostics come back. See [Testing & verification > Claude Desktop end-to-end smoke test](#claude-desktop-end-to-end-smoke-test).

Total: ~45 min. After items 1 and 2, plumb is *proven* (not just claimed) production-ready against both supported LSPs and the primary client.

---

## Architecture

Deep design changes, contract changes, and new infrastructure. These are the items most likely to need design discussion before implementation.

### Code-quality differential after edits

**Priority:** ⭐ top architectural priority. Discuss before implementing.
**Effort:** Significant (multi-day, possibly multi-week). Phased delivery makes sense.
**Status:** Idea captured. Not started.

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

### Transaction durable rollback log

**Priority:** medium-low.
**Effort:** 3–4 hours including tests.

**Why this matters.** `transaction_apply`'s rollback is **best-effort**. The current implementation: if a write in phase 2 fails, we iterate over the already-written files and call `safeWrite(path, p.before, p.perm)` to restore. If that restoration itself fails (disk full, permission revoked, fs went read-only between phase-2 writes), we log an error and the file stays in its post-write state.

For an editor-class tool, this is acceptable. For "production data" use cases — anywhere a partial transaction could leave a system in a corrupt state — it isn't.

The fix is a tiny on-disk WAL.

**Definition of done.**

1. New package `internal/tools/txlog` (or similar) with `Begin(workspace, txID) (*Log, error)`, `Record(path, beforeContent, perm)`, `Commit()`, `Rollback()`.
2. `Begin` creates `.plumb/tx-log/<txID>/`, writes a `manifest.json` listing the planned operations.
3. `Record` writes each pre-edit content to `.plumb/tx-log/<txID>/<n>-before` before phase 2 starts the corresponding write.
4. `Commit` removes `.plumb/tx-log/<txID>/`.
5. `Rollback` reads each `<n>-before` and `safeWrite`s it back to the original path. Best-effort on each; logs the rest.
6. `transaction_apply` calls `Begin` at the start of phase 2, `Record` for each prepared op, `Commit` on success, `Rollback` on failure.
7. On daemon startup, `txlog.Scan(workspace)` finds orphaned `.plumb/tx-log/*` directories (= daemon crashed mid-transaction) and completes their rollback.
8. Tests:
   - Happy path: transaction commits, tx-log dir is gone.
   - Mid-transaction failure: tx-log dir survives, contains expected snapshots, rollback restores all files.
   - Crash simulation: write tx-log + partial files manually, run `txlog.Scan`, confirm restoration.
   - Concurrent transactions on disjoint paths: each gets its own txID dir; no interference.

**Where to start.** Start with the package interface; build the simpler `Begin/Record/Commit/Rollback` flow first. Add `Scan` only once the basic path works. The startup scan in `daemon.go` is one line: `txlog.Scan(workspace)` immediately after the workspace resolves.

**Watch out for:**

- Cleanup of `.plumb/tx-log/` on success has to be reliable — if it's left behind it'll trigger a phantom rollback on next startup. Use `os.RemoveAll`.
- Snapshot file size cap: a transaction touching a 100 MiB file would duplicate 100 MiB to the tx-log. Worth either capping per-file or rejecting transactions where any operation exceeds some size.
- The user can manually inspect `.plumb/tx-log/` to recover from a crash plumb couldn't.

---

### Features

Net-new user-facing capabilities. Lower architectural risk than the Architecture section — these mostly compose existing primitives.

### Token Usage Optimization — Automatic Diffing & Truncation

**Priority:** high.
**Effort:** Significant (multi-step).

**Why this matters.** Agents often re-read files to verify changes or dump too much data into the context (logs, grep results). Plumb can solve this at the tool level.

**Definition of done:**
1. **Automatic Diffing:** `edit_file` and `write_file` return a unified diff of the change in the response. This gives the agent immediate confirmation of the change without requiring a fresh `read_file` turn.
2. **Smart Truncation:** Large tool outputs (especially `search_in_files` and `git log`) are automatically capped (e.g., at 100 lines). The response includes a summary ("Showing 100 of 450 matches") and instructions on how to page or narrow the search.
3. **Implicit Verification Mode:** A configuration option to suppress full output and return only high-signal metadata for repetitive tasks.

---

### "Working tree is dirty" guard before plumb-initiated writes

**Priority:** medium.
**Effort:** 1–2 hours (depending on chosen approach).

**Why this matters.** Plumb will happily edit a file that has uncommitted changes the user hasn't reviewed. If the agent goes off the rails, the user can't easily distinguish "what I wrote" from "what plumb wrote on the agent's behalf". `git stash` recovers the file, but only if the user noticed in time and the stash hasn't been overwritten.

**Three options, listed least-disruptive first. Pick one before starting:**

1. **`dirty_ok: bool` parameter, default `false`.** Each write tool checks `git status --porcelain <path>` for the target. If output is non-empty, refuse with a clear error unless `dirty_ok=true`. Minimal surprise. Doesn't require any persistent state.
2. **Append a notice to the tool output.** "Note: foo.go had uncommitted changes before this edit. Previous content is recoverable via `git stash` if needed." Non-blocking; informational. Easier for the agent to ignore (might be the right outcome — agents are working on user behalf).
3. **Snapshot to `.plumb/snapshots/<sha>` before every write.** Heavy. Real undo log. Closest to what an editor does. Pairs naturally with the transaction durable log. Worth doing only if option 1 turns out to be too restrictive in practice.

**Definition of done (assuming option 1):**

1. New helper in `internal/tools/file_write_helpers.go`: `pathIsDirty(path string) (bool, error)` runs `git status --porcelain --` against the file's containing git repo, returns true if there's a non-empty result.
2. `write_file`, `edit_file`, `delete_file`, `rename_file`, `transaction_apply` all accept `dirty_ok bool` (default false). Each calls `pathIsDirty` and refuses if true and `dirty_ok=false`.
3. The error message tells the agent what to do: "foo.go has uncommitted changes; review and commit, or pass `dirty_ok: true` if you intend to overwrite".
4. Tests:
   - File outside any git repo → not dirty (no error, write proceeds).
   - Clean file in a git repo → not dirty.
   - File with uncommitted modifications → dirty (refused).
   - With `dirty_ok=true` → proceeds anyway.

**Where to start.** `internal/tools/file_write_helpers.go` for the helper. Each write tool adds the parameter to its `args` struct and the check to its `Execute`. Update tool schemas.

**Watch out for:**

- `git status` is slow on huge repos (>10 s on a kernel-size repo). Cache the result per-call by `filepath.Dir(path)` to avoid running it once per edit in a transaction.
- `pathIsDirty` needs to handle: not a git repo (`err`); file inside `.gitignore` (clean); newly-added file (dirty).
- Don't shell out to `git` if `git` is not on `$PATH` — return false silently and let the write proceed.

---

### TUI: Live Log Viewer with Real-time Filtering

**Priority:** low (nice to have)
**Effort:** 3–5 hours

**Why this matters.** Currently, debugging Plumb or an agent's interactions requires tailing `daemon.log` in a separate terminal. Bringing a live, filterable log view into the TUI (via a new tab) creates a unified developer experience. 

**Architectural Note: Zap vs. Zerolog vs. slog**
When implementing this, you might consider migrating to a third-party JSON logging framework like Uber's **Zap** or **Zerolog**.
*   **Zerolog** is excellent for fluent, JSON-only pipelines and would make parsing easy.
*   **Zap** is the industry standard for raw speed in high-throughput servers.
*   **Recommendation: Use neither.** Introducing either framework violates Plumb's current lightweight, dependency-minimal architecture. More importantly, Go's standard library `log/slog` already supports high-performance JSON output natively via `slog.NewJSONHandler`. Sticking with `slog` keeps the binary small and avoids a massive refactoring of existing log calls, while fully enabling the TUI to parse structured JSON.

**Definition of done:**
1. The daemon's log output format is updated to write structured JSON (`slog.NewJSONHandler`) to `daemon.log` instead of plain text, so the TUI can easily parse the fields. (Alternatively, make the format configurable in `config.toml`).
2. A new Bubble Tea tab is added to the TUI (e.g., `focusLogs`).
3. The TUI model tails `daemon.log` asynchronously and unmarshals incoming JSON lines.
4. A text input field allows real-time fuzzy filtering of logs (e.g., by log level, tool name, or error text).

---

### Session tagging — `purpose` metadata on `session_start`

**Priority:** low.
**Effort:** 1–2 hours.

`session_start` could accept an optional `purpose` string (e.g. `"intune-project-deployment"`) that tags every subsequent tool call in that session. The TUI and stats DB could then display coherent timelines per task, making it easier to audit what the agent did across multi-turn conversations without trawling Claude Desktop history.

**Definition of done:**
1. `session_start` accepts `purpose string` (optional).
2. The purpose is stored in the session info file and in the stats DB alongside each tool call.
3. TUI session list row shows the purpose string (truncated if long).

---

## Improvements

Refinements to existing behaviour. No new contracts, no new infrastructure — just better defaults or more flexibility.

### Project-root identification fails when no language marker is present (auto-attach fallback)

**Priority:** high. User-visible breakage today.
**Effort:** ~3–4 hours including tests and config plumbing.
**Status:** Diagnosed. Design discussion pending — see "Decision points" below.

**The core problem.** Plumb identifies a workspace's *project root* by walking up from a tool call's seed path looking for one of a small fixed set of marker files: `.plumb/`, `go.mod`, `pyproject.toml`, `setup.py`. If none of those exist anywhere up the tree, plumb cannot identify the root — and without a root, the session has no workspace, no stats DB, no project config, no TUI presence. This is the wall that PowerShell, JavaScript/TypeScript, Rust, Java, shell-script, and any other-language project hits the first time it's opened in Claude Desktop without someone having run `plumb init` ahead of time.

The fix below is one possible solution (an auto-attach fallback that synthesises a root from the seed path or nearest git repo); other approaches are possible — see "Decision points." The point of this entry is that **root identification is a known gap, not a bug in any single tool.**

**The symptom.** A Claude Desktop session that drives plumb against a directory with no `go.mod`, no `pyproject.toml`, no `setup.py`, and no `.plumb/` marker stays unattached for its entire lifetime. The session file is registered with `folder=""`, `language=""`, `adapter=""`. Consequences:

- **TUI** shows the session as `⟳ resolving…` forever; no Recent Edits, no Tool Statistics, no useful right-panel data.
- **Stats are silently dropped.** `OnAfterTool` in `internal/cli/daemon.go` short-circuits when `root == ""` (line ~398), so no `.plumb/stats.db` is ever created, no history accumulates.
- **Per-project config never loads.** Global config applies, project-local overrides under `<workspace>/.plumb/config.toml` are unreachable because there is no workspace.
- **LSP notifications fail harmlessly** ("LSP server not yet ready") on every write — logged as WARN noise.

**Concrete repro (2026-05-15 incident).** Claude Desktop session attached to PowerShell project at `/Users/golimpio/Projects/engine/devtool/devtool-intune/windows/live-response/`. Hours of `write_file` / `transaction_apply` calls, all succeed at the filesystem level. `pool.Detect` walks up to `/` without finding any marker, returns error, `acquiredRoot` stays empty. User sees no session in TUI, no stats, no project config — even though plumb is clearly being used.

**Today's behaviour, traced.**

1. `OnInit` → `roots/list` returns "Method not found" (Claude Desktop limitation).
2. `OnBeforeTool` fires on each tool call → extracts seed path → calls `pool.Detect(filepath.Dir(seedPath))`.
3. `pool.Detect` walks up looking for `.plumb/`, `go.mod`, `pyproject.toml`, or `setup.py`. None found → returns `("", "", err)`.
4. `OnBeforeTool` logs `daemon: cannot determine workspace root` and returns. `attachWorkspace` is never called.
5. `acquiredRoot` stays "" forever. The session is a ghost.

User's verbatim request: *"Even without any supported language, it should have created a .plumb file, since the MCP is working in there via Claude Desktop."*

**The fix in one line.** When `pool.Detect` fails inside `OnBeforeTool` (and only there — never inside `route` or LSP-routing paths), fall back to a synthetic workspace root derived from the seed path, and treat the session as attached for everything except LSP. Optionally write `.plumb/` on attach so subsequent sessions resolve via the existing marker path.

**Decision points (need user input before implementing).**

1. **Which directory becomes the synthetic root?** Three plausible strategies, ordered safest → most useful:

   a. **Seed file's parent dir.** Cheapest. Workspace shifts every time you touch a file in a different subdir of the same project. Likely annoying for a real project (would attach 5 sibling workspaces while editing 5 files).

   b. **Walk up to nearest git repo root** (`.git/` marker). Falls back to (a) if no `.git/` found going up. Matches "this project" semantics for most users. Implementation: add `.git` to the marker list in `Detect`, but only as a *last-resort tier* — primary tiers stay as-is so existing behaviour is preserved when a real marker exists. Returns the git root with `language = LanguageNone`.

   c. **Session-cumulative common ancestor.** Track every distinct seed parent seen in this session; the workspace is the longest common path prefix. Best UX but stateful (each new file might shift the workspace upward). Requires careful invalidation if a path lands far outside the current ancestor.

   **Recommendation:** (b) with fallback to (a). (b) is the directory the user mentally calls "this project"; (a) is the safety net when there's no git either. (c) is over-engineered for the v1.

2. **Opt-in or default-on?** Plumb is in production. New behaviour that silently changes where stats are written could surprise existing users. Two options:

   a. **Opt-in via `[workspace] auto_attach = true`** in global/project config. Defaults to `false`. Existing users see zero change unless they enable it. Ship for one or two releases of soak, then flip default to `true`.

   b. **Default-on, opt-out via `auto_attach = false`.** Faster wins for new users but riskier for existing fleets.

   **Recommendation:** (a). The user who reported this can enable it on day one. Default flip is a separate release decision.

3. **Should the `.plumb/` directory be auto-created on disk?** The user's wording ("it should have created a .plumb file") suggests they want this. Two sub-options:

   a. **Yes — create `.plumb/` at the synthetic root the first time the session attaches.** Persistent: next session resolves via the standard marker path with no fallback needed. Side effect: a `.plumb/` directory shows up in the user's project tree (visible to git, possibly creating a diff against committed state).

   b. **No — keep the synthetic root in-memory only.** Stats DB lives at e.g. `~/.local/share/plumb/orphan-stats/<hash>.db` keyed by absolute path. No on-disk pollution of the user's project. Cost: synthetic resolution must repeat every session.

   **Recommendation:** (a) gated behind a second flag (`auto_attach_persist = true/false`). Default to in-memory-only for v1 (option b), let users explicitly opt into the persist behaviour. Re-evaluate the default in a later release.

**Definition of done.**

1. New section in resolved config: `[workspace]` with `auto_attach bool` (default `false`) and `auto_attach_persist bool` (default `false`). Reads from global, project, env (`PLUMB_AUTO_ATTACH`, `PLUMB_AUTO_ATTACH_PERSIST`).
2. `pool.Detect` unchanged. New helper `pool.DetectOrSynthesise(seedDir, strategy)` returns `(root, language, synthetic bool, err)`. `synthetic=true` means the root was inferred, not found via marker.
3. `OnBeforeTool` calls the new helper *only* when `Detect` failed AND `auto_attach` is true. On success, calls `attachWorkspace` with the synthetic root. `language` is `LanguageNone` for the synthetic path.
4. `session.Info` gains `Synthetic bool` field, serialised to the session JSON. TUI displays `(auto)` suffix next to the folder name for synthetic sessions so the user can tell them apart from real ones.
5. Stats store: if `auto_attach_persist` is false, route writes for synthetic workspaces to `~/.local/share/plumb/orphan-stats/<sha256(root)>.db`. If true, use `<root>/.plumb/stats.db` as today and `os.MkdirAll` the `.plumb/` dir at attach time.
6. `plumb config show` displays both `auto_attach` and `auto_attach_persist` with provenance.
7. CLAUDE.md documents the behaviour under "Workspace detection" — explicit about the precedence: `.plumb/` > language marker > (if `auto_attach`) git root > seed parent dir.
8. CHANGELOG entry for the version that ships it.

**Where to start.**

1. `internal/config/config.go`: add `WorkspaceConfig` struct with `AutoAttach`, `AutoAttachPersist`. Wire defaults. Add env-var override path mirroring `EditsConfig`.
2. `internal/cli/pool.go`: add `DetectOrSynthesise`. Use `.git/` walk as the synthetic-root marker (don't pollute the existing `Detect` marker list — keep the synthetic path opt-in).
3. `internal/session/session.go`: add `Synthetic bool` to `Info`.
4. `internal/cli/daemon.go`'s `OnBeforeTool`: add the conditional fallback. The simplest shape:
   ```go
   root, _, err := pool.Detect(startDir)
   if err != nil && workspaceCfg.AutoAttach {
       root, _, synthetic, err = pool.DetectOrSynthesise(startDir, strategy)
   }
   ```
5. `internal/cli/stats_store.go`: add `OrphanRecord(synthRoot, call)` that routes to the orphan-stats DB. `statsStore.Record` checks the session for `Synthetic` and dispatches.
6. `internal/tui/model.go`: display `(auto)` suffix in the session list label when `info.Synthetic`.
7. Tests:
   - `pool_test.go`: `TestDetectOrSynthesise_*` for git-root fallback, seed-parent fallback, both-found-prefer-real.
   - `daemon_test.go`: extend with `TestOnBeforeTool_AutoAttachOff` (silently skips), `TestOnBeforeTool_AutoAttachOn` (attaches synthetic).
   - `stats_store_test.go`: orphan DB created at expected path; survives daemon restart.

**Related code already fixed (don't duplicate).** Today's commit added `seedPathFromArgs` to `internal/cli/daemon.go` which now handles `operations[*].path` (`transaction_apply`) and `paths[*]` (`read_multiple_files`). Before that fix, `transaction_apply` and `read_multiple_files` couldn't even produce a seed for `OnBeforeTool` to work with — even with this auto-attach work in place, the workspace would never resolve for those tools. The seed-extraction fix is a prerequisite that's now done.

**Relationship to `plumb init [--discover]` (already shipped).** `plumb init` is a *manual* CLI command (`internal/cli/init.go`): the user runs it in a terminal, it creates `.plumb/` and seeds `context.md`. It is **not** invoked automatically by the daemon when a tool call lands on an unattached directory. This auto-attach work is the missing automatic counterpart: same end-state on disk (if `auto_attach_persist = true`), but triggered from the daemon's tool-call hook instead of requiring the user to know about `plumb init` ahead of time. When implementing, consider factoring out `init.go`'s `os.MkdirAll(plumbDir) + write context.md template` into a reusable helper (e.g., `plumb.MaterialiseWorkspace(dir)`) that both `runInit` and the daemon's auto-attach path call into, so the on-disk shape stays identical.

**Watch out for.**

- **Don't auto-attach from `route()` in `routing_proxy.go`.** That path runs on every LSP-bound URI and would burn cycles on irrelevant paths. Synthetic attach must only happen on the *first* tool call after a session connects, via `OnBeforeTool`.
- **The synthetic root can be wrong.** A user editing `/Users/me/scratch/quick-edit.txt` from a `$HOME`-rooted Claude Desktop would attach to `/Users/me/scratch/` (if no git) — sensible enough. But editing `/tmp/foo.txt` would attach to `/tmp/`. Document this and let the user override with explicit `plumb init`.
- **macOS `$HOME` walks.** Many users will trigger auto-attach with `$HOME` or `~/Documents` as the de facto seed. The session attaches to whatever is up-tree from the first file touched, which could be a noisy parent dir. The git-repo-first strategy mitigates this for real projects.
- **Persisting `.plumb/` writes to a user's git-tracked directory.** If the user has `auto_attach_persist = true` and edits a file inside their git repo, plumb will create `repo/.plumb/` (and probably `stats.db`) — which will show as untracked in `git status` until they `.gitignore` it. CLAUDE.md should explicitly recommend adding `.plumb/` to global gitignore or per-project gitignore. The `plumb init` command should already do this; verify when implementing.
- **Backwards compatibility.** Existing sessions that DO resolve a workspace via `.plumb/`/`go.mod`/etc. must behave identically. Auto-attach only kicks in on the `Detect` error path. Add a regression test: with marker present, `Synthetic` stays false; with marker absent and flag off, `acquiredRoot` stays "" as today.

---

### `search_in_files` exclude-glob patterns

**Priority:** low-medium.
**Effort:** 1–2 hours.

Agents investigating an issue in a directory with known irrelevant subtrees (e.g. a vendored client folder, generated code, or test fixtures) currently see false leads when `search_in_files` matches inside those subtrees. An `exclude` parameter (string array of glob patterns) would let the agent filter them out, mirroring ripgrep's `--glob='!pattern'` flag.

**Definition of done:**
1. `search_in_files` gains an `exclude []string` parameter; each entry is passed to ripgrep as `--glob='!<pattern>'`.
2. Schema and `docs/mcp-tools.md` updated.
3. Tests: search with an exclude that suppresses a hit; without exclude the hit appears.

---

## Testing & verification

Proving things actually work end-to-end. The unit suite is green; what's missing is *end-to-end confidence in a real environment*.

### Claude Desktop end-to-end smoke test

**Priority:** highest in this section.
**Effort:** 30 min, no code.

**Why this matters.** Plumb was rebuilt across 0.5.x specifically to make Claude Desktop work. None of the new mechanism has been verified against real Claude Desktop:

- `session_start`'s `roots/list` fallback (added in 0.5.1 #3) — Claude Desktop's roots support has historically been spotty; verify it actually responds.
- The cold-start workspace chain — when the daemon launches from `$HOME`, does the cwd walk find anything useful, or does roots/list save us?
- The MCP Prompts (`orient`, `whats-broken`, `recent-changes`) — do they render correctly as Desktop menu items?
- Memory resources (`plumb-memory://`, `plumb://workspace/context`) — do they appear in the resources sidebar?
- Post-write diagnostics in `edit_file` output — does the agent actually see them in its tool response?

**How to test:** wire `plumb setup claude-desktop`, restart Desktop, open a Go project, ask Claude to read + edit a file, watch the daemon log for the relevant calls.

**Definition of done.** A manual checklist run successfully against real Claude Desktop, with results captured in a `docs/claude-desktop-smoke.md` (or appended to this file). The checklist:

1. `plumb stop && make build && plumb setup claude-desktop`. Restart Claude Desktop.
2. Open Claude Desktop. Open a Go project (e.g. this one). Watch `~/Library/Caches/plumb/daemon.log` while the conversation starts.
3. Did the workspace resolve via `roots/list`? The log should show `daemon: session attached`  with the project root, not "no project root found".
4. Type `/orient` (or invoke the Orient prompt manually). Claude should respond with a 3–5 sentence summary including the project's language, branch, and any active diagnostics.
5. Ask Claude to read a small file via `read_file`. The response should begin with `# plumb-read mtime=...`.
6. Ask Claude to edit that file using `edit_file` with the mtime from step 5. The response should include `applied N edit(s)` and a `lines changed: ...` summary.
7. Ask Claude to introduce a syntax error via `edit_file`. Within 300 ms the response should include `diagnostics after write:` with at least one error line. *This is the load-bearing test for the post-write-diagnostics feature.*
8. Open the resources sidebar in Claude Desktop. Confirm `Project context` and any memories are visible.

**Likely failure modes (so you know what to watch for):**

- **Step 3 failing** means Claude Desktop's `roots/list` support is broken or absent for your install. In that case `session_start` falls through to the cwd-walk fallback, which probably won't find anything because Desktop launches the daemon from `$HOME`. The fix is in `internal/cli/daemon.go`'s `rootsFn` / `applyProjectConfig`.
- **Step 7 failing** ("diagnostics after write" never appears) means gopls didn't republish within the configured window (default 300 ms, tunable via `[edits].post_write_diagnostics_ms` in config). Either the window is too short for your machine, or `didChangeWatchedFiles` isn't being consumed. The gopls integration smoke test rules out the latter; if it passes locally with `go test -tags=integration`, raise the window in `.plumb/config.toml`.

---

### Pyright integration smoke test

**Priority:** highest after Claude Desktop.
**Effort:** 20–30 min.
**Prerequisite:** `pyright-langserver` on `$PATH` (`npm install -g pyright`).

**Why this matters.** `TestIntegration_DidChangeWatchedFiles` in `internal/lsp/adapters/gopls/adapter_test.go` proves the 0.5.x architectural rewrite is load-bearing for gopls. The pyright adapter has identical wiring (same `LSPClient` interface, same `DefaultClientCapabilities` declaration, same `handleServerRequest` for `client/registerCapability`), but **the equivalent integration test does not exist**. Until it does, `AGENTS.md` honestly has to keep pyright marked "Experimental" — a structural asymmetry that's purely about test coverage.

**Definition of done.**

1. A test `TestIntegration_DidChangeWatchedFiles` exists in `internal/lsp/adapters/pyright/adapter_test.go`, gated `//go:build integration`.
2. It spawns real `pyright-langserver --stdio`, initialises against a temp workspace populated from `testdata/python-fixture/`, writes a syntactically broken `.py` file, sends `DidChangeWatchedFiles{FileCreated}`, and asserts pyright republishes at least one error diagnostic within 5 seconds.
3. `testdata/python-fixture/` exists with minimum: `pyproject.toml` (or `setup.py`) + `main.py`. If it doesn't exist, create it with one valid Python file.
4. The test runs green with `go test -tags=integration ./internal/lsp/adapters/pyright/...`.
5. AGENTS.md's adapter validation status table updates pyright from "Experimental" to "Validated".

**Where to start.**

1. Copy `internal/lsp/adapters/gopls/adapter_test.go`'s `TestIntegration_DidChangeWatchedFiles` verbatim into `internal/lsp/adapters/pyright/adapter_test.go`.
2. Replace the gopls-specific bits:
   - `startGopls` → `startPyright` (same shape; spawn `pyright-langserver --stdio` instead of `gopls serve`)
   - `gopls.New(conn)` → `pyright.New(conn)`
   - `gopls.DefaultInitParams` → `pyright.DefaultInitParams`
   - Fixture path: `testdata/go-fixture` → `testdata/python-fixture`
   - Broken file content: invalid Python (`def broken( {`)
   - `requireGopls` → `requirePyright` (checks `pyright-langserver` is on PATH; `t.Skip` if not)
3. Run with `go test -tags=integration ./internal/lsp/adapters/pyright/... -run DidChangeWatchedFiles -v`.

**Likely failure modes (and what they mean):**

- **`pyright-langserver: not found`** → install it (`npm install -g pyright`). Test should `t.Skip` cleanly.
- **`gopls did not publish error diagnostics within 5s`** equivalent for pyright → either the `client/registerCapability` handler in `internal/lsp/adapters/pyright/adapter.go` isn't responding (unlikely — same code as gopls), or pyright wants its diagnostics in a different format. Increase the timeout to 15s for a slow first-run and re-check.
- **`testdata/python-fixture/` doesn't exist** → create it. Minimum content: `pyproject.toml` with `[tool.pyright]` empty section, and a `main.py` with `def greet(name: str) -> str: return f"hello, {name}"`.

---

### CI matrix that runs integration tests

**Priority:** high.
**Effort:** 30–60 min (depends on CI provider).

**Why this matters.** The smoke test that proves the architecture works (`TestIntegration_DidChangeWatchedFiles`) is gated `//go:build integration`. If your CI doesn't include this build tag, the load-bearing test never runs in PR checks — only locally, only when someone remembers. A regression that breaks `client/registerCapability` handling would slip through `go test ./...` without complaint and ship to users.

**Definition of done.**

1. CI config (`.github/workflows/*.yml`, `.gitlab-ci.yml`, or whatever you use) has a job that:
   - Installs `gopls` (and `pyright-langserver` once the pyright smoke test lands).
   - Runs `go test -tags=integration ./...` with a per-test timeout of 30s.
   - Fails the PR on any test failure.
2. The job runs on every PR and on every merge to `main`.
3. A `make integration-test` target exists for local convenience and matches what CI runs (so "passes locally" → "passes in CI").

**Where to start.**

1. Look at the existing CI config (probably `.github/workflows/test.yml`). The current setup almost certainly runs `make test` which is `go test ./...`.
2. Add a second job (or expand the existing one) with steps:
   ```yaml
   - name: Install gopls
     run: go install golang.org/x/tools/gopls@latest
   - name: Integration tests
     run: go test -tags=integration -timeout=2m ./...
   ```
3. Add `integration-test:` to `Makefile`:
   ```makefile
   integration-test:
       go test -tags=integration -timeout=2m ./...
   ```

**Watch out for:**

- gopls install in CI takes time; cache it via the standard Go module cache action.
- Some CI runners are slow; the gopls smoke test passes in ~1.2s locally but may need 5s in CI. Bump the deadline in the test if it flakes — better than dropping the assertion.

---

## Bugs & known limitations

Footguns and behaviour to be aware of. None of these are urgent — they are documented here so anyone touching the relevant subsystem can make an informed decision (fix it, work around it, or leave it alone).

### `pathLocks` is permanent process-global state

Every path ever locked by any tool stays in the `sync.Map[string]*sync.Mutex` in `internal/tools/file_write_helpers.go` for the daemon's lifetime. For long-running daemons handling many sessions across many files, this can grow without bound. Not a leak in the GC sense (the mutexes are reachable), but a slow memory creep.

**Why it's not fixed:** in practice, plumb daemons restart often (every `make build && plumb stop && plumb serve`), and the per-path mutex overhead is ~40 bytes plus the map entry. A daemon that touches 100,000 distinct files leaks ~4 MiB. Tolerable.

**When to fix:** if you find someone running a plumb daemon for weeks against a project with millions of unique paths.

**How to fix:** wrap the mutex in a struct with a `lastUsed time.Time`, set it in `lockPath` / on release, run an LRU sweep every 5 minutes that deletes entries idle for more than an hour. The sweep needs to acquire each mutex (with `TryLock`) before deletion to avoid racing with an in-flight lock.

---

### The rate limiter is per-connection, not per-agent

`RateLimiter` is constructed once per `handleConn` in `daemon.go`. A single agent process making 1000 MCP connections in a minute can do 120 writes per connection — effectively unlimited.

**Why it's not fixed:** the threat model is "runaway autonomous loop". A real autonomous loop runs within one MCP session and gets caught by the per-connection limit. The "open 1000 connections to bypass the limit" attack requires coordinating across connections, which a real agent doesn't naturally do.

**When to fix:** if you see the limiter actually being abused, or if you start running plumb in a multi-tenant context where connection counts can be untrusted.

**How to fix:** key the limiter by `ClientName + ClientVersion` (captured by `srv.OnClientInfo` in `daemon.go`) or by the MCP session's client-reported identity, not by Go's per-connection struct. Use a shared `sync.Map[string]*RateLimiter` at daemon scope.

---

### CRLF normalisation in `edit_file` is one-directional toward the file

If the file uses CRLF and `old_str` is LF, plumb normalises `old_str` to CRLF before matching. If the file is LF and `old_str` is CRLF, plumb normalises `old_str` to LF. **Mixed-ending files** — rare but they exist, especially in repos that have travelled through both Windows and Unix toolchains — have undefined behaviour because the "what does the file use?" detection (`strings.Contains(ref, "\r\n")`) only sees the first CRLF, not the proportion.

**Why it's not fixed:** mixed-ending files are an editor-level pathology, not something plumb should encourage. The right answer is probably "run `dos2unix`" or its inverse before letting plumb touch the file.

**Documentation action:** call this out explicitly in `docs/mcp-tools.md`'s `edit_file` section ("if the file has mixed line endings, normalise it first"). The current docs imply CRLF tolerance is comprehensive; it isn't.

---

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

### The 100 ms concurrent-write skew constant is hard-coded

`concurrentWriteDetected` in `internal/tools/file_write_helpers.go` uses `const skew = 100 * time.Millisecond` to decide whether the file's post-rename mtime indicates a third-party write. Too narrow → false negatives (concurrent writes within 100 ms are invisible). Too wide → false positives (we retry edits that didn't actually race).

**Why it's not configurable:** 100 ms is a reasonable default for SSD-backed filesystems where typical rename + stat latency is well under 10 ms. On slow filesystems (network mounts, FUSE) or under heavy load, both thresholds could be wrong in different directions.

**Recommendation:** if you see flaky `concurrent write detected` errors in legitimate workflows, bump the constant. If you see silent corruption from concurrent writes that should have been retried, lower it. Both this constant and the post-write diagnostics window (`[edits].post_write_diagnostics_ms`) share the same "expose-as-config" concern.

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
