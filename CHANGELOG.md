# Changelog

## 0.6.6 (2026-05-20)

### Changed
- **CQ-1: Mechanical lint cleanup (79 → 51 findings).** Cleared all non-gocyclo golangci-lint findings: `gofumpt`/`goimports` formatting on 10+ files (golangci-lint `--fix` used to apply the embedded formatter version); `ineffassign` in `daemon.go` (dead `rootURI` reassignment) and `walk.go` (intermediate `name` variable); `prealloc` on 5 slices (`diff.go`, `edit_apply.go`, `model_render.go`×3); `revive` stutter — renamed `lsp.LSPClient` interface to `lsp.Client` across 18 files via LSP semantic rename + text replacement; `errcheck` — `os.MkdirAll` calls in `stats_test.go` now use `t.Fatal`, `io.Copy` drain goroutine in `conn_test.go` uses `_, _ =`; `staticcheck` — `QF1008` embedded-field selectors, `QF1001` De Morgan, `QF1003` tagged switch in `mcp/server.go`, `ST1005` trailing periods on 4 error strings, `SA4010` dead `uris` append in `transaction.go` (confirmed dead code, not a rollback bug — per-file `notifyLSP` calls already handle LSP notification). Remaining 51 findings: gocyclo (37, deferred to CQ-3) and gosec (14, deferred to CQ-5).
- **CQ-2: Delete dead code.** Removed unused declarations flagged by `golangci-lint unused`/`unparam`: `invProxy` struct and its `Diagnostics`/`AllDiagnostics` methods (`internal/cli/proxy.go`), `parseFrontmatter` wrapper function (`internal/memory/store.go`), `spliceOverlayLower` (`internal/tui/model_utils.go`). Simplified three vestigial signatures: `splitFrontmatter` no longer returns the delimiter byte slice (callers discarded it); `defaultWriteRateLimit` no longer returns a `time.Duration` (window is always `time.Minute`); `setState` in `internal/lsp/supervisor.go` had both `conn` and `proc` parameters always passed as nil — signature simplified to take only the new state. Zero `unused` and zero `unparam` findings remain.

## 0.6.4 (2026-05-18)

### Added
- **Auto-attach fallback for undetected workspaces.** When `pool.Detect` fails (no `.plumb/`, `go.mod`, `pyproject.toml`, or `setup.py` anywhere above the seed path) and `[workspace].auto_attach = true` is set, `OnBeforeTool` now calls `pool.SynthesiseRoot` to walk up to the nearest `.git/` directory and uses that as a synthetic workspace root — falling back to the seed directory itself when no `.git/` is found. The session is fully attributed: stats, project config, and TUI presence all work normally; only LSP tools are unavailable (language = `none`). The session JSON and TUI session list show a `Synthetic: true` flag / `(auto)` suffix so synthetic sessions are visually distinguishable from real ones. If `[workspace].auto_attach_persist = true`, the daemon also creates `<root>/.plumb/` on first attach so subsequent sessions resolve via the standard marker path without fallback. Both flags default to `false` (opt-in); existing sessions using real markers are unaffected. Configurable at all four layers; exposed in `plumb config show` under the new `workspace` section. Env vars: `PLUMB_AUTO_ATTACH`, `PLUMB_AUTO_ATTACH_PERSIST`.

## 0.6.3 (2026-05-18)

### Added
- **TUI Live Log Viewer (`Logs` section).** Switching to the `Logs` tab in the section menu (press `/` → select `Logs`) opens a full-width, live-tailing view of `daemon.log`. On first entry the tab reads the last 64 KiB of the log file; subsequent polls (every 2 s) append new lines. Entries are rendered with a timestamp, colour-coded level badge (`INFO` green, `WARN`/`ERROR` yellow, `DEBUG` muted), message, and sorted `key=val` attribute pairs for JSON log lines; plain-text lines are shown as-is. Navigation: `j`/`k` or `↑`/`↓` to scroll, `pgup`/`pgdown` for page scroll, `G` to re-engage follow mode (auto-scroll to newest entry). Typing any printable character appends to a substring filter (case-insensitive, matched against the raw line); `backspace` erases one character; `esc` clears the filter or opens the section menu when the filter is empty. The entry count and follow status are shown in the border and footer. Works with both `log_format = "text"` and `log_format = "json"` — JSON lines are parsed into structured fields; plain text is shown verbatim.

## 0.6.2 (2026-05-18)

### Added
- **Unified diff in `edit_file` and `write_file` responses.** After a successful write, both tools append a unified diff (Myers O(ND) algorithm, capped at 80 lines) so the calling agent can verify what changed without a round-trip `read_file`. For `write_file` creating a new file the response says `new file` instead of a diff. Controlled by `[edits].show_write_diff` (default `true`) or `PLUMB_SHOW_WRITE_DIFF=0` for implicit-verification mode.
- **Smart truncation for `search_in_files` and `git log/blame`.** `search_in_files` summary messages now say `Showing first N hit(s) across M file(s) — limit reached (pass max_results=N to raise, or narrow with glob/path/pattern)` when the result set is truncated, and `N hit(s) across M file(s).` otherwise. `git log` and `git blame` output is capped at 200 lines with a how-to-narrow hint; the existing 100 KiB byte cap remains as the final safety net.

## 0.6.1 (2026-05-17)

### Changed
- **Rate limiter keyed by client identity.** `RateLimiter` gains an optional `parent *RateLimiter` (set via `SetParent`). The daemon creates one shared `RateLimiter` per MCP client identity (`ClientName/ClientVersion`) in a daemon-scoped `sync.Map`. When `OnClientInfo` fires, the connection's per-connection limiter is linked to this shared budget as its parent. `Allow()` evaluates the local window first (early return without side-effects), then checks the parent without holding the local lock (prevents lock-ordering issues), then re-acquires and records the slot only if both pass. Per-connection limiters remain isolated for per-project config updates (`applyProjectConfig → SetLimit`); only the shared budget prevents cross-connection bypass.

## 0.6.0 (2026-05-17)

### Changed
- **`pathLocks` LRU eviction.** Per-path write-lock entries are now wrapped in a `pathLockEntry` struct that tracks `lastUsedNs` via `atomic.Int64`. A background sweep goroutine (started once per daemon lifetime via `tools.StartPathLockSweep(ctx)`) evicts entries idle for more than one hour every five minutes. The sweep uses `TryLock` to skip entries that are currently held, and re-checks idleness after acquiring to guard against entries refreshed between the initial Range scan and the TryLock call. Prevents unbounded memory growth in daemons running for extended periods against large numbers of distinct file paths.

## 0.6.5 (2026-05-19)

### Added
- **End-to-end MCP wire protocol smoke test.** `go test -tags=integration -timeout=3m ./cmd/smoke/` replaces the old manual Claude Desktop smoke checklist. The test compiles a fresh `plumb` binary, spawns `plumb serve` with an isolated `HOME` (separate socket, no developer-daemon interference; macOS `sun_path` < 104 bytes via `/tmp`), speaks newline-delimited JSON-RPC 2.0 over stdin/stdout, answers `roots/list`, calls `session_start` (allowing up to 120 s for gopls cold-start), exercises `read_file`, `edit_file`, `write_file` with a syntax error (asserts "diagnostics after write"), and `list_memories`. The fixture is copied from `testdata/go-fixture`. Prerequisites: `gopls` on PATH. Skips automatically otherwise.
- **`list_symbols` — `include_signatures` option.** New optional `include_signatures: bool` parameter (default `false`). When `true`, the first source line of each symbol (the full declaration, including receiver type and parameter list) is appended below its entry in the outline, indented as `  → <line>`. Reads all declaration lines in a single `readFileLines` call via the existing helper. Useful for understanding a file's API surface without opening it.
- **`search_in_files` — line-by-line streaming scan.** The per-file scan function is rewritten to use a `bufio.Scanner` with a custom split function instead of reading the entire file into a `bytes.Buffer`. This eliminates the O(file_size) per-worker allocation and reduces memory pressure on large workspaces. Long lines (> 1 MiB) are skipped in-place without aborting the rest of the file. Description updated to steer agents toward `workspace_symbols` for name lookups.
- **Claude Code tool guidance in `session_start`.** When the MCP client identifies as `claude-code`, `session_start` now appends a `## Tool guidance (Claude Code)` section listing the 8 LSP-backed tools that have no native Claude Code equivalent (`workspace_symbols`, `find_references`, `get_definition`, `call_hierarchy`, `type_hierarchy`, `rename_symbol`, `list_symbols` with signatures, `diagnostics`). Client name is read via a new `clientNameFn func() string` closure threaded from the daemon's `OnClientInfo` callback.
- **`workspace_symbols`, `call_hierarchy`, `type_hierarchy`, `rename_symbol` descriptions** now lead with `"No native Claude Code equivalent."` to reduce choice paralysis when both plumb and native tools are available.
- **`protocol.FileURI` Windows portability.** `FileURI(path)` in `internal/lsp/protocol/types.go` now calls `filepath.ToSlash` and prepends a `/` for Windows drive paths (`C:\project` → `file:///C:/project`). `pool.go` and `notifyLSP` updated to use `protocol.FileURI` instead of `"file://" + path`.

