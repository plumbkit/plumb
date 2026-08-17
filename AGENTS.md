# Plumb — Agent Instructions

> Source of truth: edit `AGENTS.md` only.
>
> `CLAUDE.md` and `GEMINI.md` are repository symlinks to this file for client compatibility; Codex and ChatGPT read `AGENTS.md` directly. Do not replace, unlink, rewrite, or edit those symlink paths directly. If an instruction change is needed, update `AGENTS.md`; the linked files will reflect it automatically.
>
> These agent-context files are tracked in git to ensure a consistent, high-quality experience for AI assistants.

This file is the canonical brief for AI agents working in the plumb codebase. Keep it accurate; it ages fast.

> **CRITICAL — tool priority:** Always use plumb MCP tools for all tasks when plumb is present and the required capability is available through plumb. Do not fall back to native tools (Read, Edit, Bash, shell commands, etc.) for file reads, writes, edits, searches, symbol lookups, or git queries when the equivalent plumb tool exists. Plumb tools are LSP-aware, concurrency-safe, and session-tracked; native tools bypass all of that. The only exceptions are tasks plumb explicitly does not cover (interacting with external services, ad-hoc shell). Under the **lean** tool profile the read-only commodity search/list/find tools (`search_in_files`, `find_files`, `file_diff`, …) are hidden from `tools/list` because a lean client's native equivalents suffice — there the always-plumb lane is writes/edits, symbol operations, `git`, and `run_task` (set `[tools] profile = "full"` to also advertise the plumb read tools). Auto-mode serves **full** (not lean) to a client that can only invoke advertised tools — e.g. Claude Code, which builds even its tool-search list from `tools/list`, so a hidden tool would be unreachable rather than merely undisplayed — so for those clients every plumb read tool is advertised and the always-plumb rule applies in full. Build/test/lint now have a plumb path: `run_task` (and the `plumb build/test/lint/e2e/verify` CLI) run the user's stored `[tasks.<lang>]` commands — prefer it over a raw shell `go test`/`npm test` when a task is configured.

> **Per-tool detail lives in the tool's own MCP description.** Each tool registers its full description and input schema (`tools/list`), and `session_start` emits client-specific tool guidance at runtime. This file is orientation, not the authoritative tool reference — when a tool's behaviour matters, read its description.

Current version: see the `VERSION` file and `CHANGELOG.md` (not pinned in this brief, to avoid drift).

## Project purpose

Plumb gives coding agents the intelligence layer of an IDE. It is an MCP (Model Context Protocol) server exposing LSP (Language Server Protocol) capabilities, a tree-sitter topology index, and per-project memory, plus a complete filesystem toolkit (read, write, edit, delete, rename, transaction). It lets an LLM — especially Claude Desktop, Claude Code, Codex, or Gemini CLI, which may have limited filesystem access — navigate, understand, and modify a codebase entirely through structured semantic tools, no raw-file dumping or shell.

The architectural commitments are:

1. **LSP-correct semantics.** Plumb's writes reach the language server via `workspace/didChangeWatchedFiles` (not the open-document lifecycle); capabilities are negotiated; server-initiated `client/registerCapability` requests are answered.
2. **Concurrency-safe writes.** Per-path locks across all write tools; atomic `tmpdir → rename`; symlink-aware; CRLF-tolerant; optimistic-concurrency via mtime; bounded retries.
3. **Per-session isolation.** The daemon hosts multiple MCP connections. Read-tracking, rate limits, and caches are scoped per-connection — never process-global.
4. **Configurable safety.** Strict mode, rate limits, and other safety knobs are configurable at three layers: global, per-project (`<workspace>/.plumb/config.toml`), and environment.

## Architecture

Strict layered architecture — lower layers must never import higher ones. **This is enforced, not merely documented:** `internal/arch` declares the layer of every first-party package and its tests fail on a violation, on a package with no layer assigned, and on a stale entry. Adding a package therefore forces a deliberate answer to "which layer is this?" — which is cheaper than discovering it later from a dependency cycle.

