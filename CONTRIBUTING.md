# Contributing to Plumb

Thanks for your interest in improving Plumb. This guide covers everything you need to
make a change land cleanly.

## Before you start

- **Read `AGENTS.md`** (the canonical project brief — `CLAUDE.md`, `GEMINI.md`, and
  `CHATGPT.md` are symlinks to it). It explains the architecture, the layering rules,
  and the invariants that matter.
- **Discuss large changes first.** For anything beyond a bug fix or small improvement,
  open an issue so we can agree the approach before you invest the effort.

## Development setup

```sh
git clone https://github.com/plumbkit/plumb
cd plumb
make install-hooks   # REQUIRED — installs the pre-commit hook (golangci-lint --fix)
make build           # compile to ./plumb, version stamped from git/VERSION
```

`make install-hooks` is mandatory after every fresh clone. The hook runs
`golangci-lint run --fix ./...`; skipping it means CI will reject formatting the hook
would have fixed.

## The definition of "ready to commit"

```sh
make verify          # build + test + lint — run this before every push
```

Other useful targets: `make test`, `make test-race`, `make lint`, `make tidy`.

**Formatting note:** apply formatting via `golangci-lint run --fix ./...`, never the
standalone `gofumpt -w` binary — the two can pin different versions and produce phantom
lint failures.

## Code style (non-negotiable, CI-enforced)

- **Australian English** in all prose: docs, comments, log messages, error strings
  (-ise/-isation, behaviour, colour, honour). *Exception:* identifiers from external
  specs keep their canonical spelling (LSP `initialize`, `publishDiagnostics`; MCP
  fields; Go stdlib names).
- **`log/slog` only** for logging — never the `log` package or `fmt.Println`.
- **Errors wrap context:** `fmt.Errorf("loading config: %w", err)`.
- **Context first:** every blocking/I/O function takes `context.Context` as its first
  parameter.
- **No `init()` doing real work** — wire dependencies in constructors.
- **No new globals** (the allowlist in `AGENTS.md` is exhaustive).
- **Max ~600 lines per file** — split if it grows (see the allowlist in `AGENTS.md`).
- **Gocyclo ≤ 15** for every first-party non-test function. CI enforces.
- **Thin `Execute()`** — every tool's `Execute()` is a thin orchestrator over named,
  individually-testable steps (parse/validate → domain logic → presentation). See the
  pattern in `AGENTS.md`.

## Testing

- Tests live next to the code (`_test.go`, same package); table-driven where it fits.
- `internal/lsp/`, `internal/cache/`, and `internal/tools/` require meaningful coverage.
  For write tools, `WriteDeps{}` is the zero-value setup.
- Integration tests requiring external binaries (gopls, pyright, …) **must** be gated
  with `//go:build integration`.
- Don't chase TUI coverage.

## Commits

Conventional commits:

```
<type>(<scope>): <short summary>

[optional body: why, not what]
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`. Prefer **one commit
per discrete change**, each with a `CHANGELOG.md` entry — bisectable history beats
squashed PRs. When you complete an item from `docs/internal/todo.md`, delete its section
in the same commit that adds the changelog entry.

## Licensing of contributions

Plumb is MIT-licensed. By contributing, you agree your contributions are licensed under
the same MIT terms. New source files don't require a per-file licence header, but don't
remove the root `LICENSE`.

## Pull requests

- Keep PRs focused and reviewable.
- Ensure `make verify` is green and a `CHANGELOG.md` entry is included.
- Fill out the PR template — it asks the questions that speed up review.

Thank you for contributing.