### Added
- **`read_symbol` tool.** Reads the source body of a named symbol in one call, collapsing the `list_symbols` + `read_file` two-round-trip pattern. Accepts a plain name (`handleConn`) or dotted `ReceiverType.MethodName` form (`Model.renderDashboard`). Returns all matches with line ranges when the name is ambiguous. The output header matches `read_file` so the mtime can be passed as `expected_mtime` to `edit_file`. Mtime is recorded in `ReadTracker` exactly as `read_file` does.
- **Name-based lookup for `find_references`.** New optional `symbol_name` parameter: when provided alongside `uri`, the tool resolves the symbol's position via `DocumentSymbols` and then queries references, eliminating the `list_symbols → find_references` two-call pattern. For ambiguous names (multiple symbols sharing the name), references for all matches are returned with a per-symbol header. The positional form (`line` + `character`) remains unchanged.
- **Name-based lookup for `get_definition`.** Same pattern as `find_references`: optional `symbol_name` resolves the position via `DocumentSymbols` and delegates to the existing definition lookup. Positional form unchanged.
- **`client/registerCapability` glob tracking.** All three adapters (gopls, pyright, jdtls) now parse and store the file-watcher glob patterns registered by the server. `DidChangeWatchedFiles` filters events to only those matching a registered pattern before sending; when no patterns are registered yet the existing behaviour (send everything) is preserved. `client/unregisterCapability` removes the named registrations. New `internal/lsp/watcher` package provides the thread-safe `Filter` type shared across adapters.
- **Language-aware `DidOpen` in `find_references`.** `openFileForRefs` previously hardcoded `LanguageID: "go"`. It now derives the language ID from the file extension via `languageIDFromPath`, correctly handling `.py` → `python`, `.java` → `java`, and other extensions supported by plumb's LSP adapters.

### Fixed
- **Watcher glob matching: `{a,b,c}` alternation and absolute-path `**/`.** gopls v0.22+ registers file watchers using two pattern forms that the old `matchGlob` could not handle: (1) `{a,b,c}` alternation — `filepath.Match` has no brace syntax, so patterns like `**/*.{go,mod,sum,work}` never matched and every `DidChangeWatchedFiles` event was silently dropped once gopls registered its watchers; (2) absolute-path double-star, e.g. `/workspace/**/*.go` — the old code only stripped a leading `**/` prefix, so embedded `/**/` in absolute paths fell through to `filepath.Match` which also does not support `**`. The combined effect was that file-create and file-change events were filtered out, gopls never published post-write diagnostics, and the smoke test consistently failed. Fix: expand `{…}` by recursing on each alternative before matching; handle `/**/` in absolute paths by splitting on that token and testing prefix + suffix independently. New test cases cover both forms.
- **JSON-RPC conn: handle server requests with string IDs.** The `wireMessage.ID` field was `*int64`, which failed to unmarshal when a server sends a string-typed ID. jdtls sends `client/registerCapability` with `"id":"1"` (a JSON string); this caused `json.Unmarshal` to fail, the read loop to exit, and all subsequent messages — including `textDocument/publishDiagnostics` — to be silently dropped. ID is now `json.RawMessage` so both integer and string IDs round-trip correctly. The pending-call map uses `string(rawID)` as the key; outbound calls continue to use monotonically increasing integer IDs. Regression test added in `TestConn_ServerRequest_StringID`.
- **`list_symbols` `include_signatures` — callable kinds only, skip comments.** The `→` declaration annotation is now restricted to function, method, and constructor symbols (`SKFunction`, `SKMethod`, `SKConstructor`). Previously it applied to all kinds, including struct fields, constants, and type aliases where the declaration line adds no useful type information. Blank lines and comment lines at the symbol's `start_line` position are also silently skipped so that Python decorators or Go doc comments reported at the range start do not appear as the signature. Two new test cases: `TestListSymbols_IncludeSignatures_NonCallableKinds` (asserts no `→` for struct/field symbols) and `TestListSymbols_IncludeSignatures_SkipsCommentLines`. Docs updated in `docs/mcp-tools.md`.
- **`replace_symbol_body` and `transaction_apply` tool descriptions** now lead with `"No native Claude Code equivalent."`, matching the pattern established for `workspace_symbols`, `call_hierarchy`, `type_hierarchy`, and `rename_symbol` in 0.6.5.
- **Call Detail popup footer fixed and always visible.** Refactored `renderPopup` to reserve 2 lines at the bottom of the right panel for a fixed footer that stays visible during scrolling. The footer now includes contextual hints: `c copy · tab back · esc close` when the right panel is focused, and `tab detail · esc close` when the left panel is focused.
- **Java write-tool `DidOpen`/`DidClose`.** `WriteDeps` gains a new `PostWriteNotifyFn func(ctx, path) error` hook, called after `notifyLSP` in `write_file`, `edit_file`, and `transaction_apply`. The daemon wires it for Java workspaces as a closure that reads the freshly-written file and sends `DidOpen` (language ID `java`) + `DidClose` to jdtls — the pair that triggers diagnostic reconciliation. This closes the gap where `DidChangeWatchedFiles` alone updated jdtls's project model but did not cause it to publish fresh diagnostics.

### Changed
- **`search_in_files` — warm-call benchmark added.** `BenchmarkSearchInFiles_WarmCall` in `internal/tools/search_in_files_test.go` measures steady-state search latency on a 50-file workspace with the OS page cache warm, providing a reproducible lower-bound for the p95 latency target (< 200 ms). Current arm64 result: ~1.75 ms/op.

### Added
- **TUI Sessions panel redesign.** The Sessions panel border no longer embeds panel titles or `┬`/`┴` junctions; the internal `┆` divider floats free of the frame. The left panel header is now a full-width background-filled row showing `Sessions (N)`, with the selected session indicated by `❯` instead of `●`. The right panel replaces the scrolling Details/Tools combined view with four tabs — **Details · Tools · History · Diagnostics** — navigable via tab/shift+tab or click. The tab bar fills the full right-panel width with a uniform background; only the active tab's text is highlighted (bold accent colour). The Diagnostics tab shows the output of the last `diagnostics` tool call from the stats DB for the current workspace, refreshed on the normal poll cycle.
- **TUI daemon CPU widget.** The daemon now writes a lightweight `daemon.metrics.json` snapshot in the plumb cache directory, and the TUI reads it on the normal poll interval so the displayed CPU usage belongs to the plumb daemon process, not the TUI process. The top rail shows a compact `Daemon CPU` sparkline with a fixed 0–100% scale and `n/a` when process CPU time is unavailable. Child language servers (`gopls`, `pyright`, `jdtls`) are intentionally excluded.
- **Java LSP support via Eclipse JDT Language Server (jdtls).** New adapter at `internal/lsp/adapters/jdtls/` implements the full `LSPClient` interface. Install with `brew install jdtls` (requires Java 21+). Enable with `[lsp.java] enabled = true` in `~/.config/plumb/config.toml`. Root markers: `pom.xml`, `build.gradle`, `build.gradle.kts`, `.classpath`. The pool automatically passes `-data <per-workspace-dir>` (computed from a SHA-256 hash of the workspace root) so each Java project gets isolated Eclipse workspace state — no manual configuration required. `client/registerCapability` is handled so jdtls's file-watcher registration succeeds and `workspace/didChangeWatchedFiles` notifications reach the server after every plumb write. `JAVA_HOME` is forwarded via `InitializationOptions.settings.java.home` when set in the daemon's environment (SDKMAN users get this automatically). Unit tests cover all 15 `LSPClient` methods; integration test (`//go:build integration`) spawns a real jdtls binary against `testdata/java-fixture/` using `DidChangeWatchedFiles + DidOpen` (jdtls requires both for timely diagnostics, unlike gopls/pyright which react to `DidChangeWatchedFiles` alone).
- **`plumb doctor` now reports all configured language servers**, not just enabled ones. Disabled languages (Python, Java by default) are shown as informational (`✓`) with a note about how to enable them; previously they were silently skipped.
- **Transaction durable rollback log.** `transaction_apply` now writes a write-ahead log under `<workspace>/.plumb/tx-log/<txID>/` before entering phase 2 (the actual writes). For each file, the pre-write content is snapshotted to `<n>-before` and the operation is recorded in `manifest.json` before `safeWrite` is called. On success, `Commit()` removes the directory. On failure, `Rollback()` restores all snapshotted files and removes the directory. If the daemon crashes mid-transaction, the orphaned directory is detected by `txlog.Scan` at the next workspace attach and all snapshotted files are restored automatically. Files larger than 10 MiB are recorded in the manifest but not snapshotted — rollback will log a warning and skip them. New `internal/tools/txlog` package; `WriteDeps.WorkspaceFn` provides the workspace path to `transaction_apply`.
- **`dirty_ok` parameter on all write tools.** `write_file`, `edit_file`, `delete_file`, `rename_file`, and `transaction_apply` now accept `dirty_ok: bool` (default `false`). Before writing, plumb runs `git status --porcelain` for the target file(s). If any target has uncommitted changes — modified, staged, or untracked — the operation is refused with a clear error ("has uncommitted changes; review and commit first, or pass `dirty_ok: true`"). `transaction_apply` batches checks by directory (one git invocation per directory) and reports all dirty files at once rather than stopping at the first. The check is skipped when git is not on `$PATH` or the file is outside a git repository. New files that do not yet exist on disk are not dirty and are always allowed. `pathIsDirty` and `dirtyBasenamesInDir` helpers in `internal/tools/file_write_helpers.go`.
- **`rename_session` tool.** The current MCP session's generated name can now be replaced with a short user- or agent-chosen name. Names use the existing `Name` field, are normalised to uppercase, accept only ASCII letters and `-`, must not start or end with `-`, and are capped at 16 characters to match the generated-name envelope. Renames update the session file, `daemon_info`, future stats rows, and already-recorded rows for the current session in the global stats DB.
- **`daemon_info` tool.** Returns current session name (e.g. `SWIFT-FALCON`), session ID, daemon version, daemon start timestamp, and uptime. Agents can call this to identify which session they are operating in or to confirm daemon state after a restart.
- **Session name in stats DB.** The `session_name` column is now written to every `tool_calls` row. The `Filter` struct gains a `SessionName` field for narrowing queries by name. The TUI popup detail row shows `NAME  id  ● current` and the session right-panel shows a dedicated `Name` row above the session ID.
- **`[edits].post_write_diagnostics_ms` config field.** Controls how long `write_file` and `edit_file` wait for the LSP server to re-publish diagnostics after a successful write. Default 300 ms (matching the old hard-coded constant). Set to 0 to disable the wait; raise it for slow/cold language servers (e.g. pyright on first run). Configurable at all four layers (compiled default → global → project → env `PLUMB_POST_WRITE_DIAG_MS`). Visible in `plumb config show` under the `edits` section.
- **`log_format` config field** (`"text"` | `"json"`, default `"text"`). Controls the daemon's structured log output format. Set to `"json"` to enable machine-parseable log lines for the future TUI log viewer or external log pipelines. Configurable via `PLUMB_LOG_FORMAT` env var. Visible in `plumb config show` under the `core` section.
- **`plumb log-level <level>`** — new subcommand to change the running daemon's log level without restarting it. Accepted values: `debug`, `info`, `warn`, `error`, `reset`. `reset` restores the level from config (`log_level` in `config.toml` or `PLUMB_LOG_LEVEL` env var). The change is daemon-lifetime only — the daemon reverts on next restart. Implemented via a separate admin Unix socket (`plumb.ctrl.sock`) so it never appears in the MCP tool list and cannot be reached by MCP clients. Includes `plumb log-level reset` as a clean "undo debugging session" workflow.
- **`search_in_files` `exclude` parameter.** Array of glob patterns for paths to omit from search — matched against each entry's base name and relative path. Matching directories are pruned from the walk (not just skipped); matching files are skipped. Examples: `["vendor", "*.pb.go", "testdata/**"]`.
- **Session names.** Every MCP session is now assigned a memorable two-word name on registration — e.g. `SWIFT-FALCON`, `TINY-OTTER`, `AZURE-NARWHAL`. The name is stored in the session JSON file, appears as a prefix in the TUI session list, and persists for the session's lifetime. Names are generated from curated SFW word lists (~80 adjectives × ~80 nouns ≈ 6 400 combinations). No agent cooperation required — the daemon assigns the name automatically.
- **`[edits].concurrent_write_skew_ms` config field.** Exposes the previously hard-coded 100 ms clock-skew allowance used by `edit_file`'s post-rename concurrent-write detector. Set higher on network mounts or FUSE filesystems where rename + stat latency can exceed 100 ms and trigger false positives. Configurable via `PLUMB_CONCURRENT_WRITE_SKEW_MS` env var. Default unchanged at 100 ms.
- **Claude Desktop smoke test checklist** added at `docs/claude-desktop-smoke.md`. Covers workspace resolution, `session_start`, `read_file` header, `edit_file` application, post-write diagnostics, MCP Prompts, memory resources, and session name display. Run after any significant change to the daemon or write-tool pipeline.
- **Pyright integration test.** `TestIntegration_DidChangeWatchedFiles` in `internal/lsp/adapters/pyright/` spawns a real `pyright-langserver` binary against `testdata/python-fixture/`, writes a broken `.py` file, sends `DidChangeWatchedFiles{FileCreated}`, and asserts pyright publishes at least one error diagnostic within 15 s. Gated with `//go:build integration`. Pyright adapter is now promoted from Experimental to Validated.
- **CI integration-test job.** A new `integration` job in `.github/workflows/ci.yml` installs `gopls` and `pyright-langserver`, then runs `go test -tags=integration -timeout=2m ./...` on every push and pull-request. `make integration-test` mirrors the same command locally.

