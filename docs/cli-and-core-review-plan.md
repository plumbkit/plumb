# CLI and core review plan

This document captures the review findings from the CLI/core pass and turns
them into a staged remediation plan. It is intentionally separate from
`docs/todo.md`: the goal is to preserve the full review context before deciding
which items should become backlog tasks or immediate fixes.

## Scope

Reviewed areas:

- CLI workspace-facing commands: `status`, `config show`, setup/config
  presentation.
- MCP daemon registration and tool wiring.
- Write-tool safety contracts.
- Filesystem traversal tools.
- Stats database read paths.
- Config loading and merge behaviour.
- Build, lint, and local developer ergonomics.
- Code size and repetition hotspots.

Not reviewed in depth:

- TUI behaviour and layout. The TUI was only considered where CLI/TUI helper
  duplication affects maintainability.
- Site assets and marketing pages.
- New feature design beyond fixes implied by the review.

## Current baseline

The following checks were run during review:

```sh
go test ./...
go test -race ./internal/tools ./internal/cli ./internal/stats ./internal/cache
golangci-lint run ./...
```

Results:

- `go test ./...` passed.
- Targeted race tests passed.
- `golangci-lint run ./...` failed before linting code because the installed
  lint version rejected `.golangci.yml` with `unsupported version of the
  configuration: ""`.

## Reconciliation against current `main`

Reviewed again against commits through `0f278f9` before implementation.

- `edits.post_write_diagnostics_ms` is already wired through `WriteDeps` and
  daemon setup, so the old "wire post-write diagnostic timing" item is stale.
- `log_format` is already validated, displayed by `config show`, and wired into
  daemon/control-socket logging setup, so the old "finish or remove log_format"
  item is stale unless a later review finds a specific bug.
- `plumb log-level` exists and validates inputs; do not include a new log-level
  implementation item unless a fresh regression is found.
- The CLI smoke command is `go run ./cmd/plumb version`; `go run . version` is
  not the project entrypoint.
- The lint command used by the Makefile is `golangci-lint run`, not
  `golangci-lint run ./...`.
- Lint tooling needs to be fixed before the behavioural changes so later
  commits can distinguish toolchain failures from code findings.

## Priority 0: preserve write safety invariants

### 1. Bring `find_replace` under the write-tool safety model

Problem:

`find_replace` can mutate many files with `dry_run=false`, but it is registered
without `WriteDeps` and writes directly with `os.WriteFile` plus `os.Rename`.
That bypasses the guarantees that the project expects from write-capable tools:

- per-path locking;
- rate limiting;
- strict-mode read tracking where applicable;
- symlink-aware writes;
- LSP `didChangeWatchedFiles` notification;
- symbol cache invalidation;
- post-write diagnostics;
- consistent error reporting;
- all-or-nothing or clearly reported partial behaviour.

Relevant files:

- `internal/cli/daemon.go`
- `internal/tools/find_replace.go`
- `internal/tools/file_write_helpers.go`
- `internal/tools/write_file.go`
- `internal/tools/edit_file.go`

Plan:

1. Change `NewFindReplace` to accept `WriteDeps`, or introduce a narrower
   dependency bundle if `find_replace` should not require strict read tracking.
2. Decide the intended semantics for multi-file writes:
   - Conservative option: keep dry-run default, and when writing, validate all
     candidate replacements first, then write with per-file locks.
   - Stronger option: implement rollback behaviour by reusing or adapting
     `transaction_apply` internals.
3. Replace direct `os.WriteFile`/`os.Rename` calls with shared safe-write
   helpers.
4. Notify LSP and invalidate caches per changed file.
5. Surface write failures instead of silently skipping failed files.
6. Add tests for:
   - dry-run unchanged behaviour;
   - `dry_run=false` notifies LSP;
   - write failures are reported;
   - `max_files` cancellation does not leave ambiguous output;
   - symlink behaviour matches `write_file`/`edit_file`;
   - rate limiter is consumed for actual writes.

Acceptance criteria:

- All mutating paths in `find_replace` use the same safety primitives as other
  write tools.
- Failed writes are visible in the returned result or error.
- Existing dry-run UX remains compatible.

## Priority 1: make CLI workspace resolution match daemon semantics

### 2. Resolve workspace roots for `plumb status`

Problem:

`plumb status` uses the literal cwd, or the literal `--workspace` value, as the
workspace filter for the global stats DB. If the user runs it from a
subdirectory, it reports no stats even when the project root has matching rows.

Relevant files:

- `internal/cli/stats.go`
- `internal/cli/workspace_pool.go` or the workspace detection equivalent
- `internal/stats/db.go`

Plan:

1. Extract a CLI-safe workspace resolver that walks up using the same marker
   rules as the daemon without starting LSP processes.
2. Use that resolver in `runStats`.
3. Preserve explicit `--workspace /tmp` behaviour where `/tmp` is intentionally
   the inspected directory and no project marker exists.
4. Make the displayed path clear:
   - inspected path;
   - resolved workspace root;
   - global stats DB location.
