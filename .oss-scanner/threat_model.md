# Threat model: brief for the OSS Scanner

This is the short version for an automated audit. The full model, with every
trust boundary, abuse case and documented residual, is
[`docs/threat-model.md`](../docs/threat-model.md). Read it before you start, and
rate every finding by its [Severity](../docs/threat-model.md#severity) section.

## What this project does and where untrusted input enters

plumb is a local MCP server, a single Go binary, that gives coding agents file,
search, git, language-server and configured-command tools over a user's
workspace. `plumb serve` is a stdio proxy in front of one per-user daemon
(`plumb daemon`, reached over a unix socket) that is shared by every workspace
the user opens. That sharing is why a boundary failure in one workspace can
reach another.

Untrusted input, in the order plumb weighs it:

1. **A hostile repository the user clones and opens.** Everything in it is
   attacker-controlled: file contents and names, symlinks and hardlinks (git
   stores symlinks natively), `.plumb/` (`config.toml`, `tx-log/`, memories,
   indexes), `.git` contents and git output, `.gitignore`, and whatever a
   language server reports about the tree. Attaching a workspace, with no agent
   step at all, must not execute anything, write outside the workspace, or
   delete anything.
2. **A persuaded agent.** MCP tool arguments are model-controlled and arrive as
   JSON-RPC over stdio. They must never exceed the user's configuration: the
   path policy's roots, the git tiers plus `confirm: true`, `plumb trust` for
   project-supplied commands, and no shell anywhere.
3. **A web page in the user's browser** against the opt-in `plumb web` UI on
   `127.0.0.1` (per-start token, `Origin`/`Host` guard; see `docs/web.md`).
4. **Another agent of the same user.** Only the identity and mail-binding
   properties of A6 apply here, not privilege separation.

## Components that matter most / least

Most:

- Path policy and canonicalisation: `internal/tools/pathpolicy.go`,
  `internal/paths`, `internal/fsguard`, and the walk guard behind
  `search_in_files`, `find_files`, `find_replace` and the topology indexer.
- Attach-time work on repository content: `internal/tools/txlog` (orphan
  journal replay), workspace detection and pinning (`internal/workspace`,
  `internal/cli`).
- The project-config trust boundary: `internal/config` (`project_classification.go`,
  `trust.go`), which decides what a cloned `.plumb/config.toml` may change.
- Execution: `internal/tools/run_command.go`, `tasks.go`, the sandbox
  (`sandbox_linux.go` uses `bwrap`, installed in this image), the git policy
  (`internal/tools/git*.go`), and language-server spawning (`internal/lsp`).
- The proxy and protocol edge: `internal/cli` (serve proxy framing, pin replay,
  resume-credential strip), `internal/mcp` (argument guard, logical-agent
  identity), `internal/sessionstate`, `internal/collab`.
- The web UI: `internal/web` (`auth.go`, `routes.go`, the settings write path).

Least: `internal/tui` (local presentation), `docs/`, `site/`, the smoke drivers
under `cmd/smoke` and `cmd/clientsmoke`, and `testdata/` fixtures.

## How to exercise it

- The source is in `/src`. The binary is `/usr/local/bin/plumb`, and gopls,
  pyright, typescript-language-server and the vscode-langservers are on `PATH`.
  The module and build caches are warm, so `go test` works offline.
- Tests: `GOTMPDIR=/src/.testcache go test ./internal/tools/...`, and so on per
  package. The whole suite (`make test`) is slow on two CPUs, so scope it.
  `make integration-test` runs the real-language-server tier.
- Fuzz targets, discovered with `grep -rn '^func Fuzz' --include='*_test.go' .`,
  cover path-policy and canonicalisation against the kernel, journal replay,
  git argument classification, MCP argument correction and the proxy's framing.
  For example: `go test ./internal/tools -run '^$' -fuzz '^FuzzPathPolicyCheckAgainstKernel$' -fuzztime 120s`.
- End to end: `docs/demos/two-agents-one-file.sh` drives two `plumb serve`
  sessions over JSON-RPC under an isolated `HOME`. Reuse that isolation for a
  reproducer: build the hostile repository under `/tmp`, start `plumb serve`
  with `HOME` and the `XDG_*` directories pointed at a scratch directory, then
  `initialize` and `session_start` with that repository as the workspace.
- This image runs as root. A few tests that rely on permission denial skip or
  fail under root; that is not a finding.

## How you rate severity

Use [`docs/threat-model.md#severity`](../docs/threat-model.md#severity). In
short: anything triggered by opening a repository alone is the top of the
scale, a tool call that crosses a boundary the user did not open is next, and a
failed claim between the same user's agents and stores is below that.

## Anything to leave alone

- The documented residuals: "What plumb does not defend against", "Known gaps",
  and every *Residual* paragraph in `docs/threat-model.md`. Report one only if
  you can show the mitigation it relies on fails, or that it is wider than
  written.
- Anything that needs a process already running as the user, including reading
  the user's own 0600 files.
- An agent doing damage inside what the user configured.
- Windows, remote MCP and tunnelling, which are not supported.

Reports are most useful with the reproducer as a Go test beside the code it
breaks, or as a shell script against `plumb serve`. Patches should follow
`docs/contributing.md`: Go formatted through `golangci-lint run --fix`, a
regression test that fails without the fix, and Australian English in comments.
