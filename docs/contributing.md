# Contributing

Thanks for working on plumb. This page is the short version; the canonical
architecture and code-style brief is **[`AGENTS.md`](../AGENTS.md)** at the repo
root — read it before making non-trivial changes.

## Set up

```sh
git clone https://github.com/golimpio/plumb
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

> **Formatting:** always format via `golangci-lint run --fix ./...` (what the
> hook runs), not a standalone `gofumpt` binary — the two can pin different
> versions and disagree.

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

- **A new MCP tool** — follow the checklist in
  [`AGENTS.md` → "How to add an MCP tool"](../AGENTS.md). Implement the `Tool`
  interface, take `WriteDeps` for write tools, register it in
  `internal/cli/conn.go`, add tests, and document it in
  [`docs/tools.md`](tools.md).
- **A new LSP adapter** — see [`docs/adding-an-lsp.md`](adding-an-lsp.md).
- **A new config field** — add it to `config.Config`, update `defaults`,
  validate in `validate()`, and document it in
  [`docs/configuration.md`](configuration.md) and `AGENTS.md`.

## Testing

- Tests live next to the code (`_test.go`, same package); table-driven where it
  fits.
- `internal/lsp`, `internal/cache`, and `internal/tools` need meaningful
  coverage. Use `WriteDeps{}` as the zero-value setup for write tools.
- Integration tests requiring external binaries (gopls, pyright) are gated with
  `//go:build integration`.
- Don't chase TUI coverage.

## Internal working docs

Active TODOs, design notes, and review queues live under
[`docs/internal/`](internal/) and are not part of the published documentation
set.
