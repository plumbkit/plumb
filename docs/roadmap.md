# Roadmap

Plumb is pre-1.0. The core — concurrency-safe, atomic, transactional writes; the
crash-resilient daemon; the tree-sitter topology index; and per-project memory — is in
daily use. The road to 1.0 is about *proving* it beyond the validated core and smoothing
distribution, not rewriting it.

## How we get to 1.0

Rather than jump from 0.9 straight to 1.0, Plumb ships a series of focused minor
releases — **0.10 through 0.19** — each with one coherent theme. **0.19.x is the last
0.x release.** 1.0 follows it as a deliberate *stability commitment*, not a feature
milestone.

Why the runway: a 1.0 makes implicit promises about behaviour stability and language
breadth. We want to *earn* those in the open — validating the experimental language
adapters against real binaries, hardening the daemon, and smoothing install and
distribution — before we make them. Native Windows support is intentionally **not** a
1.0 gate; it lands in 1.1.

| Version | Theme | Focus |
|---|---|---|
| **0.10** | Distribution + honest claims | ~~Homebrew install (`brew install plumbkit/plumb/plumb`)~~ _(✓ shipped — GoReleaser build + `plumbkit/homebrew-plumb` tap)_; ~~semantic re-rank for topology search promoted to GA~~ _(✓ done — supported, opt-in, API/BYO-endpoint only; documented in `docs/configuration.md`)_; ~~this roadmap published~~ _(✓ done — published as `docs/roadmap.md`)_; in-repo claims trued-up (~~TypeScript adapter now validated~~ _(✓ validated — typescript-language-server 5.3.0 passes both real-binary integration tests once the `publishDiagnostics` client capability is advertised)_, ~~Linux CI green~~ _(✓ done — `verify` + real-binary `integration` jobs run on `ubuntu-latest` in CI)_). |
| **0.11** | Validate experimental adapters | Retest **~~zls~~** _(✓ validated 2026-06-17 — passes both real-binary integration tests once the `publishDiagnostics` client capability is advertised; the pull-diagnostics hypothesis was wrong)_ and **kotlin-language-server** _(deferred out of 0.11 — Kotlin remains experimental; its real-binary diagnostics integration test needs a genuine Gradle/Maven project, so promotion waits on that, not on any plumb-side gap)_ against real binaries, then promote them from experimental to validated ([#13](https://github.com/plumbkit/plumb/issues/13)). |
| **0.12** | Swift on Xcode | ~~Build Server Protocol guidance and trusted, default-off auto-configuration (`buildServer.json` via `xcode-build-server`) for bare `.xcodeproj`/`.xcworkspace` roots~~ _(✓ shipped — safe marker/scheme selection, scoped SourceKit-LSP restart, honest lifecycle states, and real-Xcode cold/built/restart coverage; Plumb never builds the project automatically)_. |
| **0.13** | Daemon robustness _(✓ delivered)_ | ~~Crash-safety around plumb's own git writes~~ _(✓ done — per-repo serialisation lock, graceful-shutdown drain, attributable stale-lock reaper)_; ~~lock-free liveness probe ([#15](https://github.com/plumbkit/plumb/issues/15))~~ _(✓ done — `tools/list` snapshots under the lock, then filters and marshals outside it, so `ping`/`daemon_info` stay responsive under write contention)_; ~~per-connection write deadline~~, ~~language-server pool self-heal~~ (a slow-failing first start — and `restartSwift` — is evicted and retried rather than reused behind a structural fallback), and ~~provably-bounded daemon shutdown~~ (concurrent store closes under a budget that sums to 9 s against the 10 s watchdog, plus notify pile-up observability) _(✓ all shipped)_. |
| **0.14** | Agent ergonomics _(✓ delivered)_ | Client-aware tool profiles (shipped); ~~configurable workspace access wired through client setup~~ _(✓ done — `--allow-dir`/`PLUMB_ALLOWED_DIRS` grant extra read-write roots per connection, transported through the resilient-proxy handshake)_; ~~enable a language server live without a restart~~ _(✓ shipped — `plumb enable-lsp <language>` flips the running daemon's effective language set live and its server attaches lazily on the next matching file; the last open 0.14 item)_. Plus a wave of shipped tool-surface work: LSP cold-start warm-up signalling, high-confidence parameter auto-correction, recoverable-error self-heal, `get_definition`-by-name topology fallback, unified-diff output for `find_replace`/`rename_symbol`, the `file_status` dirty/last-writer probe, `undo_edit` (safe single-level write revert), anchor-bounded `edit_file`, optional session `purpose` tags, post-write cross-file diagnostics (flags an edit that breaks another file), session-state persistence across a daemon restart, the `run_task`/`agent_config` tools for stored build/test commands and opt-in agent-writable config, `move_symbol` (relocate a whole declaration between files in a package), the symbol-edit steering that routes whole-declaration changes to the coordinate-free `replace_symbol_body`/`insert_before_symbol`/`insert_after_symbol`/`safe_delete_symbol` family, and configurable `[ui.keys]` TUI shortcuts. |
| **0.15** | Honesty + config surface | ~~A trustworthy tokens-saved metric~~ _(✓ shipped — two-axis capability/efficiency model, "estimated (model vN)" labelling)_; ~~full settings-screen coverage of every config field with inline help~~ _(✓ shipped — `buildSettingItems` emits a row per config field with one-line help, centralised in the `internal/config/fields.go` registry)_. |
| **0.16** | Stabilisation + cross-platform proving | Bug cleanup ([#4](https://github.com/plumbkit/plumb/issues/4), ~~[#2](https://github.com/plumbkit/plumb/issues/2)~~ _(✓ fixed 2026-06-17 — theme picker now writes a sparse `ui.theme` key, no env/default baking)_, ~~[#14](https://github.com/plumbkit/plumb/issues/14)~~ _(✓ fixed — the test now waits on an index-complete signal; green under `-race`)_); ~~the client smoke suite proven on Linux~~ _(✓ — the `clientsmoke` handshake tier now runs green on Linux, not just macOS)_; community Linux testing across distros and desktops ([#9](https://github.com/plumbkit/plumb/issues/9)). |
| **0.17** | Distribution + discoverability | ~~MCP Registry~~ _(✓ publish pipeline wired — `release.yml` stamps `server.json` and publishes via GitHub OIDC)_ and GitHub MCP Registry listings; curated-list submissions; a public-exposure polish pass (~~version/registry lockstep~~ _(✓ done — `TestServerJSONVersionMatchesVERSION` pins `server.json` to `VERSION`)_, ~~doc-count accuracy~~ _(✓ trued up each release and pinned by `TestDocToolCountMatchesRegistry`)_). |
| **0.18** | Proof + docs | Measured tool-comparison use cases; demo assets; a documentation accuracy pass. |
| **0.19** | Soak + feedback — **last 0.x** | Fixes and real-world soak rather than a formal tagged release candidate: Plumb ships rolling `0.x` patches, so this is a feedback window across several patch releases — not an RC gate — before committing to 1.0. |
| **1.0** | General availability | The stability commitment and the validated-core promise — concurrency-safe writes, the resilient daemon, topology, and memory. |

Themes are buckets, not a strict release order — an item ships when it's ready.
Several **0.13–0.15** theme items (the daemon-robustness hardening, the live
`enable-lsp`, `move_symbol`, and the symbol-edit steering) already landed in
**0.12.x** patches ahead of a tagged 0.13, which is why the rows above their
nominal minor already read as delivered.

## Shipped outside any themed row

Two capabilities landed as standalone surfaces rather than under a themed minor:

- **`plumb web`** — a daemon-hosted, loopback-only web UI (Svelte 5 +
  ECharts/uPlot) with full TUI parity (Dashboard, Sessions, Memory, Logs,
  scope-aware Settings). See [`docs/web.md`](web.md) and the `[web]` config
  section.
- **`[collab]` cross-agent sharing** (shipped in 0.10) — three config-gated,
  advisory tiers for agents that share one daemon on a machine: peer awareness
  (daemon-observed writes), intents + a mailbox (`share_intent` / `leave_note`),
  and on-demand knowledge handoff (`share_findings`). Everything beyond tier-1
  awareness is opt-in, all of it is advisory (never blocks a write) and strictly
  per-workspace. See the `[collab]` config section.

Neither fits a 0.1x theme above because neither was planned as part of the road
to 1.0 — they're called out here so a reader scanning this roadmap knows they
exist.

## After 1.0

- **1.1 — Native Windows support.** Port the daemon's Unix-socket transport to named
  pipes or loopback TCP — preserving the resilient reconnecting proxy and per-connection
  isolation — and add a Windows CI matrix ([#8](https://github.com/plumbkit/plumb/issues/8)).
- **Tree-sitter cleanup.** Retire the WASM tree-sitter fallback paths once the
  canonical-grammar extractors (Swift implicitly-unwrapped optionals, TypeScript
  typed-arrow TSX) have soaked in the field.

## A note on pacing

The later minors (roughly 0.16–0.19) are deliberately lighter — stabilisation,
real-world proving, and soak — rather than padded feature dumps. 0.19.x in particular is
a soak window: bug-fix patches while we gather field feedback, so that 1.0's stability
guarantees are something we've already demonstrated by the time we make them.

Issues and ideas are welcome — see [CONTRIBUTING.md](../CONTRIBUTING.md).
