# Plumb — Outstanding TODO

Living index of known gaps, deferred work, and footguns. Add to it as you find things; remove items when they're shipped (record the version in `CHANGELOG.md` and delete the line here).

Last reviewed against: **0.5.6** (2026-05-11).

---

## Production-blocking — do before relying on plumb for serious work

These are the items that determine whether plumb is genuinely safe and proven, not just compiling-and-passing-unit-tests. The unit suite is green; what's missing is *end-to-end confidence in a real environment*.

### Claude Desktop end-to-end smoke test

Plumb was rebuilt across 0.5.x specifically to make Claude Desktop work. None of the new mechanism has been verified against real Claude Desktop:

- `session_start`'s `roots/list` fallback (added in 0.5.1 #3) — Claude Desktop's roots support has historically been spotty; verify it actually responds.
- The cold-start workspace chain — when the daemon launches from `$HOME`, does the cwd walk find anything useful, or does roots/list save us?
- The MCP Prompts (`orient`, `whats-broken`, `recent-changes`) — do they render correctly as Desktop menu items?
- Memory resources (`plumb-memory://`, `plumb://workspace/context`) — do they appear in the resources sidebar?
- Post-write diagnostics in `edit_file` output — does the agent actually see them in its tool response?

**How to test:** wire `plumb setup claude-desktop`, restart Desktop, open a Go project, ask Claude to read + edit a file, watch the daemon log for the relevant calls.

### Pyright integration smoke test

`TestIntegration_DidChangeWatchedFiles` (0.5.6) proved the architectural rewrite works against real gopls. The pyright adapter has the same wiring but **the equivalent integration test does not exist**.

**Action:** copy the gopls test to `internal/lsp/adapters/pyright/`, point at `testdata/python-fixture/` (create if needed), assert pyright republishes diagnostics for a broken `.py` file after `DidChangeWatchedFiles`. Gate `//go:build integration`. ~30 minutes once `pyright-langserver` is on `$PATH`.

Until this passes, pyright stays "experimental" in AGENTS.md regardless of how clean its unit tests are.

### CI matrix that runs integration tests

The smoke test that proves the architecture works is gated `//go:build integration`. There's no CI config I touched that runs with `-tags=integration`. If your CI doesn't include this build tag, the load-bearing test never runs in PR checks — only locally.

**Action:** add a CI job that installs `gopls` (and eventually `pyright-langserver`), runs `go test -tags=integration ./...`, and fails the PR on regression. The cost is the install + ~5s per run.

---

## Real gaps — not blocking, but the next things you'd want

### `expected_sha` parameter on `edit_file` (content-hash verification)

`expected_mtime` is voluntary, and even when supplied it relies on the filesystem reporting mtime honestly. mtime can be:

- Set arbitrarily (`touch -d`, restore-from-backup).
- Identical-but-different-content if a process writes-and-touches within the same second on a coarse-mtime fs.
- Unchanged if a process `mmap`s and writes (some setups).

Adding a SHA-256 of the file contents to `read_file`'s output header (alongside the mtime) and an optional `expected_sha` parameter on `edit_file` / `transaction_apply` would be ironclad against an adversary. mtime stays as the cheap path; SHA is for callers that care.

Cost: ~30 min. The contract becomes: read_file returns `{mtime, sha256, content}`; edit accepts either or both, and *any* mismatch rejects.

### Stats DB migrator + `input_json` column

`stats.db` carries `PRAGMA user_version = 1` since 0.5.3, but **no migrator runs**. Bumping to version 2 will overwrite the on-disk value without applying any schema change. The infrastructure is half-built.

Same fix-window applies to the **biggest missing feature in the TUI Recent Edits panel**: we don't store the tool's `args` JSON, so we can't show "edited `foo.go`" vs "edited `bar.go`" — just "edit_file 12ms 4s ago".

**Action plan:**

1. Define a `migrate(db, from, to int) error` function that walks a slice of `migration{from, to, sql string}`.
2. `Open` reads `user_version`, applies migrations up to `SchemaVersion`, writes the new version.
3. Add `migration{from: 1, to: 2, sql: "ALTER TABLE tool_calls ADD COLUMN input_json TEXT NOT NULL DEFAULT ''"}`.
4. Have `daemon.go`'s `OnAfterTool` capture and pass `args` to `stats.Record`.
5. TUI's `filterWriteCalls` extracts `path` from `input_json` and renders it.

~1 hour. The infrastructure pays off as soon as anyone else wants to add a column.

### Configurable post-write diagnostics window

`postWriteDiagWindow = 300 * time.Millisecond` is a magic number in `file_write_helpers.go`. Empirically fine for gopls on incremental edits, but:

- Cold pyright on a large project can be >1s before it republishes.
- A fast warm gopls might deliver in <50ms — making 300ms wasteful.

Add an `[edits].post_write_diagnostics_ms` config field with the same defaults-→global-→project-→env precedence as the rest. Plumb it through `WriteDeps`. Probably default 300ms still; allow `0` to disable entirely.

### "Working tree is dirty" guard before plumb-initiated writes