### Removed
- **`--log-level` global flag removed.** The flag was broken by design: it only took effect when spawning a new daemon subprocess, had no effect on an already-running daemon, and was not propagated correctly. Use `plumb log-level <level>` to change the running daemon's log level at any time, or set `log_level` in `~/.config/plumb/config.toml` for a persistent default.

### Changed
- **TUI top rail controls.** The old vertical menu is now a compact section selector opened with `/`, navigated with arrows or `j`/`k`, selected with `enter`, and closed with `esc`. The TUI now uses `ctrl+h` for help and `ctrl+q` for quit, with the footer and help panel updated to match. The top rail also shows a `Tokens Saved` box beside daemon activity when there is enough horizontal room; it reports estimated token savings for calls recorded since the current daemon's first active session, matching the Activity widget's daemon-lifetime scope.
- **Stats are global again to match the singleton daemon.** `internal/stats` now writes to one database under `config.DataDir()/stats.db`; each `tool_calls` row carries `workspace`, `session_id`, and `session_name`. `plumb stats`, `session_start`, and TUI project/session views filter by row attributes instead of opening per-project database files.
- **`find_replace` description** now opens with "Grep-equivalent: find text across files with optional replacement." so agents searching for grep or content-search find the tool via semantic matching.

### Fixed
- **`find_replace` now uses the shared write-safety model.** Previously it wrote files directly with `os.WriteFile`/`os.Rename`, bypassing per-path locking, the rate limiter, strict-mode read tracking, symlink-aware writes, LSP `didChangeWatchedFiles` notification, and symbol-cache invalidation. All mutating paths now go through `safeWrite` and the shared `WriteDeps` helpers. Failed writes are surfaced in the returned result rather than silently skipped.
- **`plumb status` and `plumb config show` now resolve workspace roots** the same way the daemon does: walking up from the given path to find a `.plumb/` marker or language root marker (`go.mod`, `pyproject.toml`, etc.). Previously, both commands used the literal path, so running from a subdirectory returned no stats and the wrong config.
- **`find_files` traversal errors are now reported.** The walk error was previously discarded, hiding context cancellation, timeout, and filesystem errors behind a silent partial result. Errors are now returned or labelled as partial in the output.
- **`search_in_files` correctly continues after oversized lines.** A `bufio.Scanner` token-too-large error was causing the scanner to abort; lines after the oversized one were never scanned. Replaced with a `bufio.Reader`-based implementation that skips the oversized line and continues scanning.
- **Stats `OpenReadOnly` reports schema version clearly.** Databases older than the current stats schema opened read-only by `plumb stats` or the TUI now return a clear schema-version diagnostic instead of a raw SQL error about a missing column.
- **Config `Defaults()` and `LoadProject()` deep-copy maps and slices.** Returning `Config` by value shared the backing storage of `LSP` maps and nested slices across callers; mutating one load could corrupt another. `cloneConfig` and `cloneLSPConfig` helpers ensure all loads are isolated.
- **Symlink writes are now locked by their resolved target path.** The lock key was previously computed from the as-supplied path before symlink resolution. A write through a symlink and a write to the real path could therefore race on the same underlying file. Both paths now normalise to the resolved target before acquiring the lock.
- **MCP message reads are bounded explicitly.** The `bufio.Scanner` with a 1 MiB max-token limit could fail a valid large request at the transport layer. Replaced with a `bufio.Reader`-based reader that enforces the same limit but returns a structured JSON-RPC error when the ID is decodable, or a clear transport error otherwise.

## 0.5.29 — 2026-05-16

### Added
- **`plumb doctor`** — new health-check command. Runs a series of checks and reports pass/fail in a table: daemon reachable + version matches, language servers on PATH, MCP client registrations, global/project config parseable, stats DB readable. Pass `--workspace` to include per-project checks. Each failing check prints a one-line fix hint.
- **`expected_sha` parameter on `edit_file` and `transaction_apply`.** `read_file`'s output header now includes `sha256=<hex>` (over the full file, not the sliced excerpt). Pass the hash as `expected_sha` to `edit_file` or per-operation in `transaction_apply` for content-hash concurrency checks. Rejected edits return an `editLogicErr` with both the expected and current hashes so the agent knows to re-read. Stronger than `expected_mtime` — survives coarse-mtime filesystems, restore-from-backup, and `touch -d` aliasing.
- **MCP `instructions` field.** The `initialize` response now includes an `instructions` string that directs the model to call `session_start` as the first tool of every session. Clients (Claude Desktop, Claude Code, Gemini CLI, Cursor, etc.) inject this as a system-prompt-style hint — session orientation becomes automatic without per-client configuration. The text is sourced from `DefaultInstructions` in `internal/mcp/instructions.go`; pass a custom value via `ServerInfo.Instructions` or `"-"` to suppress. `internal/mcp/server_test.go` covers default, custom, and suppressed cases.

## 0.5.28 — 2026-05-16

### Added
- **Codex setup support.** `plumb setup codex` now registers plumb as a stdio MCP server in Codex's TOML config, preserving existing `mcp_servers` entries, backing up existing config files before modification, and honouring `CODEX_HOME` when resolving the config location. `plumb config show` now includes Codex in the MCP integration table.

### Fixed
- **Stats are now fully per-project.** The active stats schema no longer has a row-level workspace column; the `<workspace>/.plumb/stats.db` location is the workspace identity. `plumb stats`, the TUI, and `session_start` read the selected project DB directly instead of filtering by row-level workspace metadata, so moved checkouts keep their history. Stats inserts now return errors up to the daemon, which logs failed writes instead of silently dropping them.
- **Daemon singleton guarantee enforced via `flock(2)`.** Two `plumb serve` processes racing from a cold start (Claude Desktop / Claude Code launching multiple conversations simultaneously) could each observe "no daemon", each call `startDaemonProcess`, and end up with **two** daemons running on the same socket path. The second daemon ran `os.Remove(socketPath); net.Listen(...)` and quietly stole the path from the first, while the first kept serving its already-connected clients on the now-orphaned listener fd. Symptom: two `daemon: ready` messages in `daemon.log` 100–200 ms apart, sessions split across both processes, the TUI showing partial state (sessions registered against one daemon, stats written by the other). Fixed with two advisory file locks:
  - `~/Library/Caches/plumb/plumb.spawn.lock` — held briefly by `plumb serve` around the dial-or-spawn block. Concurrent serves now serialise on this lock; the first to acquire it spawns the daemon, the rest re-dial after release and connect to the existing one. Implemented as a non-blocking `flock` retry loop so `ctx.Done()` (Ctrl-C, parent exit) is honoured promptly.
  - `~/Library/Caches/plumb/plumb.daemon.lock` — held by `plumb daemon` for the lifetime of the process. Acquired non-blocking before the socket is opened; a second daemon (manual `plumb daemon` invocation, missed serve-side lock, whatever) sees `EWOULDBLOCK` and exits with `"another plumb daemon is already running"` instead of stealing the socket. Released automatically by the kernel on process exit (clean SIGTERM or crash) — the lock lives on the open file description, not the file itself, so there is no stale-lock cleanup problem. The `plumb.daemon.lock` file persists on disk as a zero-byte rendezvous point.
  - `internal/cli/lock.go` is the new home for both helpers. `daemon.go` and `serve.go` get tiny additions; the rest of the codebase is untouched. Test coverage in `internal/cli/lock_test.go`: parallel stress (20 goroutines, max concurrent holders == 1), serialised-waiters timing assertion, context-cancellation deadline, and a crash-recovery test that closes the fd directly to simulate process death.
