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
| **0.10** | Distribution + honest claims | Homebrew install (`brew install plumbkit/plumb/plumb`); semantic re-rank for topology search promoted to GA; this roadmap published; in-repo claims trued-up (TypeScript adapter now validated, Linux CI green). |
| **0.11** | Validate experimental adapters | Retest **~~zls~~** _(✓ validated 2026-06-17 — passes both real-binary integration tests once the `publishDiagnostics` client capability is advertised; the pull-diagnostics hypothesis was wrong)_ and **kotlin-language-server** (still needs a real Gradle/Maven project) against real binaries, then promote them from experimental to validated ([#13](https://github.com/plumbkit/plumb/issues/13), [#3](https://github.com/plumbkit/plumb/issues/3)). |
| **0.12** | Swift on Xcode | Build Server Protocol guidance (`buildServer.json` via `xcode-build-server`) so the semantic tools work on a bare `.xcodeproj` without a SwiftPM manifest. |
| **0.13** | Daemon robustness | Finish crash-safety around plumb's own git writes (~~graceful-shutdown drain + attributable stale-lock reaper~~ _(✓ done)_, following the per-repo serialisation lock); lock-free liveness probe ([#15](https://github.com/plumbkit/plumb/issues/15)). |
| **0.14** | Agent ergonomics | Client-aware tool profiles (shipped); enable a language server live without a restart; configurable workspace access wired through client setup. |
| **0.15** | Honesty + config surface | ~~A trustworthy tokens-saved metric~~ _(✓ shipped — two-axis capability/efficiency model, "estimated (model vN)" labelling)_; full settings-screen coverage of every config field with inline help. |
| **0.16** | Stabilisation + cross-platform proving | Bug cleanup ([#2](https://github.com/plumbkit/plumb/issues/2), [#4](https://github.com/plumbkit/plumb/issues/4), ~~[#14](https://github.com/plumbkit/plumb/issues/14)~~ _(✓ fixed — the test now waits on an index-complete signal; green under `-race`)_); community Linux testing across distros and desktops ([#9](https://github.com/plumbkit/plumb/issues/9)). |
| **0.17** | Distribution + discoverability | MCP Registry and GitHub MCP Registry listings; curated-list submissions; a public-exposure polish pass (version/registry lockstep, doc-count accuracy). |
| **0.18** | Proof + docs | Measured tool-comparison use cases; demo assets; a documentation accuracy pass. |
| **0.19** | Release candidate — **last 0.x** | Feature freeze: fixes and soak only. Gather real-world feedback across several `0.19.x` patches before committing to 1.0. |
| **1.0** | General availability | The stability commitment and the validated-core promise — concurrency-safe writes, the resilient daemon, topology, and memory. |

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