Plumb will happily edit a file that has uncommitted changes the user hasn't reviewed. A polite tool would at least warn ("you have uncommitted changes to `foo.go`; proceed?").

Options, listed least-disruptive first:

1. **Add `dirty_ok: bool` to write tools, default `false`.** If the target's parent repo has uncommitted changes to that file, refuse unless `dirty_ok=true`.
2. **Append a notice to the tool output** ("note: foo.go had uncommitted changes; previous content is recoverable via `git stash`").
3. **Snapshot to `.plumb/snapshots/<sha>` before every write.** Heaviest. Real undo log.

Option 1 is probably right. The point isn't to be paranoid — it's to make accidental destruction of uncommitted work loud.

### Transaction durable rollback log

`transaction_apply`'s rollback is **best-effort**. If the rollback `safeWrite` itself fails (disk full, permission revoked mid-operation, fs went read-only between phase 2 writes), we log an error and the file stays in its post-write state. No replay log on disk.

For an editor-class tool, this is acceptable. For "production data" use cases it isn't. The fix is a tiny WAL:

1. Phase 1: write each `prepared.before` content to `.plumb/tx-log/<txID>/<n>-before-<hash>` before any phase-2 write.
2. Phase 2: writes proceed as today.
3. On failure: rollback reads from `.plumb/tx-log/<txID>/`.
4. On success: log dir is removed.
5. On daemon startup: scan `.plumb/tx-log/` for orphaned txs (= daemon crashed mid-transaction) and complete the rollback.

~3 hours to do properly with tests. Defer until a real use case demands it.

---

## Subtle things to be aware of

The footguns. None of these are bugs; they're behaviour you'd want to know before depending on the relevant subsystem at scale.

- **`pathLocks` is permanent process-global state.** Every path ever locked stays in the `sync.Map` for the daemon's lifetime. For long-running daemons with many sessions, this can grow without bound. Not a leak in the GC sense, but a slow memory creep. Worth adding an LRU/timed eviction at some point.

- **The rate limiter is per-connection.** A single agent making 1000 connections in a minute can do 120 writes per connection. Probably fine in practice but worth knowing.

- **CRLF normalization in `edit_file` is one-directional toward the file.** If your `old_str` is CRLF and the file is LF, the normalization converts. But if your file is mixed (rare but happens), behaviour is fuzzy. The test coverage doesn't include mixed-ending files; we should probably document this corner case rather than try to be clever about it.

- **`expected_mtime` is voluntary.** Agents can ignore it. Strict mode (which forces the matter) is opt-in via `PLUMB_STRICT_EDITS=1` or `[edits].strict = true`. For a hostile or buggy agent, the per-path lock is the only real defence — and it only catches *concurrent* corruption, not "agent edits stale content because it didn't bother to re-read."

- **`readMtimes` lives in the `ReadTracker` per connection, not per agent identity.** If one Claude Desktop instance opens N tabs that each spawn separate `plumb serve` processes, each has its own tracker. Strict mode's "you must have read this in *this session*" is per MCP connection, not per human-meaningful "session".

- **Daemon-version mismatch warns but doesn't enforce.** After a `make build`, `plumb serve` warns "connected daemon is X but this binary is Y — run `plumb stop`". It does not auto-restart. Manual `plumb stop && plumb serve` required to pick up new code.

- **Capability negotiation: gopls's response to `client/registerCapability` is accepted but never inspected.** We respond `null` (OK) and move on. If a future LSP server registered a glob we don't intend to honour, plumb wouldn't know. In practice gopls registers `**/*.go` etc. and we send notifications for everything; the registered globs are advisory at worst.

- **Symlink resolution in `safeWrite` calls `filepath.EvalSymlinks`** which fails on broken symlinks. If the target is a dangling symlink, `safeWrite` falls back to writing through to whatever-the-link-points-at (which doesn't exist), and the underlying `os.Stat` returns `IsNotExist` — handled as a new-file create. This is probably the right behaviour, but it's subtle.

---

## Considered and deferred

Things that came up in review discussions but were decided against (or deferred deliberately). Listed here so future-you doesn't re-litigate.

- **`WriteDeps` refactor** — done in 0.5.4. No longer pending.
- **Push to `origin/main`** — explicit user decision per session. Kept local for now; user pushes when ready.
- **Style nits in the linter** (`for i := 0; ...` → `for range`, `WaitGroup.Go`, etc.) — applied opportunistically in files we touched (0.5.3); not chasing them across the rest of the codebase.
- **Bigger TUI features** (filterable panels, search box) — out of scope for the 0.5.x release line. Would be a 0.6 feature pass.
- **Native Windows support** — `safeWrite`'s atomic rename relies on POSIX semantics. Windows `os.Rename` over an existing file may fail on older Go versions. Not on the roadmap unless someone asks.

---

## How to use this file

When you complete an item:

1. Delete the section here.
2. Add the corresponding entry to `CHANGELOG.md` under the version that ships the fix.
3. If the work uncovered new gaps, add them to the relevant section above.

If you spot a new gap during normal work, **add it to this file in the same commit** — the cost of not capturing it is high, the cost of writing it down is one paragraph.
