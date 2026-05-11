# Plumb — Outstanding Work

Canonical index of known gaps, deferred work, and subtle footguns. Each entry carries enough context that another session can pick it up cold and execute.

Last reviewed against: **0.5.7** (2026-05-11).

When you complete a TODO entry: delete its section, add a `CHANGELOG.md` entry for the version that ships the fix, in the **same commit**. If new gaps surface during the work, add them here in the same commit.

---

## The next two hours — recommended priority order

If you have ~2 hours of work to invest, do these in this order. They're the items whose absence undermines the most confidence:

1. **Pyright integration smoke test** (~20 min, once `pyright-langserver` is installed) — same shape as the gopls test that landed in 0.5.6. Highest-value confidence boost: confirms pyright is structurally equivalent to gopls, not just unit-test-equivalent. See [Pyright integration smoke test](#pyright-integration-smoke-test) below.

2. **Claude Desktop end-to-end test** (~30 min, no code) — the whole 0.5.x line was built for Claude Desktop. Connect real Claude Desktop to 0.5.7, run the `orient` prompt, write a file via `edit_file`, check that diagnostics come back in the response. Most important verification that's not been done. See [Claude Desktop end-to-end smoke test](#claude-desktop-end-to-end-smoke-test) below.

3. **Stats `input_json` column + Recent Edits paths in TUI** (~45 min) — includes the schema migrator that should already exist. Unblocks any future "what did Claude actually call?" introspection. See [Stats DB migrator + `input_json` column for tool args](#stats-db-migrator--input_json-column-for-tool-args) below.

4. **`expected_sha` parameter on `edit_file`** (~30 min) — the mtime path stays as the cheap default; the SHA path is for callers that care. Doesn't break existing behaviour. See [`expected_sha` parameter on `edit_file` and `transaction_apply`](#expected_sha-parameter-on-edit_file-and-transaction_apply) below.

Total: ~2¼ hours. After these, plumb is *proven* (not just claimed) production-ready against the two LSPs we support and against the client we built for.

---

## Production-blocking

These are the items whose absence prevents plumb from being claimed as proven, not just compiling-and-passing-unit-tests.

### Claude Desktop end-to-end smoke test

**Priority:** highest.
**Effort:** 30 min, no code.

**Why this matters.** Every 0.5.x feature was built specifically to make Claude Desktop work — the `session_start` cold-start chain, the MCP Prompts, the resource provider, the per-project config. None of it has been confirmed against real Claude Desktop. The unit tests confirm the wire format and the daemon's internal state machine, not the actual user experience.

**Definition of done.** A manual checklist run successfully against real Claude Desktop 0.5.7 or newer, with results captured in a `docs/claude-desktop-smoke.md` (or appended to this file). The checklist:

1. `plumb stop && make build && plumb setup claude-desktop`. Restart Claude Desktop.
2. Open Claude Desktop. Open a Go project (e.g. this one). Watch `~/Library/Caches/plumb/daemon.log` while the conversation starts.
3. Did the workspace resolve via `roots/list`? The log should show `daemon: session attached`  with the project root, not "no project root found".
4. Type `/orient` (or invoke the Orient prompt manually). Claude should respond with a 3–5 sentence summary including the project's language, branch, and any active diagnostics.
5. Ask Claude to read a small file via `read_file`. The response should begin with `# plumb-read mtime=...`.
6. Ask Claude to edit that file using `edit_file` with the mtime from step 5. The response should include `applied N edit(s)` and a `lines changed: ...` summary.
7. Ask Claude to introduce a syntax error via `edit_file`. Within 300 ms the response should include `diagnostics after write:` with at least one error line. *This is the load-bearing test for the post-write-diagnostics feature.*
8. Open the resources sidebar in Claude Desktop. Confirm `Project context` and any memories are visible.

**Where to start.** No code changes expected — this is a verification exercise. If a step fails, file a TODO entry for the specific failure and address it before re-running.

**Likely failure modes (so you know what to watch for):**

- **Step 3 failing** means Claude Desktop's `roots/list` support is broken or absent for your install. In that case `session_start` falls through to the cwd-walk fallback, which probably won't find anything because Desktop launches the daemon from `$HOME`. The fix is in `internal/cli/daemon.go`'s `rootsFn` / `applyProjectConfig`.
- **Step 7 failing** ("diagnostics after write" never appears) means gopls didn't republish within 300 ms. Either the `postWriteDiagWindow` constant in `internal/tools/file_write_helpers.go` is too short for your machine, or `didChangeWatchedFiles` isn't being consumed. The gopls integration smoke test rules out the latter; if it passes locally with `go test -tags=integration`, the issue is timing — see [Configurable post-write diagnostics window](#configurable-post-write-diagnostics-window) below.

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

## Real gaps — meaningful, not blocking

### `expected_sha` parameter on `edit_file` and `transaction_apply`

**Priority:** medium-high.
**Effort:** 30 min.

**Why this matters.** `expected_mtime` is the optimistic-concurrency primitive we have today. It relies on the filesystem reporting mtime honestly, and it doesn't:

- `touch -d` sets mtime arbitrarily.
- Restore-from-backup preserves mtime.
- Same-second writes on coarse-mtime filesystems can yield identical mtime for different content.
- Some `mmap` write patterns don't update mtime.

For honest use, mtime is fine; for any adversarial or replicated scenario, content hashing is what you'd want. A SHA-256 in the `read_file` output header and an optional `expected_sha` on `edit_file` / `transaction_apply` would make this ironclad without breaking the existing cheap mtime path.

**Definition of done.**

1. `read_file`'s output header is augmented to include `sha256=<hex>` alongside the mtime:
   ```
   # plumb-read mtime=2026-05-11T13:46:38.895137000+10:00 sha256=3a7bd3e2360a3...
   ```
   Computed over the *full file content* (not the line-sliced excerpt). 200 KiB cap applies to the body, not the hash.
2. `edit_file` accepts an optional `expected_sha` parameter (RFC: hex-encoded lowercase 64-char string). If provided, the file's current SHA-256 is computed before any edit; mismatch rejects with `editLogicErr`.
3. `transaction_apply` operations accept `expected_sha` the same way.
4. Tests:
   - `read_file` output includes `sha256=`.
   - `edit_file` rejects when `expected_sha` doesn't match.
   - `edit_file` succeeds when both `expected_mtime` and `expected_sha` are correct.
   - `expected_mtime` + `expected_sha` together: both must match.
5. AGENTS.md and docs/mcp-tools.md updated.

**Where to start.**

1. In `internal/tools/read_file.go`, after the `os.Stat` and before the binary-detection sniff, compute SHA-256 of the file via `crypto/sha256.New()` + `io.Copy`. *Caveat:* the existing code reads the file via `io.MultiReader(bytes.NewReader(sniff), f)` to avoid seeking. For SHA you need the full content — either compute by opening the file a second time, or hash the prefix and the rest via a `io.TeeReader`. The two-open approach is simpler; the cost (one extra `os.Open` + linear read) is acceptable for files at the 200 KiB cap.
2. In `internal/tools/edit_file.go`, add `ExpectedSha string \`json:"expected_sha"\`` to `editFileArgs`. After the `expected_mtime` block, add a parallel check: read the file, compute SHA, compare.
3. In `internal/tools/transaction.go`, mirror the same change in `txOperation`.
4. Update schemas in both `editFileSchema` and `transactionApplySchema`.

**Watch out for:**

- The schemas are inline `json.RawMessage` strings. Match the format of `expected_mtime` for consistency.
- Don't recompute the SHA inside `tryEdit`'s retry loop — compute once before the loop and pass into the retry. Otherwise three retries means three full-file reads.

---

### Stats DB migrator + `input_json` column for tool args

**Priority:** medium-high. Closes two gaps at once.
**Effort:** 45 min.

**Why this matters.** Two related problems share this fix:

1. **Migration infrastructure is half-built.** Since 0.5.3, `stats.db` is stamped with `PRAGMA user_version = 1`. There is **no migrator** — `Open` just writes the constant value. A future schema bump (to 2) would overwrite the on-disk `user_version` without applying any `ALTER TABLE`, silently corrupting older databases.
2. **Recent Edits panel can't show paths.** The TUI's `filterWriteCalls` returns the tool name, duration, and age — but not *what was edited*. The stats schema doesn't store the call's args, so we can't show "edited foo.go" vs "edited bar.go". The agent gets "write_file 12 ms 4 s ago" with no way to know which file.

The fix is to land both at once: build the migrator now, use it to add `input_json` to `tool_calls`, then surface paths in the TUI.

**Definition of done.**

1. `internal/stats/db.go` exports `migrate(db *sql.DB, from, to int) error` that walks a slice of `{from, to, sql}` records and applies each.
2. `Open` reads the current `user_version`, applies migrations forward to `SchemaVersion`, then stamps the new version.
3. `SchemaVersion` bumps to `2`. The new migration records: `{from: 1, to: 2, sql: "ALTER TABLE tool_calls ADD COLUMN input_json TEXT NOT NULL DEFAULT ''"}`.
4. `stats.Call` struct gains `InputJSON string`. `stats.DB.Record` writes it. `stats.RecentCall` exposes it. `Recent` query reads it.
5. `internal/cli/daemon.go`'s `OnAfterTool` callback captures `args json.RawMessage` (parameter already there) and passes `string(args)` to `Record`.
6. `internal/tui/model.go`'s `filterWriteCalls` / rendering parses the JSON, extracts the `path` (or `from`/`to` for `rename_file`), and renders it in the panel:
   ```
   ── Recent Edits ──
     ✓  edit_file       internal/tools/edit_file.go      12ms  4s ago
     ✓  rename_file     foo.go → bar.go                   8ms  9s ago
   ```
7. Tests:
   - `TestMigrate_AppliesV1ToV2` — write a v1 database, open it, confirm `user_version` is 2 and `input_json` column exists.
   - `TestMigrate_NoOpAtCurrent` — open a fresh v2 database; no migrations run; `user_version` stays at 2.
   - `TestRecord_StoresInputJSON` — record a call with args, read it back via `Recent`, confirm `InputJSON` round-trips.
8. CHANGELOG entry covers both the migrator infrastructure and the new column. `architecture.md` reference to "Schema migrations: none yet" is replaced with a pointer to `migrate()`.

**Where to start.**

1. `internal/stats/db.go`:
   ```go
   type migration struct {
       from, to int
       sql      string
   }
   var migrations = []migration{
       {from: 1, to: 2, sql: `ALTER TABLE tool_calls ADD COLUMN input_json TEXT NOT NULL DEFAULT ''`},
   }
   func migrate(db *sql.DB, from, to int) error { /* walk migrations, exec each */ }
   ```
2. In `Open`, after the schema CREATE, read `PRAGMA user_version`, call `migrate(db, current, SchemaVersion)`, then stamp `SchemaVersion`.
3. Bump `SchemaVersion = 2`.
4. Add `InputJSON string` to `stats.Call` and `stats.RecentCall`. Update `Record` and `Recent`.
5. In `internal/cli/daemon.go`, `OnAfterTool` already receives `args json.RawMessage`. Pass `string(args)` into `statsStore.Record(...).InputJSON`.
6. In `internal/tui/model.go`, the Recent Edits section. Add a tiny `extractPath(tool, inputJSON string) string` helper that:
   - For `write_file`, `edit_file`, `delete_file`: unmarshal into `{Path string}` and return `Path`.
   - For `rename_file`: unmarshal into `{From, To string}` and return `"From → To"`.
   - For `transaction_apply`: unmarshal into `{Operations []struct{Path string}}` and return `fmt.Sprintf("%d files", len(...))`.

**Watch out for:**

- The `input_json` column will be empty (`""`) for all rows that were recorded by pre-migration daemons. The TUI needs to handle that gracefully — if `extractPath` returns `""`, render the existing tool-only format.
- `input_json` is unindexed. If you ever want to query by path, add an index in a future migration.
- Capping the stored JSON size at, say, 4 KiB would prevent a pathological tool call with megabytes of args from bloating the DB. Cheap to add now.

---

### Configurable post-write diagnostics window

**Priority:** medium.
**Effort:** 20 min.

**Why this matters.** `postWriteDiagWindow = 300 * time.Millisecond` is a magic constant in `internal/tools/file_write_helpers.go`. The gopls integration smoke test passes in ~1.2 s, but the diagnostic itself arrives within ~300 ms; that's our budget. For:

- Cold pyright on a large project: 1–3 s before republishing. Our window is too short; the agent doesn't see the error.
- Fast warm gopls on small file: <50 ms. Our window is wastefully long; we sleep until the deadline instead of returning quickly.

The right answer is configurable, with the existing four-layer precedence (default → global → project → env).

**Definition of done.**

1. `EditsConfig` gains `PostWriteDiagnosticsMs int \`toml:"post_write_diagnostics_ms"\``. Default 300.
2. `validate` enforces `>= 0` (0 disables polling entirely).
3. `WriteDeps` gains `PostWriteDiagWindow time.Duration` (zero value = 300 ms for back-compat).
4. `awaitDiagnosticsRefresh` uses the passed-in duration, not the constant.
5. Daemon wiring: `applyProjectConfig` updates the window on the live `WriteDeps`. Tools read from `deps.PostWriteDiagWindow`.
6. AGENTS.md and README.md document the field.
7. `plumb config show` displays the resolved value.

**Where to start.**

1. `internal/config/config.go`: add the field to `EditsConfig`, set default in `defaults`.
2. `internal/tools/write_deps.go`: add `PostWriteDiagWindow time.Duration`.
3. `internal/tools/file_write_helpers.go`: change `awaitDiagnosticsRefresh` signature to take a duration, fall back to 300ms if zero.
4. Update `write_file.go` and `edit_file.go` to pass `t.deps.PostWriteDiagWindow`.
5. `internal/cli/daemon.go`'s `applyProjectConfig`: when the config changes, the closure-captured `editsCfg` already updates. Either expose the window through `editsCfg` and have the tools read via a closure (cleaner) or set it on `writeDeps` once at startup and accept that runtime config changes don't propagate to the window (simpler, probably fine).

**Watch out for:** the test for `awaitDiagnosticsRefresh` doesn't exist today. Worth adding one: stub `postWriteDiagSource`, call with a 50ms window and `time.Sleep(60ms)`, confirm the function returns. Then bump the window to 200ms and confirm it returns immediately when a diagnostic change fires.

---

### `plumb doctor` — discovery + health-check CLI

**Priority:** medium. Improves first-run experience and ongoing debuggability.
**Effort:** 2–4 hours depending on scope (it's a "how far do you want to take it?" feature).

**Why this matters.** Users install plumb but don't always know it can be wired into multiple MCP-capable clients (Claude Desktop, Claude Code, Gemini CLI, Cursor, Continue, possibly others). Discovery is one-by-one through docs. A `plumb doctor` (in the spirit of `brew doctor`) would scan the host for known MCP-capable clients, show config status for each, and surface system-level health issues (daemon running, version match, LSP servers on PATH, stats DB writable, etc.).

**Scope:** detection and reporting only. Does **not** auto-configure — the user runs `plumb setup <client>` for the ones they want. The point is *visibility*: "here's everything you could be using plumb with, and where things stand right now."

**Definition of done.**

1. New `plumb doctor` subcommand (`internal/cli/doctor.go`). Output is a traffic-light report:
   ```
   plumb doctor — 0.5.8

   System
     ✓ plumb binary       /usr/local/bin/plumb (0.5.8)
     ✓ daemon running     PID 21370, version 0.5.8 (matches binary)
     ✓ gopls              /Users/gilberto/go/bin/gopls (v0.16.2)
     ⚠ pyright-langserver not found on PATH (Python projects won't have an LSP backend)
     ✓ stats DB           ~/Projects/plumb/.plumb/stats.db (246 calls, schema v2)

   Configuration
     ✓ global config      ~/.config/plumb/config.toml (exists)
     ⚠ project config     ~/Projects/plumb/.plumb/config.toml (not found — using global)

   MCP clients
     ✓ Claude Desktop     ~/Library/Application Support/Claude/claude_desktop_config.json (plumb registered)
     ✓ Claude Code        ~/.claude.json (plumb registered)
     ✗ Gemini CLI         ~/.gemini/settings.json (exists, plumb NOT registered — run `plumb setup gemini`)
     ⚠ Cursor             ~/.cursor/mcp.json (not found — install Cursor or skip)
     ⚠ Continue           ~/.continue/config.json (not found — install Continue or skip)

   Status: 1 problem (Gemini CLI), 3 informational warnings.
   ```

2. The check set:
   - **System**: plumb binary path + version; daemon process running + version-match (compare to `~/Library/Caches/plumb/plumb.version`); `gopls` on `$PATH`; `pyright-langserver` on `$PATH`; current workspace's `.plumb/stats.db` exists + readable + at expected schema version.
   - **Configuration**: global config existence; project config existence (if `--workspace` or cwd is inside a project); `[edits].strict` and rate-limit values; warn if env-var overrides are active.
   - **MCP clients**: walk a known list of client config paths; for each, parse the JSON, check whether `plumb` appears in the `mcpServers` (or equivalent) block; check whether the command path matches our binary.

3. Exit code: `0` if everything is ✓, `1` if any ✗. ⚠ (warnings) don't fail.

4. `plumb doctor --json` for machine-readable output.

5. Tests: each detector is unit-testable by injecting fake filesystem paths and binaries. The composition function that prints the report can be tested by feeding it a synthetic detector-result set.

**Where to start.**

1. Look at `internal/cli/setup.go` for the existing client-config writers — they already know how to locate Claude Desktop / Claude Code / Gemini CLI config paths. Reuse those path-resolution helpers (`claudeDesktopConfigPath`, `GeminiConfigPath`, etc.). Each `setup-*` command already knows its target; doctor just needs the read-side equivalents.
2. Create `internal/cli/doctor.go` with one function per check:
   ```go
   type checkResult struct {
       Name    string
       Status  status // ok | warn | fail
       Detail  string
       Hint    string // what to do about it
   }
   func checkPlumbBinary() checkResult { ... }
   func checkDaemon() checkResult { ... }
   func checkGopls() checkResult { ... }
   func checkPyright() checkResult { ... }
   func checkStatsDB(ws string) checkResult { ... }
   func checkGlobalConfig() checkResult { ... }
   func checkProjectConfig(ws string) checkResult { ... }
   func checkClaudeDesktop() checkResult { ... }
   func checkClaudeCode() checkResult { ... }
   func checkGemini() checkResult { ... }
   func checkCursor() checkResult { ... }
   func checkContinue() checkResult { ... }
   ```
3. The `runDoctor` function calls all of them, groups by section, prints with appropriate colour, sets exit code.
4. Register in `rootCmd.AddCommand(...)` in `root.go`.

**Known MCP client config locations** (research these per-OS; macOS paths shown):

| Client | Config path | Detection rule |
|---|---|---|
| Claude Desktop | `~/Library/Application Support/Claude/claude_desktop_config.json` | parse JSON, check `mcpServers.plumb` |
| Claude Code (user) | `~/.claude.json` | parse JSON, check `mcpServers.plumb` |
| Claude Code (project) | `<workspace>/.mcp.json` | parse JSON, check `mcpServers.plumb` |
| Gemini CLI | `~/.gemini/settings.json` | (research; the user added Gemini support in 0.5.x — see `internal/cli/setup.go` and `internal/cli/config.go`'s `GeminiConfigPath`) |
| Cursor | `~/.cursor/mcp.json` (or similar — verify with Cursor docs) | parse JSON, check for plumb entry |
| Continue | `~/.continue/config.json` | parse JSON, check `mcpServers` |
| Cline / Cody | research per-tool | likely JSON config in `~/.config/<tool>/` |

**Watch out for:**

- Each client uses slightly different JSON shape for MCP server registration. Don't assume one schema fits all. Use the existing `setup-*` writers as the source of truth for "what plumb's entry looks like in this client's config".
- Detection should be *gentle*: a missing config file means the user hasn't installed the client, not that plumb is broken. Use ⚠ (warning), not ✗ (failure), for those.
- The "command path matches our binary" check is the one that catches stale installs (e.g. plumb was installed via `go install`, then via Homebrew; the config points at the old `go install` path). Get this right and you'll save a lot of "why isn't plumb working?" debugging.
- Don't shell out to each client to "test" the integration — purely static analysis. Keep it fast.

**Future extensions (don't do them now, but worth noting):**

- `plumb doctor --fix`: opt-in auto-fix for the simplest issues (register plumb in a detected-but-unconfigured client).
- `plumb doctor --client <name>`: deep-dive on one client (full config dump, validation against the client's known schema).
- Integration into `plumb` (the TUI) — show a small "doctor: 1 issue" badge at the bottom if any ✗ is present.

---

### "Working tree is dirty" guard before plumb-initiated writes

**Priority:** medium.
**Effort:** 1–2 hours (depending on chosen approach).

**Why this matters.** Plumb will happily edit a file that has uncommitted changes the user hasn't reviewed. If the agent goes off the rails, the user can't easily distinguish "what I wrote" from "what plumb wrote on the agent's behalf". `git stash` recovers the file, but only if the user noticed in time and the stash hasn't been overwritten.

**Three options, listed least-disruptive first. Pick one before starting:**

1. **`dirty_ok: bool` parameter, default `false`.** Each write tool checks `git status --porcelain <path>` for the target. If output is non-empty, refuse with a clear error unless `dirty_ok=true`. Minimal surprise. Doesn't require any persistent state.
2. **Append a notice to the tool output.** "Note: foo.go had uncommitted changes before this edit. Previous content is recoverable via `git stash` if needed." Non-blocking; informational. Easier for the agent to ignore (might be the right outcome — agents are working on user behalf).
3. **Snapshot to `.plumb/snapshots/<sha>` before every write.** Heavy. Real undo log. Closest to what an editor does. Pairs naturally with the transaction durable log (below). Worth doing only if option 1 turns out to be too restrictive in practice.

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

## Subtle things to be aware of

Footguns. Not bugs; behaviour you'd want to know before depending on the relevant subsystem at scale. Each carries enough context that someone touching the area can decide whether to fix or just respect.

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

**Recommendation:** if you see flaky `concurrent write detected` errors in legitimate workflows, bump the constant. If you see silent corruption from concurrent writes that should have been retried, lower it. Pair this with the [Configurable post-write diagnostics window](#configurable-post-write-diagnostics-window) work — they share the same "exposed-as-config?" question.

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
- **Bigger TUI features** (filterable panels, search box, write-targets visualisation) — out of scope for 0.5.x. Worth a dedicated 0.6 line.
- **Native Windows support** — `safeWrite`'s atomic rename relies on POSIX rename-over-existing semantics. Windows handles this differently across Go versions. Not on the roadmap unless someone asks.
- **Per-agent identity for rate limiting and read tracking** — see Subtle Things entries above. Requires upstream MCP support for a stable client-session header.

---

## How to use this file

1. **Pick up an item:** read its section in full. The acceptance criteria (Definition of done) and the Where to start pointers should be enough to begin without re-deriving the problem.
2. **While working:** if you find a new gap, add it to this file in the same commit as your fix.
3. **When you finish:** delete the section from this file, add the corresponding entry to `CHANGELOG.md` under the version that ships the fix, and commit both changes together.
4. **If you can't finish:** leave the section in place but add a short "Status:" note describing how far you got and what's blocking, so the next person doesn't start from scratch.

The cost of *not* capturing a gap is high — months later, the gap turns into a mystery bug or a confused new contributor. The cost of writing it down is one paragraph. Always favour capturing.