5. Add tests for:
   - cwd at workspace root;
   - cwd in a nested subdirectory;
   - explicit nested `--workspace`;
   - explicit non-project directory.

Acceptance criteria:

- `plumb status` and daemon stats agree on the project root for normal projects.
- The no-stats message remains accurate for real non-project paths.

### 3. Resolve workspace roots for `plumb config show`

Problem:

`plumb config show --workspace <subdir>` loads `<subdir>/.plumb/config.toml`
instead of the workspace root config. This makes CLI config inspection disagree
with daemon/session behaviour.

Relevant files:

- `internal/cli/config.go`
- `internal/config/config.go`

Plan:

1. Reuse the same CLI workspace resolver from the status fix.
2. Show both the requested path and resolved workspace when they differ.
3. Update provenance labels so project config points at the resolved root.
4. Add tests mirroring the `status` workspace-resolution cases.

Acceptance criteria:

- `plumb config show` reports the config the daemon would use for the same
  project.

## Priority 2: restore developer tooling reliability

### 4. Fix the lint toolchain contract

Problem:

Local `golangci-lint run ./...` fails before running because the config format
is incompatible with the installed/current lint version. CI uses `version:
latest`, so it can break unpredictably as lint releases change.

Relevant files:

- `.golangci.yml`
- `.github/workflows/ci.yml`
- `Makefile`
- `AGENTS.md`

Plan:

1. Decide whether to pin golangci-lint to a known compatible version or migrate
   `.golangci.yml` to the current schema.
2. Prefer pinning CI to a specific version even after migration.
3. Update local documentation to name the expected lint version.
4. Run:

   ```sh
   golangci-lint run ./...
   make lint
   ```

Acceptance criteria:

- Local lint and CI lint use compatible versions.
- Lint failure means code issues, not config loading failure.

### 5. Remove or relocate root scratch executable

Problem:

`test_table.go` at the repository root defines `package main`. As a result,
`go run . version` runs a table demo instead of the real CLI.

Relevant file:

- `test_table.go`

Plan:

1. Delete the file if it is disposable scratch work.
2. If the table demo is still useful, move it under an ignored examples or
   experiments path with a clear build tag.
3. Add a quick smoke check:

   ```sh
   go run ./cmd/plumb version
   ```

Acceptance criteria:

- Running from the repo root no longer produces unrelated demo output.

## Priority 3: fix correctness edge cases

### 6. Lock symlink writes by their resolved target

Problem:

The path lock key is computed before symlink resolution, but `safeWrite` writes
through to the resolved target. A write through a symlink and a write through
the real path can therefore modify the same file under different locks.

Relevant file:

- `internal/tools/file_write_helpers.go`

Plan:

1. Add a helper that computes the lock key using the resolved symlink target
   when possible.
2. Keep behaviour sane for paths that do not exist yet.
3. Ensure all write tools use the helper consistently.
4. Add a concurrency-oriented unit test with a symlink path and target path.

Acceptance criteria:

- Symlink aliases and real paths serialise through the same lock when they
  target the same file.

### 7. Replace the 1 MiB MCP scanner limit with a safer reader

Problem:

The MCP server reads newline-delimited JSON-RPC messages with `bufio.Scanner`
and a 1 MiB max token. Large valid requests can make serving fail at the
transport level instead of returning a structured JSON-RPC error.

Relevant file:

- `internal/mcp/server.go`

Plan:

1. Replace `bufio.Scanner` with `bufio.Reader.ReadBytes('\n')` or an explicit
   bounded line reader.
2. Choose and document a protocol input limit.
3. If a request exceeds the limit, return a structured error when an ID can be
   decoded, or a clear transport error otherwise.
4. Add tests for:
   - request just below the limit;
   - request above the limit;
   - malformed oversized input.

Acceptance criteria:

- Large input handling is deliberate, tested, and documented.

### 8. Report traversal errors in `find_files`

Problem:

`find_files` discards the `walk` error, so timeout, cancellation, and filesystem
errors can be hidden behind partial results.

Relevant files:

- `internal/tools/find_files.go`
- `internal/tools/search_in_files.go`

Plan:

1. Capture the `walk` error.
2. Mirror the partial-result reporting pattern used by `search_in_files`.
3. Add tests for cancellation and permission/error injection if practical.

Acceptance criteria:

- Partial results are labelled as partial.
- Hard traversal failures are returned as errors when no useful result exists.

### 9. Fix long-line handling in `search_in_files`

Problem:

The comment says lines over 1 MiB are skipped without dropping the rest of the
file, but `bufio.Scanner` stops scanning after a token-too-long error.

Relevant file:

- `internal/tools/search_in_files.go`

Plan:

1. Replace `splitLines` with a `bufio.Reader` based implementation that can
   skip one oversized line and continue.
2. Return or track a warning when oversized lines are skipped.
3. Add a test where a match appears after an oversized line.

Acceptance criteria:

- Matches after oversized lines are still found.
- The user can tell when a file contained skipped oversized lines.

### 10. Handle old stats DB schemas in read-only flows

Problem:

`OpenReadOnly` does not run migrations, but read paths query current schema
columns. Before 1.0, plumb treats the active global stats schema as the
supported shape, so read-only opens should fail clearly when the DB is older
than the current `PRAGMA user_version`.

Relevant file:

- `internal/stats/db.go`

Plan:

1. Detect old schema and return a clear upgrade-required diagnostic.
2. Keep read paths current-schema-only; do not add pre-1.0 compatibility
   shims for missing columns.
3. Add tests that current-schema read-only opens succeed and stale versions
   fail clearly.

Acceptance criteria:

- Old stats DBs do not fail with raw SQL errors in CLI/TUI.
- Current-schema global stats DBs expose workspace/session filters correctly.

### 11. Deep-copy config defaults and merge inputs

Problem:

`Config` contains maps and slices. Returning or assigning it by value shares
mutable backing storage for `LSP` defaults and nested fields.

Relevant file:

- `internal/config/config.go`

Plan:

1. Add `cloneConfig` and `cloneLSPConfig` helpers.
2. Use them in `Defaults`, `Load`, and `LoadProject`.
3. Add tests that mutate returned defaults/project config and prove later loads
   are unaffected.

Acceptance criteria:

- Config loads are isolated from each other.
- Tests cover map and slice mutation.

## Priority 4: stale after reconciliation

### 12. Wire post-write diagnostic timing

Status: stale on current `main`.

The current tree already threads `edits.post_write_diagnostics_ms` from config
through daemon setup into `WriteDeps.PostWriteDiagWindow`. Keep this section
only as review history.

Relevant files:

- `internal/config/config.go`
- `internal/tools/file_write_helpers.go`
- `internal/tools/write_file.go`
- `internal/tools/edit_file.go`
- `internal/cli/daemon.go`

No implementation planned.

### 13. Finish or remove `log_format`

Status: stale on current `main`.

The current tree validates `log_format`, displays it in `config show`, and wires
it into daemon and control-socket logging setup. Keep this section only as
review history.

Relevant files:

- `internal/config/config.go`
- `internal/cli/root.go`
- daemon logging setup files

No implementation planned.

## Priority 5: reduce repetition and file size

### 14. Extract shared CLI presentation helpers

Problem:

Path contraction, age formatting, padding, diagnostic rendering, and table
styling are repeated across CLI commands and partly duplicated with the TUI.

Relevant files:

- `internal/cli/stats.go`
- `internal/cli/config.go`
- `internal/cli/sessions.go`
- `internal/cli/diagnostics.go`
- `internal/cli/doctor.go`
- `internal/tui/model.go`

Plan:

1. Create an internal CLI presentation helper package or file.
2. Move CLI-only helpers first:
   - path contraction;
   - age formatting;
   - common table style;
   - diagnostic box rendering.
3. Avoid coupling CLI directly to TUI internals beyond shared style constants
   already used intentionally.
4. Add snapshot-style tests only where output stability matters.

Acceptance criteria:

- CLI commands use one implementation for common display concerns.
- TUI behaviour remains unchanged.

### 15. Split large files by responsibility

Problem:

Several files exceed the project's own approximate 400-line guidance. The main
hotspot is `internal/tui/model.go`, followed by daemon, setup, stats DB, and
edit-file logic.

Initial targets:

- `internal/tui/model.go`
- `internal/cli/daemon.go`
- `internal/cli/setup.go`
- `internal/stats/db.go`
- `internal/tools/edit_file.go`
- `internal/mcp/server.go`

Plan:

1. Start with low-risk extraction:
   - move pure formatting helpers;
   - move setup path helpers;
   - move stats query helpers.
2. Defer behavioural refactors until correctness bugs above are fixed.
3. Keep each split mechanical and separately tested.

Acceptance criteria:

- File responsibilities are easier to scan.
- No behaviour changes in pure extraction commits.

## Suggested implementation order

1. Fix golangci-lint config/version pinning.
2. Remove or relocate `test_table.go`.
3. Fix CLI workspace resolution for `status` and `config show`.
4. Fix `find_files` traversal error reporting.
5. Fix `search_in_files` long-line handling.
6. Fix stats read-only compatibility.
7. Deep-copy config defaults and merge inputs.
8. Fix `find_replace` write safety.
9. Fix symlink lock keys.
10. Fix MCP message-size handling.
11. Defer stale partial-config items unless a fresh bug is found.
12. Defer broad CLI presentation/helper extraction.
13. Defer mechanical file splitting, especially TUI files.

## Validation checklist for each fix

Run the narrow tests for the touched package first, then:

```sh
go test ./...
go test -race ./internal/tools ./internal/cli ./internal/stats ./internal/cache
golangci-lint run
```

For CLI-facing changes, also run the built binary manually:

```sh
go run ./cmd/plumb version
go run ./cmd/plumb status
go run ./cmd/plumb status --workspace .
go run ./cmd/plumb config show --workspace .
```

When daemon behaviour changes, restart the daemon before manual verification:

```sh
go run ./cmd/plumb stop
go run ./cmd/plumb serve
```