- **No-LSP workspaces (`.plumb/` in JS/TS/Rust/Ruby/anything-not-Go-Python) now resolve fully.** A `.plumb/` marker in a non-Go/non-Python project used to leave the session permanently in `Folder=""` state: `workspacePool.Detect` returned an error because no enabled language matched, `startGopls` aborted before `session.Patch`, the TUI rendered `⟳ resolving…` indefinitely, and *every* tool call was dropped by `OnAfterTool` because the workspace lookup failed. JS/TS users running plumb via Claude Desktop saw the agent happily reading and writing files for hours with no stats and no TUI visibility. Three changes:
  - `pool.Detect` honours a `.plumb/` marker even when no LSP language is detectable, returning `(root, "none", nil)` instead of erroring. New constant `LanguageNone = "none"` documents the sentinel.
  - `handleConn`'s workspace-attach closure (renamed `startGopls` → `attachWorkspace` because it has handled pyright since 0.5.x and now handles no-LSP workspaces too) special-cases `LanguageNone`: skip `pool.acquireLang`, leave `sessionProxy`/`sessionInv` without a primary (LSP tools fail with the usual "LSP server not yet ready"), but still set `acquiredRoot`, call `session.Patch` with `Language="none"` and `Adapter=""`, and let `applyProjectConfig` load `<workspace>/.plumb/config.toml` as normal. Stats attribution now works for these workspaces.
  - TUI: the left-panel label drops the `language:` prefix for `"none"` (`/path/to/proj` reads cleaner than `none: /path/to/proj`), and the session-detail panel renders an empty Adapter field as `—` rather than blank.
  - Test coverage in `internal/cli/pool_test.go`: `.plumb/` alone in a non-Go/non-Python project returns `LanguageNone`; `.plumb/` plus `go.mod` returns `go`; `.plumb/` in parent with empty subtree returns `LanguageNone`; child `.plumb/` wins over ancestor `go.mod` for root selection but inherits language from the ancestor; "no markers anywhere" still errors. Tests use `os.MkdirTemp("", …)` directly rather than `t.TempDir()` to bypass `GOTMPDIR` and land outside the plumb source tree (otherwise `Detect` finds plumb's own `go.mod` as an ancestor and the assertions don't reflect real-world behaviour).

## 0.5.25 — 2026-05-12

### Changed
- **`plumb stats` CLI tables refactored** — replaced `text/tabwriter` and manual string formatting with `charm.land/lipgloss/v2/table`. The output now perfectly aligns columns, handles multi-line error traces inside cells natively, and applies the TUI theme colors for success (✓) and warning (✗) indicators.

### Fixed
- **Gemini CLI configuration path corrected.** `plumb setup gemini` now writes to `~/.gemini/settings.json` (the standard location) instead of the incorrect `~/.gemini/antigravity/mcp_config.json`.
- **`find_replace` could exceed the 4-minute MCP timeout on otherwise normal trees.** Three root causes, three fixes in `internal/tools/find_replace.go`:
  1. **Loaded entire files before checking if they were binary.** The old loop called `os.ReadFile(path)` then `looksLikeBinary(bytes.NewReader(data))` — so a 500 MB sqlite db or compiled artifact that wasn't `.gitignore`'d got fully buffered into memory only to be discarded. The scan now does the sniff up front: open the file, `io.ReadFull` the first 8 KB, bail on null byte, only then `io.ReadAll` the rest. `looksLikeBinary` was the only caller and is removed from `walk.go` (the `binarySniffBytes` constant stays).
  2. **No file-size cap.** Huge plain-text files (JSON dumps, generated SQL, lockfiles without null bytes) were scanned in full. New `max_file_bytes` arg, default 50 MiB, skips files past the cap via `os.Stat` before opening.
  3. **Sequential file loop.** Replaced with a `runtime.NumCPU()`-sized worker pool: paths fan out on an unbuffered channel, results return on a buffered channel, an atomic counter enforces `max_files` exactly (workers that lose the race bail before writing — so the cap is never exceeded even under contention), and `ctx.Cancel()` shuts the pipeline down cleanly. Output is sorted by path so the report is deterministic regardless of worker scheduling.
- **`find_replace` glob with a literal directory prefix now prunes sibling subtrees.** A glob like `src/**/*.go` can never match files outside `src/`, but the walker still descended into every directory. Added `globLiteralPrefix` and `dirCompatibleWithPrefix` helpers; the walk callback returns `fs.SkipDir` for directories that fall outside the prefix. Test coverage in `find_replace_test.go` includes a `wanted/` + `skipme/` tree to assert pruning.
- **Stats DB migration v1→v2 failed on every freshly-created database.** The baseline `CREATE TABLE` in `internal/stats/db.go` included `input_json` and `output_text` columns (the v3 state), and then the migration loop tried to `ALTER TABLE ADD COLUMN input_json` — duplicate column error. Two fixes: (a) the baseline schema is reverted to v1 (only the original columns); migrations bring it forward to v3. (b) Each migration is now idempotent — `hasColumn` is consulted before `ADD COLUMN`, so databases corrupted by the old buggy build (all columns present but `user_version=0`) recover cleanly on next open. Regression test `TestOpen_IdempotentOnUnstampedAllColumnsDB` seeds the broken state and asserts `Open` succeeds and stamps `user_version=3`.

### Added
- **`find_replace` tests:** parallel correctness across 200 files, exact `max_files` cap under parallelism, binary-skip with matches past the sniff window, deterministic output ordering, context cancellation, and `max_file_bytes` skip. Plus unit tests for `globLiteralPrefix` and `dirCompatibleWithPrefix`.
- **`search_in_files` now has the same guards as `find_replace`:** new `max_file_bytes` argument (default 50 MiB) skips outsized text files (logs, JSON dumps, lockfiles without nulls) before opening; glob literal-prefix directory pruning skips sibling subtrees so a glob like `src/**/*.go` never descends into `tests/`. Same playbook as the `find_replace` work, applied to the more-trafficked search path. Tests in `search_in_files_test.go`.
- **`find_files` matched updates:** 30 s default wall-clock deadline (matching `search_in_files`) so runaway walks over `$HOME` can't outlive the MCP timeout, and glob literal-prefix directory pruning when the pattern is a path-anchored glob. Tests in `find_files_test.go`.
- **Shared glob helpers:** `globLiteralPrefix` and `dirCompatibleWithPrefix` moved from `find_replace.go` to `walk.go` so all three walk-and-scan tools share one implementation.

### Changed
- **`VERSION` bumped to 0.5.25** so `make build` produces binaries that self-report the correct version.
- **`search_in_files` is now parallel** (`runtime.NumCPU()` workers). Phase 1 of the walk collects candidate paths (with glob, size, and ignore filtering still inline); phase 2 fans out per-file open + sniff + scan + format across workers. Output is sorted by path so the report is deterministic regardless of worker scheduling. Truncation semantics changed slightly: `max_results` is enforced at the *file boundary*, so the actual hit count may exceed `max_results` by one file's worth — the summary line now reports both file count and total hits (e.g. `12 file(s) matched, 247 hits`) and the truncation suffix reads `(truncated past N hits — narrow with glob or a tighter pattern)`. Tests in `search_in_files_test.go` cover the parallel path across 200 files.
- **Modernization in files touched this session.** Removed user-defined `min`/`max` helpers in `search_in_files.go` (Go 1.21+ has builtins). Converted `for i := 0; i < N; i++` to `for i := range N` in new test code (Go 1.22+). Converted `wg.Add(1) + go func(){ defer wg.Done(); ... }()` to `wg.Go(func(){...})` in `find_replace.go` and `search_in_files.go` (Go 1.25+). The CI is on 1.26.2 so all of these are supported.
- **End-to-end timeout regression test.** `TestFindReplace_LargeTreeFinishesQuickly` builds a tree with 300×50 KiB matching files plus a 20 MiB sibling decoy that the glob pruner must skip, then asserts the whole operation completes within a 10 s wall-clock budget. Observed on a laptop: ~60 ms (~167× headroom). Skipped under `-short` for slow CI hardware.
- **Documentation gap closed in README.md.** The `Configuration` section previously only documented `[edits]`. It now covers the top-level `log_level` / `log_file`, the `[cache]` section (`ttl`, `max_size`), the `[walk]` section (`refuse_home_roots` and its macOS TCC rationale), and the `[lsp.<lang>]` per-language blocks (`command`, `args`, `root_markers`, `enabled`, `env`). `plumb config show` still surfaces resolved values with provenance.

## 0.5.24 — 2026-05-12

### Fixed
- **TUI Call Detail popup box-width math was off-by-one** — `boxSection` in `internal/tui/model.go` computed `inner := rightWidth - 5` and `topFill := inner + 2 - len(topLabel)`, but the actual row geometry (`" │ " + padded(inner) + " │ "` = `inner+5` visible chars, fitting into `pRW-1 = rightWidth+1`) requires `inner = rightWidth - 4` and `topFill = inner + 1 - len(topLabel)`. The previous math left an extra column of right-margin slack and offset the top label, so the right border of the box didn't line up with the bottom border or with adjacent rows. Geometry comment updated to match the corrected derivation.

