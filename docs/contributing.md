# Contributing

Thanks for working on plumb. This page is the short version; the canonical
architecture and code-style brief is **[`AGENTS.md`](../AGENTS.md)** at the repo
root — read it before making non-trivial changes.

## Set up

```sh
git clone https://github.com/plumbkit/plumb
cd plumb
make install-hooks    # REQUIRED after every fresh clone
make build
```

`make install-hooks` installs a pre-commit hook that runs
`golangci-lint run --fix ./...`, so formatting and lint issues are caught before
they reach the tree.

## Build & verify

| Command | Does |
|---|---|
| `make build` | Compile to `./plumb`, version stamped from git/`VERSION`. |
| `make test` | `go test ./...` |
| `make test-race` | `go test -race ./...` |
| `make lint` | `golangci-lint run` |
| `make verify` | build + test + lint — the **definition of "ready to commit"**. |
| `make tidy` | `go mod tidy` |
| `make clean` | Remove build artefacts. |
| `make install-clients` | Install every supported client CLI, for local client-integration testing. |
| `make clients-test` / `make clients-test-auth` | On-demand connection/auth tiers that drive each installed client CLI headless (own build tags, never part of `make verify`) — see the comments above these targets in the `Makefile`. |

> **Formatting:** always format via `golangci-lint run --fix ./...` (what the
> hook runs), not a standalone `gofumpt` binary — the two can pin different
> versions and disagree.

**Why the CHANGELOG placement guard is not in `verify`.** A rebase replays a
`CHANGELOG.md` addition under whatever heading happens to sit at that offset, and
it never conflicts, so the entry lands silently in an already-released section —
this has needed a by-hand fix four times. `scripts/check-changelog-placement.sh`
catches it by diffing against the merge-base, which means it needs a base ref, and
a working tree has not got one. `make verify` and the pre-commit hook have to keep
working offline and in a shallow clone, so the guard runs instead as a step in
CI's `verify` job, with the base SHA taken from the pull-request event. Run it by
hand with `make check-changelog-placement` (defaults to `origin/main`); its
regression suite is `make check-changelog-placement-test`. The whole-file half of
the pair — no version number in two headings — is `make check-changelog`, and that
one *is* in `verify` and the hook.

**Why `make lint-cross` exists.** golangci-lint only analyses files matching the
current `GOOS`, so a Linux `make lint` never sees `sandbox_darwin*.go` /
`process_darwin*.go` and a macOS run never sees the `_linux` files. This is not
hypothetical: the widened linter set was trialled on Linux only and passed clean,
then failed CI's macOS leg on the one `usetesting` hit hiding behind a build tag.
CI's two-OS matrix is the backstop, not the first line.

## Code style (essentials)

The full rules are in [`AGENTS.md`](../AGENTS.md); the ones people trip on:

- **Australian English** in all prose, comments, logs, and error strings
  (initialise, behaviour, colour…). Exception: identifiers from external specs
  (LSP method names, etc.) keep their canonical spelling.
- `log/slog` only — never `log` or `fmt.Println` for logging.
- Wrap errors with context: `fmt.Errorf("loading config: %w", err)`.
- `context.Context` first parameter on every blocking/I/O function.
- Comments explain *why*, not *what*. No what-comments.
- Max ~600 lines per file; cyclomatic complexity ≤ 15 (CI enforces both).
- Every `Tool.Execute()` is a thin orchestrator over parse → run → format steps.

## Commit conventions

```
<type>(<scope>): <short summary>

[optional body: why, not what]
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`. Prefer one
commit per discrete change, each with a `CHANGELOG.md` entry — bisectable
history over squashed PRs.

## Adding things

- **A new MCP tool** — follow the `add-mcp-tool` project skill in
  `.claude/skills/` (the checklist, including the thin-orchestrator `Execute()`
  pattern and the lean-profile decision). Implement the `Tool`
  interface, take `WriteDeps` for write tools, register it in
  `registerAllTools` (`internal/cli/conn_register.go`), add tests, and document it in
  [`docs/tools.md`](tools.md).
- **A new LSP adapter** — see [`docs/adding-an-lsp.md`](adding-an-lsp.md).
- **A new config field** — add it to `config.Config`, update `defaults`,
  validate in `validate()`, and document it in
  [`docs/configuration.md`](configuration.md).

## Testing

- Tests live next to the code (`_test.go`, same package); table-driven where it
  fits.
- `internal/lsp`, `internal/cache`, and `internal/tools` need meaningful
  coverage. Use `WriteDeps{}` as the zero-value setup for write tools.
- Integration tests requiring external binaries (gopls, pyright) are gated with
  `//go:build integration`.
- Don't chase TUI coverage.

## TUI conventions (Bubble Tea v2)

- Import paths are **v2 only**: `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2`. Never mix in the v1 packages — type/API incompatibilities.
- `Model` is exported; `NewModel(logPath)` constructs, `Run(logPath)` is the entry point. `View()` returns `tea.View` (`v.AltScreen = true`). Keys: `tea.KeyPressMsg`, match via `msg.String()`.
- Sections (opened with `/`): `Dashboard`, `Sessions`, `Memory`, `Logs`, `Settings` (0–4). Settings (`internal/tui/model_settings.go`) is a two-pane editor: a left **Scope** column (Global + each workspace) and a right rows pane with **General**/**LSP** tabs; `tab`/`shift+tab` cycle focus, `[`/`]` resize.
- Settings persistence is scope-aware: Global rows write global config (`config.Save`), workspace rows write a sparse override to `.plumb/config.toml`. Each row carries a reload-tier numeral (`¹` live / `²` next session / `³` restart) and a `⁴` override / `⁵` inherited mark; only **Theme** and **Log level** apply live. `ctrl+t` opens the theme picker.
- List and `[lsp.<lang>]` rows open shared pop-up editors (`model_settings_listeditor.go`, `model_settings_texteditor.go`), auto-saving on close; overlays dim via `dimLines()` + `spliceOverlay()`.
- **Rebindable keys:** the twelve navigation/action keys are configurable via the global-only `[ui.keys]` table (unknown actions and key conflicts are warned at startup with deterministic resolution; overlay/popup keys and the vim aliases stay fixed). The help overlay and Sessions footer render the live bindings. See the [`[ui.keys]` config section](configuration.md#ukeys--keyboard-shortcuts).
- **Theme system:** `ActiveTheme`/`ActiveThemeName` are globals in `internal/tui/theme.go`; lipgloss style vars are rebuilt by `RebuildStyles()` after any mutation. Adding a `Theme` field means updating every theme literal — `TestTheme_AllFieldsSet` catches omissions.
