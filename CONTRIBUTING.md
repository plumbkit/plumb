# Contributing to Plumb

Thanks for your interest in improving Plumb. This guide covers everything you need to
make a change land cleanly.

## Before you start

- **Read `AGENTS.md`** (the canonical project brief — `CLAUDE.md` and `GEMINI.md` are
  symlinks to it, and Codex/ChatGPT read it directly). It explains the architecture,
  the layering rules, and the invariants that matter.
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

Other useful targets: `make test`, `make test-race`, `make lint`, `make integration-test`
(needs gopls/pyright on `PATH`), `make tidy`.

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

## Testing MCP client integrations

Plumb registers itself as a stdio MCP server for many client CLIs via
`plumb setup <client>`. The on-demand **`cmd/clientsmoke` harness** verifies those
clients actually work *through* plumb, non-interactively (no TUI blocks). It is gated
behind its own build tags, so it never runs in CI or `make verify` beyond a compile
check — run it yourself when you touch client setup or want to validate a new client.

**Install the client CLIs** (idempotent; installs CLIs only, never API keys):

```sh
make install-clients      # or: ./scripts/install-clients.sh
```

Prerequisites: Node 20+ (npm clients), Python 3.8+ (hermes), `curl` (script-installed
clients). Some installers drop binaries in `~/.local/bin` or `~/.npm-global/bin` — add
those to `PATH` if a binary isn't found.

**Connection tier** — no API keys, deterministic. Confirms each installed client
completes the MCP handshake with plumb, asserted on plumb's own session record rather
than each CLI's output:

```sh
make clients-test
```

Verified-passing: gemini, qwen, opencode, hermes (needs its `[mcp]` extra), claude-code.
cursor-agent, codex, auggie, crush, and goose have no auth-free connecting probe and are
skipped here (still drivable in the auth tier).

**Auth tier** — drives a real headless LLM prompt to force a plumb tool call, then
asserts a `tool_calls` row in the stats DB. It **costs money** and runs only the clients
whose API key is exported; the rest skip automatically:

```sh
OPENAI_API_KEY=…  ANTHROPIC_API_KEY=…  GEMINI_API_KEY=…  make clients-test-auth
```

| Env var | Clients it enables |
|---|---|
| `OPENAI_API_KEY` | codex, qwen, opencode, crush, goose, hermes |
| `ANTHROPIC_API_KEY` | claude-code (also valid for opencode/crush) |
| `GEMINI_API_KEY` / `GOOGLE_API_KEY` | gemini |
| `CURSOR_API_KEY` | cursor-agent |
| `AUGMENT_API_TOKEN` | auggie |

The auth tier is **nondeterministic** (it asserts the model *chose* to call a plumb
tool): use it to confirm a new client integration works, not as a gate. Per-client
auto-approve/auth flags are version-sensitive — see the spec table in
`cmd/clientsmoke/harness_test.go`. `make build-clients` compile-checks both tiers and is
folded into `make verify`. To add a client, add a `clientSpec` to `clientSpecs()`.

## Commits

Conventional commits:

```
<type>(<scope>): <short summary>

[optional body: why, not what]
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`. Prefer **one commit
per discrete change**, each with a `CHANGELOG.md` entry — bisectable history beats
squashed PRs.

## Licensing of contributions

Plumb is MIT-licensed. By contributing, you agree your contributions are licensed under
the same MIT terms. New source files don't require a per-file licence header, but don't
remove the root `LICENSE`.

## Pull requests

- Keep PRs focused and reviewable.
- Ensure `make verify` is green and a `CHANGELOG.md` entry is included.
- Fill out the PR template — it asks the questions that speed up review.

Thank you for contributing.