### Changed
- **`edit_file`'s "old_str not found" error now diagnoses the failure cause.** Previously the message said *"the file may have been modified, or the string is incorrect"* — a guess that forced the agent into a recovery loop of retrying with cosmetic snippet variations. The daemon already records every `read_file` mtime per session via `ReadTracker`, so it can tell the agent which it is: if the recorded mtime differs from the current mtime, the error says *"file has been modified since you read it"* and prints both mtimes; if the mtimes match, it says *"file unchanged since your read; the snippet is incorrect"* and asks the agent to verify character-by-character. Collapses what was often a three-tool-call recovery into one.
- **`edit_file` errors echo the post-normalisation form of `old_str` when CRLF normalisation transformed the input.** `matchLineEndings` can rewrite an LF snippet to CRLF (or vice versa) before searching; until now the error printed only the as-sent form, so any failure that involved line-ending normalisation looked like a phantom miss. The error now includes a `searched (after newline normalisation):` line whenever the searched form differs from what the agent sent. The ambiguous-match (`count > 1`) branch gets the same treatment.
- **`read_file` header now includes `indent=tabs|spaces|mixed|none`.** Many clients (Claude Desktop, Claude Code's code-block rendering) visually expand tabs to spaces, so the agent has no reliable way to tell whether the underlying file uses tabs or spaces for indentation — and a wrong guess produces a silent `edit_file` mismatch. The new field is computed in one pass over the returned body and lets agents author `old_str` with the right leading whitespace. Example: `# plumb-read mtime=2026-05-12T... indent=tabs`.

## 0.5.23 — 2026-05-12

### Fixed
- **`search_in_files` could hang the daemon for minutes** — `Execute` accepted `context.Context` but bound it to `_`, so client timeouts and cancellations had no effect. A single bad call (e.g. workspace resolved to `$HOME`, no `.gitignore` to prune, or a multi-megabyte single-line text file slurped into RAM and scanned) could outlive Claude Desktop's own 4-minute MCP timeout, leaving the daemon wedged on a goroutine the user couldn't reach. `Execute` now binds `ctx`, applies a 30s wall-clock budget when the caller has no deadline of its own, checks `ctx.Err()` per file in the walk callback, and surfaces a clear "search timed out (partial — narrow with path/glob)" summary instead of failing silently. `find_files` and `find_replace` had the same `_ context.Context` bug and got the same fix (cancellation plumbing, without a default deadline). `walk` and `walkDir` now take `context.Context` as their first arg and check cancellation on entry and on every directory iteration, so even a giant tree aborts within one entry.
- **`splitLines` silently truncated long-line files** — used the default `bufio.Scanner` 64 KB token cap, so any file with a line longer than 64 KB (generated/minified content) had the rest of the file silently skipped. Now sized to 1 MiB via `sc.Buffer(...)`.
- **TUI Call Detail "Args" panel reshuffled every render tick** — the popup iterated the unmarshalled `map[string]any` directly, and Go intentionally randomises map iteration order. Same call, same timestamp, but rows like `path`/`start_line`/`end_line` reordered on every poll, which looked like a refresh bug. Keys are now collected, `sort.Strings`-ed, and iterated in stable order.

## 0.5.22 — 2026-05-11

### Changed
- **Statistics and Recent panels redesigned** — sections renamed from "Tool Statistics"/"Recent Edits"/"Recent" to "Statistics" and "Recent". Section titles now highlight with `SelectedStyle` when that panel has focus instead of showing an inline hint text. Columns use 3-space gaps for better readability. **Recent** gains an `Errors` column showing the error message (truncated) for failed calls. Inline error expansion row removed. A blank line separates each section title from its table header.

## 0.5.21 — 2026-05-11

### Fixed
- **`json.Indent` buffer type** — `formatJSON` used `strings.Builder` as the destination for `json.Indent`, but the stdlib signature requires `*bytes.Buffer`. Fixed the buffer type and added the `bytes` import.

## 0.5.20 — 2026-05-11

### Fixed
- **Table alignment** — header, separator, and data rows now all use a consistent 2-char indent. The cursor `▸` replaces the 2-space prefix on the selected row, keeping columns pixel-perfect. The Recent table's extra 4-space indent (introduced in 0.5.19) is corrected to 2 spaces. The tool column in Recent now correctly includes `status + icon + name` as one fixed-width cell, so `Tool` in the header aligns with `✓ ▤ read_file` in data rows.

## 0.5.19 — 2026-05-11

### Changed
- **Tool Statistics and Recent rendered as tables** — both sections now use aligned fixed-width columns with a header row and `─` separator instead of free-form `fmt.Sprintf` strings.
  - **Tool Statistics**: columns `Tool · Calls · Avg · Errors`. Each tool row prefixed with a category icon: `✎` write/mutate, `▤` read/browse, `⊕` LSP/symbol, `◎` memory, `◇` git, `◈` diagnostics, `⟳` session, `▪` other.
  - **Recent Edits**: columns `Tool · Path · Dur · When`.
  - **Recent**: columns `Tool · Dur · When`. Same category icons alongside the `✓`/`✗` status.
  - Added `padRight`, `padLeft`, `truncate`, `toolIcon` helpers in `model.go`.

## 0.5.18 — 2026-05-11

### Fixed
- **Panic recovery in `OnInit` and `OnRootsChanged` goroutines** — `mcp/server.go` launches `OnInit` and `OnRootsChanged` as bare goroutines (`go s.OnInit(…)`). Any panic inside them (e.g. during workspace resolution via `roots/list`) crashed the daemon process silently — no log, no stack trace. Replaced with `go safeRun("OnInit", …)` which catches panics, logs them as `ERROR mcp: goroutine panic — daemon kept alive` with a full stack trace, and lets the daemon survive. Combined with the `handleConn` recovery from 0.5.17, every goroutine path that can crash the daemon is now protected.

## 0.5.17 — 2026-05-11

### Fixed
- **Daemon panic recovery** — the `wg.Go(handleConn)` goroutine now has a top-level `defer recover()`. Any unhandled panic inside an MCP connection (nil deref, LSP error, etc.) is caught, logged as `ERROR daemon: connection goroutine panic` with a full stack trace, and the daemon process stays alive to serve other connections. Previously a single bad connection would kill the whole daemon silently.
- **Daemon log moved to OS log directory** — log output now goes to `~/Library/Logs/plumb/daemon.log` on macOS and `$XDG_STATE_HOME/plumb/daemon.log` (fallback `~/.local/state/plumb/daemon.log`) on Linux. Previously it went to `~/Library/Caches/plumb/` which is a cache directory, not a log directory. The log path is printed in the startup `daemon: ready` line.

## 0.5.16 — 2026-05-11

### Added
- **Popup detail scrollbar** — the right panel of the call-detail popup now shows a vertical scrollbar (`╎` track / `┃` thumb) when the content overflows the visible height. The thumb position reflects the current scroll offset. No scrollbar is drawn when content fits without scrolling. Implemented via `scrollbarCol()` in `model.go`; the right panel content width is reduced by 1 to make room for the bar column.

## 0.5.15 — 2026-05-11

### Fixed
- **TUI shift+tab** — added reverse panel-focus cycling (Recent → Tool Stats → Sessions) to complement the existing forward Tab cycle. Hint text updated to show `tab/shift+tab`.

## 0.5.14 — 2026-05-11

### Changed
- **TUI hint/status colour** — replaced Nord4 `#D8DEE9` (too close to white) with `#7B8EA6`, a mid-tone blue-gray in the Nord tonal family. Sits clearly between the invisible dim-gray (terminal colour 8) and the harsh brightness of the Snowstorm whites. Readable at a glance, unmistakably muted.
- **Pending sessions now always visible in TUI** — sessions that haven't resolved their workspace yet (e.g. the live Claude connection at startup before `roots/list` completes) are no longer hidden behind the 'a' key. They appear immediately with a `⟳ resolving…` label in the left panel and update in-place once the workspace is determined. The 'a' key and `hiddenCount` banner (`N hidden (press 'a' to show)`) have been removed.

## 0.5.13 — 2026-05-11

### Changed
- **TUI status bar and hint text colour** — both the bottom status bar (`N session(s) · N tool calls · …`) and the top-right shortcut hint were using terminal colour `8` (dim gray), making them almost invisible on Nord-themed terminals. Both now use **Nord4 `#D8DEE9`** (darkest Snowstorm shade) — readable off-white that stays clearly distinct from the bright `#ECEFF4` of labels and panel content. The in-panel navigation hints (`(j/k navigate · enter popup · tab next)`) were updated from `MutedStyle` to `HintStyle` for the same reason. `MutedStyle` (colour `8`) is retained for genuinely secondary in-panel details (timestamps, sizes, separators). Added `StatusStyle` alongside `HintStyle` in `styles.go` for semantic clarity.

## 0.5.12 — 2026-05-11

### Added
- **Stats DB schema migrations (v1 → v3)** — `internal/stats/db.go` now has a real `migrate()` function that walks a `[]migration` slice and applies `ALTER TABLE` statements forward from the on-disk `PRAGMA user_version` to the current `SchemaVersion` (3). Any daemon upgrade that adds columns is now safe: older databases are migrated in-place on first `Open`, not silently overwritten.
- **`input_json` and `output_text` columns** in `tool_calls` (schema v2 and v3). The daemon's `OnAfterTool` callback captures the raw JSON args and the tool response text and stores them (capped at 64 KiB each). `RecentCall` exposes both fields; `Recent()` returns them.
- **`Filter.Tool` and `CallsForTool()`** — stats queries can now be scoped to a single tool name, enabling the popup to show all calls for a given tool across the workspace.
- **Full-screen call-detail popup** in the TUI — press `enter` on any Tool Statistics row or any Recent call to open a two-column overlay: left panel lists all calls for that tool (●/○ session indicators, timestamp, duration, ▸ cursor), right panel shows full detail (Args JSON pretty-printed, Output text, scrollable via `tab`). Navigate with `j`/`k`; close with `esc`.
- **Tool Statistics panel** is now fully navigable with `j`/`k` and shows all tools (not capped at 5). `Tab` cycles Sessions → Tool Stats → Recent → Sessions.
- **Recent Edits paths** — the Recent panel now extracts and shows the target file path from `input_json` for write-class tools (`write_file`, `edit_file`, `delete_file`, `rename_file`, `transaction_apply`).

### Fixed
- **Workspace not resolving from `list_files`** — the `root` JSON field was not read by `OnBeforeTool`/`workspaceFromArgs`. Added `Root string \`json:"root"\`` to both decode structs.
- **Workspace not resolving from `session_start`** — the `workspace` JSON field was similarly missing. Added `Workspace string \`json:"workspace"\`` to both decode structs.
- **Directory path passed to `Detect` walked to wrong parent** — `filepath.Dir("/path/to/workspace")` → `/path/to`, missing the project root marker. Fixed with an `isDir` check: if the seed path is itself a directory, use it directly as `startDir`; only call `filepath.Dir` for file paths.
- **`plumb stop` only killed one daemon instance** — `findDaemonPID()` returned on first match. Replaced with `findAllDaemonPIDs()` that collects all PIDs from PID file + `lsof` + `pgrep`, deduplicates via a `seen` map, and kills all of them with per-daemon "stopped" output.

### Documentation
- **`docs/todo.md`** — removed the now-complete "Stats DB migrator + `input_json` column" section; updated "The next two hours" sequence; bumped last-reviewed version to 0.5.12.

## 0.5.11 — 2026-05-11

### Added
- **Gemini CLI support** — added `plumb setup gemini` to register plumb as an MCP server in Gemini's configuration (`~/.gemini/antigravity/mcp_config.json`). New `plumb config gemini` shows integration status, and `plumb config show` now includes the Gemini config path in its provenance report.
- **Empty-config tolerance** — the setup logic now gracefully handles existing but empty (0-byte) configuration files (common after a fresh tool install) instead of failing with a JSON parse error.

### Documentation
- **AGENTS.md (and GEMINI.md symlink) updated** to include Gemini CLI as a primary supported assistant alongside Claude. Version bumped to 0.5.11.

## 0.5.10 — 2026-05-11

### Documentation
- **`docs/todo.md` reorganised by topic.** New top-level sections: **Architecture**, **Features**, **Improvements**, **Testing & verification**, **Bugs & known limitations**, **Considered and deferred**. Within each section, items are ordered by priority. The cross-cutting "next two hours" recommended sequence remains at the top of the file. Items themselves are unchanged in content — purely reshuffled for findability.
- **New top architecture item:** [Code-quality differential after edits](docs/todo.md#code-quality-differential-after-edits). Marked ⭐ top architectural priority. The pitch: after every plumb-initiated edit, run the file's offline code-quality analyser (golangci-lint for Go, ruff for Python, eslint for TS, ...) and append findings to the response — elevates plumb from "a better way to edit files" to "a code-review-loop in the inner agent loop". Phased delivery (Go-only Phase 1 → Python and beyond Phase 2 → async/advanced Phase 3) with full design notes including the proposed `internal/quality/Analyser` interface, new `[quality]` config block, and watch-out-fors (timeout budgets, false positives in legacy code, noise control). To be discussed before implementation begins.

## 0.5.9 — 2026-05-11

### Documentation
- **`plumb doctor` design captured in `docs/todo.md`** — a `brew doctor`-style discovery + health-check CLI that scans the host for MCP-capable clients (Claude Desktop, Claude Code, Gemini CLI, Cursor, Continue, …), shows config status for each, and surfaces system-level health (daemon running, gopls/pyright on PATH, version match, stats schema). Detection-only — does NOT auto-configure. Full design including check set, output format, file pointers for the implementation, known MCP client config locations table, and watch-out-for notes.

## 0.5.8 — 2026-05-11

### Documentation
- **`docs/todo.md` rewritten** so each outstanding item is self-sufficient — another session can pick it up cold without re-deriving the problem. Every entry now carries Priority, Effort, Why-it-matters, Definition-of-done, Where-to-start (with file paths and function names), and Watch-out-for sections. The "next two hours" recommended sequence is included verbatim at the top: pyright smoke test → Claude Desktop e2e → stats migrator + Recent Edits paths → expected_sha.
- Captures all 11 items from the 0.5.6 honest review under Production-blocking + Real gaps + Subtle things; additional "100 ms concurrent-write skew constant" subtle entry added so it's not lost.
- Workflow rule formalised: when you complete a TODO, delete the section from `docs/todo.md`, add a `CHANGELOG.md` entry, commit both changes together. If you can't finish, add a "Status:" note so the next person doesn't restart.

## 0.5.7 — 2026-05-11

### Documentation
- **New `docs/todo.md`** — canonical index of outstanding work, real gaps, and "subtle things to be aware of" footguns. Carries everything from the 0.5.6 honest review that isn't yet addressed (Claude Desktop e2e, pyright integration test, CI integration matrix, `expected_sha`, stats migrator + `input_json`, configurable post-write window, dirty-tree guard, transaction durable rollback). Grouped by priority (production-blocking → real gaps → subtle things → considered/deferred).
- AGENTS.md gains a short "Known limitations and pending work" section pointing at `docs/todo.md`, with the rule: complete-an-item → delete-its-section → add-CHANGELOG-entry in one commit.

## 0.5.6 — 2026-05-11

### Tests
- **End-to-end smoke test against real gopls** for `workspace/didChangeWatchedFiles`. The test copies the Go fixture into a temp workspace, initialises gopls, writes a syntactically broken `broken.go` to disk, sends `DidChangeWatchedFiles{FileCreated}`, and asserts gopls publishes error diagnostics within 5 seconds. **Passes in 1.2s** — proves the 0.5.0 architectural rewrite is load-bearing: gopls really does consume our notifications, the capability negotiation work in 0.5.1 #1 lets it register watchers, and the end-to-end loop is closed. Gated `//go:build integration` (requires `gopls` on `$PATH`).

This closes the last big "is this even working?" worry from the 0.5.0 review: the answer is yes, gopls is acting on plumb-initiated file changes.

## 0.5.5 — 2026-05-11

### Documentation
- **AGENTS.md fully refreshed** for the 0.5.x line. Now reflects: 33 tools (was 28); `WriteDeps` pattern for adding write tools; per-project `[edits]` config layer; capability negotiation via `client/registerCapability`; per-session `ReadTracker`; per-path locks; rate limit + strict mode env/config layers; `plumb config show`; daemon version-mismatch warning; cold-start workspace chain; quick-reference patterns for agents.
- **README.md rewritten** for the 0.5.x line. New tool tables; expanded file-write safety section covering all eight layers (per-path lock, atomic rename, symlink-aware, uniqueness + CRLF, expected_mtime, strict mode, retry, LSP notify); `[edits]` config block; `plumb config show`; updated TUI screenshot showing the Recent Edits panel.
- **docs/mcp-tools.md** updated: corrected the introduction (was describing the obsolete `didOpen`/`didChange`/`didClose` write notification, now `workspace/didChangeWatchedFiles`); rewrote `write_file` and `edit_file` sections to cover all current safety layers; added sections for `delete_file`, `rename_file`, `transaction_apply`; refreshed `read_file` with the mtime-header contract; documented `read_multiple_files` parallelism and `list_directory` glob filter; updated `session_start` with the four-step cold-start chain and the 200-line context.md cap. Note added pointing readers to AGENTS.md for the full tool list.

## 0.5.4 — 2026-05-11

### Added
- **`plumb config show` subcommand** — prints the resolved configuration with source provenance ("from env (PLUMB_X=…)", "from project config", "from global config", "default") for each settable field, plus the paths of the global and project config files with existence flags. Pass `--workspace <dir>` to merge a specific project's config. Use for "why is strict mode on?" diagnostics.

### Changed (refactor)
- **`WriteDeps` struct replaces the multi-arg write-tool constructors.** `write_file`, `edit_file`, `delete_file`, `rename_file`, `transaction_apply` now all take a single `WriteDeps{Client, Cache, Diag, Limiter, Strict, Reads}` instead of 4–6 positional parameters. Stops the constructor sprawl that was making each new cross-cutting concern an N-place change. Test setups can pass `WriteDeps{}` (everything nil-safe).
- **Per-session `ReadTracker` replaces the process-global `readMtimes` map.** Strict mode is now correctly isolated between MCP sessions: session A reading a file no longer satisfies session B's strict-mode check. `NewReadFile` takes a `*ReadTracker`; the daemon creates one per connection.

### Fixed
- Cross-session strict-mode interference (known gap in 0.5.1/0.5.2 release notes) is closed.

## 0.5.3 — 2026-05-11

### Fixed
- **Session adapter no longer hardcoded to `"gopls"`** — sessions now register with empty Language/Adapter and have both filled in by `startGopls` once the project's language is detected. Python workspaces correctly show `adapter: pyright` from now on; the TUI shows `(resolving workspace…)` until the language is known.
- Dead-code cleanup in `daemon.go`: removed unused `findGoModRoot`, `findProjectRoot`, `goModRootForDir` (all superseded by `pool.Detect`).
- Style modernizations: `errors.AsType[T]` instead of `errors.As`+addressing, `fmt.Appendf` instead of `[]byte(fmt.Sprintf(...))`, `wg.Go(fn)` instead of `wg.Add(1)`+goroutine+`wg.Done`. All compiled out identically; cleaner reading.

### Added
- **`stats.db` schema versioning** — `PRAGMA user_version` is stamped to `1` on every Open. Future schema changes will compare-then-migrate. `DB.CurrentSchemaVersion()` reads the on-disk value (0 for pre-0.5.3 databases).

## 0.5.2 — 2026-05-11

### Added
- **Per-project config overrides** — `<workspace>/.plumb/config.toml` is now loaded on top of the global `~/.config/plumb/config.toml` (XDG-respectful) once the daemon resolves a workspace. Only the fields the project file sets are overridden; the rest inherit from global. New `[edits]` section controls write-tool safety:
  ```toml
  [edits]
  strict = true                    # require read_file before edit_file
  rate_limit_per_minute = 30       # 0 disables; default 120
  ```
  Precedence: defaults → global config → project config → env vars (`PLUMB_STRICT_EDITS`, `PLUMB_WRITE_RATE_LIMIT`). Env vars remain the highest layer for emergency overrides without editing files.

### Changed
- `strict_mode` and the rate limit no longer require an environment variable. They read from the resolved config; env still works as the highest-precedence override.
- `RateLimiter` gained `SetLimit(int)` so the daemon can adjust the running limit when project config loads after session start.

## 0.5.1 — 2026-05-11

### Added (9/9)
- **`transaction_apply` tool — atomic multi-file edits** — applies str_replace edits across up to 50 files as a single transaction. Phase 1 validates every operation in memory; if any old_str is missing/ambiguous or any expected_mtime mismatches, NO files are written. Phase 2 writes each file via safeWrite under per-path locks acquired in lexical order (deadlock-safe); if a write fails partway, already-written files are rolled back to their pre-transaction content. Phase 3 fires `didChangeWatchedFiles` and invalidates the symbol cache per file. Each operation consumes one rate-limit slot. Use for refactors that must land as a unit (cross-file string rename, coordinated config + caller updates).

### Added (8/9)
- **TUI "Recent Edits" panel** — distinct section in the right panel showing the last 5 write-tool calls (`write_file`, `edit_file`, `delete_file`, `rename_file`) for the selected session. Surfaces "what did Claude touch?" without scanning the full recent-calls list.

### Added (7/9)
- **Per-session write rate limit** — `RateLimiter` (sliding window) gates `write_file` / `edit_file` / `delete_file` / `rename_file`. Default 120 writes per minute per session; configurable via `PLUMB_WRITE_RATE_LIMIT` (0 disables). Protects against runaway-loop scenarios in autonomous agents; transparent under normal use. New constructor parameter on the four write tools; the daemon installs one limiter per connection.

### Added (6/9)
- **Post-write diagnostics in `write_file` and `edit_file` output** — after a successful write, the tools snapshot the current diagnostics for the URI, then poll for up to 300ms looking for a change. Any new errors/warnings (up to 3 of each) are appended to the response so the agent learns immediately whether it broke the build, without a follow-up `diagnostics` call. Skipped when the diagnostics source is nil (test setups).

### Added (5/9)
- **Strict-mode mtime auto-tracking** — set `PLUMB_STRICT_EDITS=1` to require every `edit_file` target to have been read in this daemon's lifetime AND for the file's current mtime to match what was observed at read time. Catches the "agent edits without reading first" footgun and silent-overwrite scenarios when an external process modifies the file. Read mtimes are recorded in a process-global map; cross-session interference is possible but acceptable for an opt-in safety mode. Off by default.

### Added (4/9)
- **Watched-files unit tests for both adapters** — gopls and pyright now have explicit test coverage for `DidChangeWatchedFiles` wire transmission with `FileChanged` / `FileCreated` events. Catches regressions if the adapter wiring breaks. (A real-process integration smoke test against running pyright is not part of this release because pyright availability isn't guaranteed in all environments; the unit-level test confirms the LSP wire format is correct.)

### Added (3/9)
- **`session_start` roots/list fallback** — when the daemon hasn't yet resolved a workspace, `session_start` now queries the MCP client via `roots/list` to discover one before falling back to the cwd walk. New `RootsResolver` parameter on `NewSessionStart`; the daemon constructs one that uses the captured `RequestFn` from `OnInit`/`OnRootsChanged`. Properly fixes Claude Desktop cold-start where the daemon launches from `$HOME` and the cwd walk can't find the project.

### Added (2/9)
- **Direct cache invalidation on writes** — `write_file`, `edit_file`, `delete_file`, and `rename_file` now evict matching entries from the symbol cache (`cache.InvalidateByPath`) immediately after a successful write. Closes the stale-symbol window without depending on gopls's diagnostic-republish timing. Constructors gained an optional `*cache.Cache` parameter (`nil` is safe for tests).

### Added (1/9)
- **LSP server-request handling** — `jsonrpc.Conn` now dispatches server-initiated requests through a registered `RequestHandler`, sending back JSON-RPC responses (or `-32601 method not found` if no handler is registered). Closes the gap where `client/registerCapability` requests from gopls were silently dropped.
- **`workspace.didChangeWatchedFiles.dynamicRegistration` advertised** — `DefaultClientCapabilities` now declares this capability so gopls actually registers its file-watcher globs and consumes our `workspace/didChangeWatchedFiles` notifications. Makes the 0.5.0 LSP rewrite load-bearing.
- **gopls + pyright accept `client/registerCapability`** — both adapters register a default request handler that responds OK to `client/registerCapability` / `client/unregisterCapability` and method-not-found to anything else. Unregistered methods are answered politely so the server can decide what to do.

## 0.5.0 — 2026-05-11

### Changed (architectural)
- **LSP notification primitive rewritten** — write tools now use `workspace/didChangeWatchedFiles` (one notification per write) instead of the prior `didOpen`/`didChange`/`didClose` lifecycle dance. The new primitive is the LSP-correct way to tell a server about external file changes: no buffer-ownership claim, no version counter abuse, no language-ID guessing. `langIDForPath` (~60 lines) deleted. Plumb now declares `FileCreated`, `FileChanged`, or `FileDeleted` explicitly per event. Protocol types, gopls + pyright adapter methods, routing-proxy fanout, and test mocks added across the LSP layer.

### Added
- **`delete_file` tool** — atomic file delete with `FileDeleted` notification.
- **`rename_file` tool** — atomic move/rename with paired `FileDeleted` + `FileCreated` notifications. Deadlock-safe two-path locking. Distinct from `rename_symbol` (LSP-semantic identifier rename).
- **Per-path lock** — process-global `sync.Map[path]→Mutex` serialises concurrent `write_file` / `edit_file` / `delete_file` / `rename_file` calls to the same path. Two parallel sessions can no longer interleave reads and writes on the same file.
- **`expected_mtime` on `edit_file`** — optional RFC3339Nano timestamp (typically copied from `read_file`'s output header). When supplied, the edit is rejected if the file's current mtime differs — optimistic-concurrency guarantee that the agent is editing the same revision it read.
- **mtime header on `read_file` output** — every response begins with `# plumb-read mtime=<RFC3339Nano>` so the agent can thread it back as `expected_mtime`.
- **Line-change summary in `edit_file` output** — response now includes the new mtime and a compact `lines changed: L12-15, L45` summary so the agent can verify changes without a follow-up read.
- **CRLF tolerance in `edit_file`** — line endings in `old_str` are normalised against the file before matching, so an LF `old_str` matches a CRLF file (and vice versa).
- **Symlink-aware `safeWrite`** — writes to a symlinked path follow the link to the real target instead of replacing the symlink with a regular file.
- **Pre-rename mtime check in `edit_file`** — if the file changes between our read and our rename, the attempt is surfaced as retryable rather than silently overwriting.
- **Daemon version-mismatch warning** — the daemon publishes its build version to `<runtime>/plumb.version` on start. `plumb serve` reads it and prints a stderr warning if the connected daemon's version differs from the binary that's launching ("run `plumb stop` to refresh").
- **`session_start` enhancements** — now includes current git branch, 3 most recent commits, 5 most recently-modified files (workspace-relative, skipping `.git`/`node_modules`/`vendor`/etc.). `context.md` cap raised from 80 → 200 lines. Cold-start fallback walks up from `os.Getwd()` for project markers when the daemon hasn't resolved a workspace.
- **Glob filter on `list_directory`** — optional `pattern` parameter (e.g. `*.go`) for consistency with `list_files`.

### Fixed
- **`list_directory` modified times** — the column was collected but never rendered. Now visible alongside name and size.
- **`read_file` streaming line ranges** — when `start_line`/`end_line` are set, `bufio.Scanner` stops at `end_line` instead of reading the whole file into memory and slicing.
- **`read_file` binary detection** — refactored to use `io.MultiReader` instead of read-seek-reread (works on pipes/devices that don't support seeking).
- **`read_multiple_files` parallelism** — now reads up to 8 files concurrently and propagates the parent context. Previously serial and passed `nil` ctx to the inner reader.
- **`write_file` schema cleanup** — removed the dead-code empty-content double-unmarshal hack; `content` is schema-required and enforced by the MCP layer.

### Known gaps
- `client/registerCapability` is silently dropped by the jsonrpc `Conn` (no server-request handler). Functionally OK for gopls — it has built-in static watchers for `.go` files and consumes our notifications via that path — but full LSP correctness needs the conn-level request handler plus `dynamicRegistration` declared in client capabilities. Slated for 0.5.1.
- Symbol cache (`internal/cache`) is invalidated only indirectly via the existing `Invalidator` that listens for `publishDiagnostics`. After a write, the cache stays stale until gopls re-publishes. Direct eviction from the write tools is the right fix; deferred to 0.5.1.
- Pyright `didChangeWatchedFiles` is wired but untested in this release.

## 0.4.1 — 2026-05-11

### Added
- **`write_file` tool** — creates or overwrites a file atomically. Content is staged in a temp file in `os.TempDir()` (no project-tree noise) then renamed into place. If the system temp dir and target are on different filesystems (EXDEV), falls back to a `.plumb.tmp` sibling automatically. Preserves existing file permissions. Notifies the LSP server via `didOpen`/`didChange`/`didClose` after writing so diagnostics and symbol lookups reflect the new content immediately.
- **`edit_file` tool** — applies one or more str_replace edits to an existing file with a four-layer safety model: (1) uniqueness lock — each `old_str` must appear exactly once, rejecting absent or ambiguous matches cleanly; (2) in-memory application — all edits applied before any write, file untouched on any failure; (3) atomic write — staged in `os.TempDir()` + rename, EXDEV-fallback to sibling; (4) concurrent-write retry — after the rename, plumb re-stats the file and retries the edit up to 3 times if a third-party write is detected. LSP notification on success.
- **`read_multiple_files` tool** — reads up to 20 files in a single call. Per-file errors reported inline.
- **`list_directory` tool** — immediate directory contents with `[FILE]`/`[DIR]` type prefixes, file sizes, and modification times. Sortable by name, size, or modification time.
- **`DaemonVersion` in session info** — `session.Info` now carries the daemon's version string. The TUI right panel shows it as a `Daemon` row so sessions from different daemon versions are distinguishable.

### Changed
- File write safety model revised: temp files go to `os.TempDir()` rather than a sibling `.plumb.tmp`, keeping the project tree clean. EXDEV cross-device rename is handled transparently by falling back to the sibling approach only when needed.
- `file_write_helpers.go` centralises `safeWrite`, `notifyLSP`, and `langIDForPath`. `safeWrite` returns a `writeResult` carrying timestamps used for concurrent-write detection in `edit_file`.

## 0.4.0 — 2026-05-11

### Added
- **`read_file` tool** — reads the text contents of any file by absolute path or `file://` URI. Supports `start_line`/`end_line` for slicing large files without loading them entirely. Binary files are detected and rejected. Output is capped at 200 KiB with guidance to use line ranges for larger files. Fixes the hard ceiling Claude Desktop users hit when navigating to a file but being unable to open it.
- **`session_start` tool** — bootstrap tool designed to be called first in every session. Returns in one round-trip: workspace path and auto-detected language, first 80 lines of `.plumb/context.md`, all memory names and descriptions, top-5 most-used tools from session history, and active LSP errors and warnings. Eliminates the "starts blind" problem on Claude Desktop where no filesystem access is available without tools.
- **`context.md` as MCP resource** — `.plumb/context.md` is now exposed as `plumb://workspace/context` via the MCP resource system, appearing as the first entry in the resources panel (above memories). Claude Desktop users can attach project context with one click.
- **MCP Prompts** — three named workflows surfaced as buttons/menu items in Claude Desktop:
  - **`orient`** — calls `session_start` and delivers a structured 4-point project summary.
  - **`whats-broken`** — chains `session_start` → `diagnostics` → `read_file` per broken file → triage and suggested fixes.
  - **`recent-changes`** — chains `session_start` → `git log` → `git diff --stat` → `diagnostics` for a recent-activity summary.
  All three accept an optional `workspace` argument; `recent-changes` also accepts `since` (e.g. `'1 week ago'`, a commit SHA).

## 0.3.3 — 2026-05-11

### Fixed
- **`plumb stop` now stops all daemon instances** — previously only one process was killed per invocation; users had to run `plumb stop` multiple times if multiple daemon processes were running. The command now collects all PIDs from all three lookup strategies (PID file, lsof, pgrep) and stops each one. Output is more verbose: prints each PID being stopped and reports upfront when multiple daemons are found.
- **Workspace not resolving from filesystem tool calls** — `list_files`, `find_files`, and `search_in_files` calls were not triggering workspace resolution, leaving sessions stuck in "pending" state in the TUI. Two causes: (1) `list_files` passes its directory as `root` but `OnBeforeTool` only read `path`; (2) when a tool passes a directory path, `filepath.Dir` was stripping the last component and causing `Detect` to start from the parent, missing the project root marker. Both fixed.

## 0.3.2 — 2026-05-11

### Added
- **`include_doc_comment` flag on symbol edits** — `insert_before_symbol`, `replace_symbol_body`, and `safe_delete_symbol` now accept an optional `include_doc_comment` boolean. When true, the operation extends to cover any contiguous comment lines (`//`, `#`, `/*`, `*`) directly above the symbol declaration. Lets you replace a function together with its doc comment, delete a function without orphaning its comment, or insert a new declaration above an existing doc comment instead of between the comment and its symbol. Default is false (backwards-compatible).
- **Stats `RecentCall` now surfaces `ErrorMsg`, `InputBytes`, `OutputBytes`** — the columns were already stored on every call but the read path discarded them. `plumb stats` recent-calls table now prints failed-call error messages on a continuation line below the row, and the TUI uses the same data for inline error expansion.
- **TUI keyboard navigation of recent calls** — press `tab` to move focus to the right panel's recent-calls list; `j/k`/`↑↓` then scrolls within it. The selected row is marked with `▸`, and for failed calls the error message expands inline below the row (wrapped to panel width). `tab` again returns focus to the sessions list. Footer hint updated.

### Changed
- **`find_symbol` is now single-file only** — the `uri` parameter is required. Previously, calling without `uri` ran a workspace-wide search that was a byte-identical duplicate of `workspace_symbols` (same LSP call, same cache key, same output format). After the split, `find_symbol` and `workspace_symbols` have clearly distinct purposes: file-scoped (case-insensitive substring against the document symbol tree) vs. workspace-scoped (LSP `workspace/symbol`, fuzziness depends on the language server).

### Fixed
- **TUI "No calls recorded yet" stuck on a session that does have calls** — `dbFor()` was caching the `nil` returned by `OpenReadOnly` when the per-project `<workspace>/.plumb/stats.db` didn't yet exist. Once cached, subsequent polls returned that nil even after the daemon created the file. The cache now only stores non-nil handles, so the TUI picks up writes as soon as they begin.

## 0.3.1 — 2026-05-11

### Changed (architecture)
- **Per-project stats DB** — moved from a single global `~/.local/share/plumb/stats.db` to per-workspace `<workspace>/.plumb/stats.db`. Plumb is for the projects you're working on now, not a multi-project history archive. `plumb stats` defaults to the current directory's DB; `--workspace <path>` targets another project. Old global DB stays in place but is no longer written; safe to delete.
- **TUI hides unresolved sessions by default** — dormant Claude Desktop connections that never resolved a workspace no longer clutter the dashboard. Press `a` to show them; the footer reports the hidden count.
- **`plumb sessions` filters too** — pending sessions hidden unless `--all` is passed.

### Added
- **`plumb diag` alias** for `plumb diagnostics`.
- **`plumb diag` actually works** — without a file argument, walks every Go file in cwd and aggregates per-file diagnostics (gopls only emits for files it's seen via `didOpen`, so we explicitly request each one).
- **CLI diag prints session/workspace banner** so you know which daemon session produced the output.

### Fixed
- **Per-file diag distinguishes "clean" from "not tracked"** — a file gopls has analysed reports "clean"; an unanalysed file says "not yet tracked" with a hint to open it.
- **`find_references` sends `didOpen` first** — eliminates shifted line numbers when gopls hasn't seen the file via its in-memory view.
- **`workspace_symbols` and `find_symbol` post-filter dependency-cache hits** — drops symbols from `/pkg/mod/`, `/usr/local/go/src/`, etc. Workspace-only results.
- **`find_files` schema** — clarified that `*` matches everything (a literal `.` only matches a file named `.`).

## 0.3.0 — 2026-05-11

### Added
- **MCP resources** — memories are now exposed as MCP resources (`resources/list`, `resources/read`) so Claude Desktop's resources panel surfaces them as browseable markdown artifacts. URI scheme: `plumb-memory://<name>`.
- **Multi-language support** — pool now starts the right adapter (`gopls` for Go, `pyright` for Python) based on which root marker the workspace contains. Python is shipped disabled-by-default in config; enable via `~/.config/plumb/config.toml` and ensure `pyright-langserver` is on `$PATH`.
- **`search_memories` tool** — grep across all memories in a workspace (smart-case, regex, with file:line locators).
- **`relevant_memories` tool** — return memories whose frontmatter `paths:` globs match a given file. Memories can declare `paths: internal/auth/**` to auto-attach to relevant areas.
- **Tokens-saved metric** — each tool call's estimated savings are aggregated per tool and globally; shown in `plumb stats` (SAVED column) and the TUI footer.
- **Multi-workspace LSP routing (carried over from 0.2.2)** — each tool call routes to the gopls/pyright for the workspace containing its URI. Cross-project queries within one MCP connection just work.

### Changed
- Pool now keys on root and selects adapter by detected language; `acquireLang` is the new direct API.
- `findProjectRoot` superseded by `pool.Detect(start)` which returns `(root, language, error)`.
- TUI footer shows tokens-saved estimate alongside session and call counts.

### Internal
- `internal/memory/provider.go` — implements `mcp.ResourceProvider`.
- `internal/stats/savings.go` — per-tool alternative-cost multipliers and `TokensSaved()` helper.
- Routing proxy reuses the pool's multi-language detection.

## 0.2.2 — 2026-05-11

### Added
- **Multi-workspace LSP routing** — `routingProxy` / `routingInvProxy` dispatch each LSP call to the workspace containing its URI. Connection still has one "primary" workspace as fallback for URI-less methods.
- `pool.lookup(root)` — read-only entry lookup used by diagnostics routing without spinning up new gopls.
- `workspaceFromArgs(args)` — daemon helper that captures the per-call workspace for stats attribution.

### Changed
- Stats are now attributed to the call's own workspace, not the connection's primary.

## 0.2.1 — 2026-05-11

### Added
- **Edit tools** (read-only philosophy relaxed):
  - `rename_symbol` — LSP semantic rename
  - `replace_symbol_body` — replace a symbol's full declaration
  - `insert_before_symbol`, `insert_after_symbol` — add code around symbols
  - `safe_delete_symbol` — delete only if no references remain
  - `find_replace` — text/regex search-and-replace across files, dry-run by default
- **Onboarding** — `plumb init --discover` auto-detects build system, entry points, test layout, seeds `.plumb/context.md`.

### Internal
- `internal/tools/edit_apply.go` — `applyWorkspaceEdit`, `applyTextEditsToFile`, `findSymbolByPath`.

## 0.2.0 — 2026-05-11

### Added
- **Memory system** — `list_memories`, `read_memory`, `write_memory`, `delete_memory`. Persistent markdown notes at `<workspace>/.plumb/memories/<name>.md` with optional YAML frontmatter.
- **`plumb diagnostics [file]`** — debug LSP diagnostics from the terminal.
- **`version` MCP tool** — server identity for bug reports.
- Architecture doc updated with daemon, session registry, persistence layout, stats schema, memory store.

### Fixed
- TUI per-session stats filter by `session_id`, not workspace (fixes "all sessions show same stats").
- Sessions register immediately on connect (was: 8-minute lazy wait until first LSP call).
- Workspace resolves from `path` argument (filesystem tools now seed the workspace).
- `plumb stop` finds the daemon via three-stage lookup: PID file → `lsof` → `pgrep -f "plumb daemon"`.

### Internal
- `internal/memory/` package.
- `internal/cli/mcpclient.go` — reusable MCP client for future CLI commands.
- Stats `Filter` struct — query by workspace, session_id, or both.