```
Foundation (paths, durable writes, tokenisation, redaction, colour data)
→ Transport (MCP/LSP) → Domain (symbols, edits, capabilities)
                    → Intelligence (topology)
                    → Application (composite tools, caching, rate-limiting)
                    → Presentation (TUI, CLI)
```

Key packages:

| Package | Role |
|---|---|
| `internal/mcp/` | MCP server, tool registry, prompts, stdio transport |
| `internal/lsp/` | `lsp.Client` interface (23 methods), JSON-RPC 2.0 (with server-request support), process supervisor |
| `internal/lsp/adapters/base/` | The half of every adapter that is identical across servers; adapters embed `*base.Adapter` and shadow only what differs. **Its exported surface is exactly `lsp.Client` and must stay that way** — one extra exported method silently opts every server into a capability it lacks. Escape hatches are package-level FUNCTIONS, never methods. Rules + the four guard tests: [`docs/architecture.md`](docs/architecture.md#the-base-adapters-exported-surface) |
| `internal/lsp/adapters/{gopls,pyright,jdtls,rust,swift,zig,typescript,kotlin,html}/` | All languages enabled by default and activated automatically when their server binary is installed (set `[lsp.<lang>] enabled = false` to exclude one). Validated except `html`; see the adapter status table in [`docs/adding-an-lsp.md`](docs/adding-an-lsp.md#validation-levels). |
| `internal/topology/` | SQLite/FTS5 semantic graph; background indexer; Go AST, gotreesitter (most languages incl. TypeScript/TSX/JSX, below), and canonical-tree-sitter-via-WASM Swift (`wasmts`); search + BFS explore/impact/affected/routes |
| `internal/topology/extractors/golang/` | Go extractor (`go/parser`+`go/ast`; no CGo) |
| `internal/topology/extractors/` | Three engines: `golang` (go/ast), `treesitter` (gotreesitter, pure-Go, ~29 languages incl. JS/TS/TSX — primary since the v0.48.0 flip), `wasmts` (wazero; production for **Swift only**). `typescript/` is the retired regex extractor, kept for the parity harness. **Grammars decode lazily and parse arenas are released back to the pool** — which is why idle RSS looks large without leaking. Per-language status + the memory rule: [`docs/architecture.md`](docs/architecture.md#structural-extractors-and-their-memory-discipline) |
| `internal/langsupport/` | Per-language capability registry (structural engine + LSP adapter). Single source of truth for `buildExtractors`; the seam for moving a language onto tree-sitter. |
| `internal/tools/` | MCP tool implementations; `WriteDeps` bundles write-tool deps; `txlog` subpackage is the transaction rollback WAL |
| `internal/quality/` | Offline post-write analysers (golangci-lint, …) on changed files; findings appended to write responses |
| `internal/cache/` | Session-scoped symbol cache + LSP-driven invalidator |
| `internal/config/` | TOML config, XDG paths, project-config merging |
| `internal/session/` | Session-file registration + client identity tracking |
| `internal/stats/` | Global SQLite tool-call statistics, row-scoped by workspace and session (WAL, P95, client-aware). Writes funnel through one batched-transaction `Writer` (single-writer goroutine; non-blocking enqueue, never on the response path); reads use a process-cached `SharedReadOnly` handle. Also holds the `episodic_memories` table; stats schema `user_version` 16 |
| `internal/memory/` | Per-workspace markdown memory store (source of truth), exposed as MCP resources. Plus a rebuildable per-workspace FTS5 index (`memory.db`, separate from `topology.db`) backing ranked `search_memories`; generated-memory provenance + redaction (`internal/redact`); and `paths:`-glob hint matching for response injection |
| `internal/redact/` | Secret scrubber (API keys, tokens, PEM keys, URL credentials, secret assignments) applied before any generated/episodic memory is persisted |
| `internal/tui/` | Bubble Tea v2 TUI — live session + stats dashboard, recent-edits panel |
| `internal/render/` | Shared, pure CLI/TUI presentation helpers (stdlib + rendering libs only) |
| `internal/fsync/` | The one atomic temp-then-rename write (`AtomicWrite`): stages a per-writer temp sibling, fsyncs it before the rename and the parent dir after, preserves the target's permissions on rewrite, and falls back across filesystems (EXDEV). Every durable write in the tree goes through it; the fsync steps are gated by the `[edits] fsync` knob |
| `internal/sqlitex/` | The one place a SQLite DSN is built (`Open`/`OpenReadOnly`): pragmas, busy timeout, WAL and a real `file:`-URI `mode=ro` by construction, with the DB dir created 0700 |
| `internal/textfmt/` | Stdlib-only text primitives (no lipgloss): pluralisation, rune-safe truncation/ellipsis, and KiB/MiB byte-size labels |
| `internal/fsguard/` | Guards filesystem walks against macOS TCC false-positive prompts on protected dirs |
| `internal/monitor/` | Process resource-usage snapshots (CPU %, memory) plus daemon start time, feeding the TUI daemon metrics |
| `internal/cli/` | Cobra subcommands; daemon, proxy, pool, workspace detection, `config show` |
| `internal/arch/` | The layered architecture as data (`arch.Layers`) plus the shared-primitives and pinned-calls rules (`PrimitiveRules`/`CallRules`) — all enforced by tests. No runtime code depends on it |

## Daemon architecture

`plumb serve` is a resilient stdio proxy in front of a shared `plumb daemon` (one process per machine; one language server per (root, language); per-connection MCP sessions). The full runtime layout, singleton/proxy/write-deadline/memory-limit detail, and workspace-detection rules live in [`docs/architecture.md`](docs/architecture.md) (*Daemon architecture*, *Workspace detection*).

## Configuration

Four layers, each overriding the prior: compiled defaults → global `config.toml` → project `<workspace>/.plumb/config.toml` → environment variables; `plumb config show` prints the resolved config with provenance. **The full per-section reference lives in [`docs/configuration.md`](docs/configuration.md)** — `[edits]`, `[workspace]`, `[git]`, `[ui]`/`[ui.keys]`, `[web]`, `[lsp_query]`, `[topology]`, `[session]`, `[memory]`, `[collab]`, `[rastro]`, `[semantics]`, `[tasks.<lang>]`, `agent_config_writes`, `[tools]`, `[lsp.<lang>]`, `[xcode]`, `[[command]]`/`[commands]`. Two facts worth keeping in head: the `[tools]` profile governs which tools are advertised in `tools/list` — the non-lean remainder (38 tools today, out of a 59-tool registry; the lean set keeps 21) is hidden only for a client with verified deferred tool discovery, and a hidden tool stays callable by name — and adapter validation status (validated vs experimental per language server) lives in [`docs/adding-an-lsp.md`](docs/adding-an-lsp.md#validation-levels).

## Client setup commands

`plumb setup <client>` registers the current `plumb` binary as a stdio MCP server; the per-client table, the `--project`/`--lean`/`--repair`/`--all` flag contracts, and the `plumb doctor` grading live in [`docs/cli-reference.md`](docs/cli-reference.md#plumb-setup). Setup is config-only; skills come from `plumb skills sync`:

`plumb skills` owns the skill surface, now that setup is config-only: `plumb skills sync` installs eight idempotent user-scoped skills for the clients that read `SKILL.md` — `plumb-explore` (the discovery ladder, `workspace_search` down to a bounded `read_file`), `plumb-refactor` (semantic rename, atomic cross-file edits), `plumb-testing` (`topology_affected` for which tests to run, `run_task` to run them), `plumb-minimal-change` (prove reuse and minimality with plumb evidence before writing non-trivial code), `plumb-memory` (search the per-workspace memories before re-deriving, write one when the knowledge outlives the task), `plumb-diagnose` (read the refusal, get compile truth, tell "broken" from "not ready"), `plumb-git` (the tiered git policy, and the narrower plumb tool to prefer over a destructive git command), and `plumb-chat` (the agent-to-agent mailbox: addressing, the blocking wait, why silence is not refusal, and what the exchange cap does and does not bound). Bare `plumb skills` is a read-only per-client, per-skill status table (installed / missing / stale, with a `not registered` marker for skill-capable clients whose config lacks plumb); `plumb skills sync [client]` is the only writer, installing or refreshing them into every registered skill-capable client, or just the named one. Client-directory verification, generic-client routing, and the skill-count ceiling are covered in [`docs/cli-reference.md`](docs/cli-reference.md#plumb-skills).

## Contributor recipes

Step-by-step procedures live as project skills in `.claude/skills/` (plain markdown, readable from any client): `add-lsp-adapter` (adapter checklist + promotion rule; full guide in `docs/adding-an-lsp.md`) and `add-mcp-tool` (tool checklist incl. the thin-orchestrator `Execute()` pattern and the lean-profile decision).

## Available tools (59)

Concise index only. Full behaviour, schemas, and per-tool steering live in each tool's MCP description (`tools/list`); sources are `internal/tools/<name>.go`.

- **Bootstrap:** `session_start` is the first call; it returns workspace, language, branch, recent context, tool stats, diagnostics, git policy, memories, and client-specific guidance.
- **LSP queries:** `workspace_symbols` (workspace-wide by name, or one document when given a `uri`), `get_definition`, `explain_symbol`, `file_outline`, `find_references`, `call_hierarchy`, `type_hierarchy`, `diagnostics`.
- **LSP edits:** `rename_symbol`, `replace_symbol_body`, `insert_before_symbol`, `insert_after_symbol`, `safe_delete_symbol`, `move_symbol` (relocate a whole declaration between two files in the same directory/package, atomically — refuses cross-package moves it cannot rewrite references for); these are semantic operations, distinct from file moves/copies, and support `include_doc_comment` where relevant.
- **Filesystem reads:** `read_file`, `read_symbol`, `read_multiple_files`, `find_files`, `search_in_files`, `file_status`. `find_files` is also the directory lister (`list_files` and `list_directory` were folded into it): `pattern` is optional, `max_depth: 1` lists one level like `ls`, and `include_details` adds a `[FILE]`/`[DIR]`/`[LINK]` marker with size and modified time. Reads are bounded, binary-safe, `.gitignore`-aware where applicable, and return mtime/sha headers for optimistic edits. `file_status` is a content-free probe reporting per-path `git_dirty` / `changed_since_plumb_wrote` / `last_writer` / mtime / size.
- **Filesystem writes:** `write_file`, `edit_file`, `delete_file`, `rename_file`, `copy_file`, `transaction_apply`, `undo_edit`. Writes take `WriteDeps`, hold per-path locks, respect dirty-file checks, notify LSP, invalidate caches, and consume the write-rate budget. `undo_edit` safely reverts plumb's most recent write to a file (its own change only, refusing if the file changed since), the safe alternative to a whole-file `git checkout`.
- **Search/replace and git:** `find_replace` is dry-run by default; prefer `rename_symbol` for identifiers. `git` is tiered by policy (read/write/destructive/network), with typed `add`/`commit` and confirmation for dangerous tiers.
- **Other utilities:** `git_init`, `file_diff`, `daemon_info` (session, daemon version, Go runtime, OS/arch, config state; `version` was folded into it), `rename_session`, `workspace_sessions`.
- **Cross-agent sharing (`[collab]`):** `share_intent` broadcasts what you are working on (optionally scoped to `path_globs`); `leave_note` + `check_messages` are a threaded agent-to-agent mailbox; `share_findings` hands off what you have learned as a durable generated memory on demand (riding the episodic pipeline: redact → provenance → `.plumb/memories/` → FTS index → `generated_memory_keep` retention), instantly discoverable by peers via `search_memories`/`workspace_search`/`relevant_memories`/hints/`session_start`. All are advisory (never block a write) and secret-scrubbed; `share_intent`/`leave_note` render to peers as unverified claims, `share_findings` writes lower-confidence agent-generated content. **Mailbox:** delivery is polling only and exactly once — addressing, the blocking wait, threading, the cap and the etiquette are the `plumb-chat` skill. Gated on `[collab] intents` (default off) / `mailbox` (default **on**, same-workspace only) / `cross_project` (default off, the *recipient's* opt-in for messages from another workspace) / `knowledge_handoff` (default off); intents/messages expire per `intent_ttl_minutes` in `<workspace>/.plumb/collab.db` (gitignored), with cross-project messages in a daemon-level store. See the `[collab]` section in `docs/configuration.md`.
- **Tasks, commands & config:** `mutation_test` applies an explicit mutant, compile-gates it, runs a scoped test set and restores — `killed`/`survived`/`invalid` (never a kill); it refuses to start unless the workspace is green unmutated, since a kill means green-before-red-after. `run_task` runs a stored `[tasks.<lang>]` command (build/lint/test/e2e/verify; verify = build then test) — no shell, bounded, with a per-workspace trust gate (`plumb trust`) for project-supplied commands. `run_command` runs a named entry from the `[[command]]` allow-list (fixed argv + one `{target}`) under an OS sandbox — the safe, injection-proof way to run workspace commands; a project entry needs `plumb trust`. `execute_shell_command` runs an ad-hoc `sh -c` command (pipes/redirects work) and is **disabled by default** — enable it with `[commands] allow_shell` (global, or project + `plumb trust`); it too runs under the sandbox, which is integrity-only (it confines writes, not reads/env) and **denies the network by default** (`[commands] deny_network`, so the reply reports `network=off`). `agent_config` reads (`describe`) and, only when the user enabled `[agent_config_writes]`, writes (`set`) a small allowlist of config keys — validated atomically, `provenance=agent`, revertible via `plumb config unset`. Guardrails (git tiers, roots, strict mode, API keys, the enable knob) are never agent-writable. See the `[tasks.<lang>]`, `[[command]]`/`[commands]` and `agent_config_writes` sections in `docs/configuration.md`.
- **Topology:** `topology_status`, `topology_search`, `topology_explore`, `topology_impact`, `topology_affected`, `topology_routes`, `structural_query` use the SQLite/FTS5 index at `<workspace>/.plumb/topology.db`.
- **Advisory review:** `minimal_diff_review` reviews a git diff for signs of over-building (single-use abstraction, thin forwarding wrapper, new dependency with a stdlib equivalent, possible duplicate helper, logic change with no test change). Deterministic, no LLM; findings **never block a write**. Evidence is asymmetric — silence is not proof a change is minimal — and each finding is confidence-labelled (`high` = proven from the diff text; `low` = leans on the approximate, intra-file topology call graph, unlike `find_references`' exact cross-file lookup).
- **Ranked discovery:** `workspace_search` is the broker over the indexed corpora — code and docs via topology FTS, memories via the memory FTS index — interleaved by per-corpus rank and labelled with `corpus`/`source`/`field`/`score`/`why` plus per-corpus index freshness; `exact_match=false` always (it is discovery, never proof of absence — the exact lane stays `search_in_files`). Discovery ladder: `workspace_search` → topology/LSP → `search_in_files` → bounded `read_file`.
- **Memory:** `list_memories`, `read_memory`, `write_memory`, `delete_memory`, `search_memories`, `relevant_memories` operate on per-workspace markdown memories under `<workspace>/.plumb/memories/`. `search_memories` is FTS5-ranked when the index is fresh (grep fallback otherwise; `mode` = auto/fts/grep); `read_memory` shows a provenance footer for generated memories; writes/deletes keep the index current. See `[memory]` in `docs/configuration.md`.

## Code style rules

- **Australian English** in all prose: docs, comments, log messages, error strings. Use -ise/-isation, behaviour, colour, honour, favour. **Exception:** identifiers from external specs keep their canonical spelling — LSP method names (`initialize`, `publishDiagnostics`), MCP protocol fields, Go standard library names.
- **`gofumpt`** on save. `golangci-lint` v2.12.2 before every commit; CI enforces.
- **`log/slog`** exclusively. Never `log` package or `fmt.Println` for logging.
- **Errors wrap context:** `fmt.Errorf("loading config: %w", err)`.
- **Context everywhere:** every blocking/I/O operation takes `context.Context` first.
- **Concurrency contract** stated in doc comments on every type.
- **No `init()` doing real work.** Wire dependencies in constructors.
- **No globals** except package-level style vars in `internal/tui/styles.go` and `pathLocks`/`mutationRunLock` in `internal/tools/` (process-global by design).
- **Max ~600 lines per non-test file, ~900 per `_test.go`.** Split if it grows — `make check-size` (in `verify` and the pre-commit hook) enforces both. Tests get the looser cap because table data and fixtures do not decompose the way production code does, but the cap is not absent: an oversized test file is a merge-conflict magnet when several agents edit one package at once. Exception allowlist: `internal/lsp/protocol/types.go`. Anything else needs a line in `scripts/.filesize-baseline` with justification (goal: empty).
- **Comments only when the WHY is non-obvious.** No what-comments.
- **Gocyclo-15 contract.** No first-party non-test function may exceed cyclomatic complexity 15. CI enforces.
- **This brief is budgeted.** Keep `AGENTS.md` under 200 lines — rules and pointers only, detail in `docs/`; `make check-brief` (in `verify` and the pre-commit hook) enforces it. TUI (Bubble Tea v2) conventions live in [`docs/contributing.md`](docs/contributing.md#tui-conventions-bubble-tea-v2).

## Testing requirements

- Tests live next to the code (`_test.go` in the same package); table-driven where the shape fits.
- `internal/lsp/`, `internal/cache/`, `internal/tools/` require meaningful coverage. For write tools, `WriteDeps{}` is the zero-value setup. Per-session isolation tests belong in the package they test.
- The MCP parameter-alias engine (`internal/mcp/argguard.go`) resolves alias names only at the dispatch boundary, so an internal tool→tool `Execute` call (e.g. `read_multiple_files` composing args for `read_file`) must use the target's canonical parameter names — guarded by `internal/tools/inprocess_call_guard_test.go`.
- Do not chase TUI coverage.
- Integration tests requiring external binaries (gopls, pyright) must be gated with `//go:build integration`.
- **Coverage is measured, not assumed.** `make cover` enforces a whole-tree statement floor — currently **71.5%**, canonical in `scripts/check-coverage.sh` — instrumenting every package with `-coverpkg=./...`, so an untested package counts against the total instead of escaping it. CI runs it on ubuntu only, because `internal/fsguard` is Darwin-only and its statements are unreachable elsewhere, so the total is not comparable across OSes. Use `make cover-report` to see where the gaps are before adding tests.
- **Linter selection is deliberate, not maximal.** `.golangci.yml` carries a "considered and REJECTED" list naming the linters whose hits were reviewed and found to be noise at this codebase's conventions (`nilerr`, `misspell`, `exhaustive`, `noctx`, …). Do not enable one of those without re-reviewing the hits, and record the reasoning if you do. Path-scoped exclusions each state why the excluded code is correct as written. **Every `//nolint` directive carries an inline justification** (gosec and errcheck are at 100%) — write the reason when adding one, and prefer a justified path-scoped exclusion over a bare directive when a whole file or pattern is legitimately exempt.
- **Lint the other platform before you push.** golangci-lint only analyses files matching the current `GOOS`, so a Linux `make lint` never sees `sandbox_darwin*.go` / `process_darwin*.go`, and a macOS run never sees the `_linux` files. Run **`make lint-cross`** after changing `.golangci.yml` or touching platform-constrained code — it lints and vets the other OS's tree statically, without running that platform's tests. It has already caught a real CI failure; see [`docs/contributing.md`](docs/contributing.md#build--verify). Windows is out of scope (`internal/session`'s flock has no Windows implementation), so `lint-cross` never targets it.

## Versioning

Version is injected at build time: `-X .../internal/cli.Version=<version>` (defaults to `"dev"`); the Makefile resolves it from the exact git tag → `VERSION` file → short commit hash. To bump during development, edit `VERSION`; do not tag every iteration. The source commit is stamped alongside it and surfaced by `plumb version --json` and `daemon_info` — see [`docs/cli-reference.md`](docs/cli-reference.md#plumb-version).

The daemon writes its build version to `~/Library/Caches/plumb/plumb.version`; `plumb serve` warns on mismatch. **If you've just rebuilt, restart the daemon** — new code never activates against the old process. `plumb restart` brings a fresh daemon straight back up (the resilient proxy reconnects clients); `--force` skips the confirmation prompt.

## Commit conventions

```
<type>(<scope>): <short summary>

[optional body: why, not what]
```

Types: `feat`, `fix`, `refactor`, `test`, `docs`, `ci`, `chore`. Prefer one commit per discrete change with a `CHANGELOG.md` entry — bisectable history > squashed PRs.

## Build commands

```sh
make verify   # build + test + lint + tag-compile + check-size + check-brief + tidy-check — "ready to commit"
make build    # compile to ./plumb, version stamped from git/VERSION
make test     # go test ./...          (test-race for the race detector)
make lint     # golangci-lint run      (lint-cross static-checks the OTHER OS's tree)
make cover    # statement coverage against the floor in scripts/check-coverage.sh
make vuln     # govulncheck over the module graph
```

`make help` lists every target; the rest are in [`docs/contributing.md`](docs/contributing.md#build--verify).

**`make test` redirects the test temp dir inside the checkout.** It runs with
`GOTMPDIR=$(CURDIR)/.testcache`, and Go's `testing` package creates `t.TempDir()`
under `GOTMPDIR` — so under `make test` every `t.TempDir()` sits inside the
repository, whereas a bare `go test` puts it in the system temp dir. A test that
assumes its temp dir is outside the repo (repo ancestry, path contents, outside
the workspace) is green locally and red on CI. Pre-push, run the CI-style
invocation too: `GOTMPDIR=$PWD/.testcache go test ./...`.

**`cover` and `vuln` are deliberately NOT in `verify`** — coverage re-runs the whole suite instrumented (roughly doubling the local edit loop) and govulncheck needs the network. CI runs both on every push as their own jobs, alongside `verify` (2-OS matrix), `test-race`, and the real-binary `integration` job. The coverage floor is a ratchet: raise it when the tree sits comfortably above, never lower it to make a red build green.

**`make install-hooks` is required after every fresh clone** — the pre-commit hook runs `golangci-lint run --fix ./...`, resolving that binary on `PATH` and then in the Go tool bin dir, because hooks inherit the environment of whatever invoked git (an editor, a GUI client, an agent daemon) which often lacks `~/go/bin`. It fails loudly if the binary resolves nowhere rather than skipping the lint silently. **Formatting note:** format via `golangci-lint run --fix ./...`, never the standalone `gofumpt -w` binary — the two can pin different versions and produce phantom lint failures.

## Known limitations and pending work

Take particular care before adding a feature that touches concurrency, the rate limiter, the read tracker, or the stats schema — these areas carry subtle invariants. Land each discrete change with its own `CHANGELOG.md` entry.

## Quick reference for agents

- **First call:** `session_start({})` for orientation, live git policy, diagnostics, memories, and client-specific guidance.
- **Stay in the plumb lane:** after a plumb read, edit with plumb `edit_file`/`write_file`, not a native client edit tool; read-state is tracked separately.
- **Read before edit:** use `read_file`, then pass its `mtime` or `sha256` header to `edit_file.expected_mtime`/`expected_sha`. Required in strict mode, recommended always.
- **Common file ops:** `write_file({file_path, content})`, `edit_file({file_path, edits: [{old_string, new_string}], expected_mtime})`, `transaction_apply({operations: [...]})`, `rename_file`/`copy_file` (`{from, to}`), `delete_file` (`allow_dir: true` only for empty directories).
- **Common rejections:** "has not been read" means strict mode or native/plumb lane mixing; "uncommitted changes" means the file was dirty before this session, so commit it or pass `dirty_ok: true`; throttling means wait or adjust `PLUMB_WRITE_RATE_LIMIT`.
- **Useful checks:** `git({subcommand: "status"})`, `git({subcommand: "log", args: ["-10", "--oneline"]})`, `plumb log-level warn/reset`, and `plumb config show --workspace .`.
