# Architecture

Plumb is an MCP (Model Context Protocol) server that exposes LSP (Language
Server Protocol) capabilities to LLMs.  Instead of dumping raw source files
into an LLM's context, clients call structured tools (`workspace_symbols`,
`get_definition`, `explain_symbol`) and receive focused, language-aware
answers from a real language server running under the hood.

## Layers

```mermaid
flowchart TD
    P["Presentation — internal/tui · internal/cli"]
    A["Application — internal/tools · internal/cache · internal/quality"]
    D["Domain — internal/domain · internal/workspace"]
    I["Intelligence — internal/topology (SQLite/FTS5)"]
    T["Transport — internal/mcp · internal/lsp"]
    P --> A --> D --> I --> T
```

**Rule: lower layers must never import higher layers.**  The transport layer
knows nothing about tools or the CLI; tools know nothing about the TUI.

### Package map

| Package | Role |
|---|---|
| `cmd/plumb` | Entry point — calls `cli.Execute()` |
| `internal/cli` | Cobra subcommands: `serve`, `daemon`, `stop`, `init`, `setup`, `version`, `config`, `sessions`, `stats` (alias `status`), `diagnostics`, `doctor`, `log-level`; per-connection session wiring; workspace + topology pools |
| `internal/tui` | Bubble Tea v2 TUI: dashboard widgets, sessions, memory, logs, settings, stats, and recent calls |
| `internal/tools` | MCP tool implementations (58 tools — see `docs/tools.md`); `WriteDeps` bundles write-tool dependencies; the `txlog` subpackage is the transaction rollback WAL |
| `internal/quality` | Offline post-write code analysers (golangci-lint, ruff, …) against changed files; findings appended to write responses; `golangcilint` subpackage |
| `internal/cache` | Sharded TTL cache + LSP invalidator |
| `internal/session` | Per-connection session registry with client identity tracking |
| `internal/stats` | Global SQLite tool-call statistics, row-scoped by workspace and session (WAL, per-tool summary, P95, client-aware, `user_version` 16); also holds the `episodic_memories` table for idle-session summaries |
| `internal/memory` | Per-workspace markdown memory store (`<workspace>/.plumb/memories/`) |
| `internal/topology` | SQLite/FTS5 semantic graph; background indexer; Go AST + pure-Go tree-sitter (gotreesitter — most languages incl. TypeScript/TSX/JSX) + canonical-grammar WASM via wazero for Swift (`extractors/{golang,treesitter,wasmts}`); search + BFS explore/impact/affected/routes |
| `internal/render` | Shared, pure CLI/TUI presentation helpers (leaf-level: stdlib + rendering libs only) |
| `internal/fsguard` | Guards filesystem walks against macOS TCC false-positive prompts on protected dirs ($HOME, Desktop, Documents, …) |
| `internal/monitor` | Process resource-usage snapshots (CPU %, memory) plus the daemon start time, with per-OS implementations; feeds the TUI daemon metrics and its uptime baseline |
| `internal/mcp` | MCP server, `Tool` interface, stdio transport, hook callbacks |
| `internal/lsp` | `lsp.Client` interface (23 methods), process supervisor |
| `internal/lsp/jsonrpc` | JSON-RPC 2.0 over LSP content-framed stdio (server-request support); mock for testing |
| `internal/lsp/protocol` | LSP types and method-name constants |
| `internal/lsp/adapters/base` | The half of every adapter that is identical across servers: all 23 `lsp.Client` methods, capability cache, notification fan-out, server-request handler, error labelling. Adapters embed `*base.Adapter` |
| `internal/lsp/adapters/gopls` | Validated Go adapter (unit- + integration-tested) |
| `internal/lsp/adapters/pyright` | Validated Python adapter (unit- + integration-tested) |
| `internal/lsp/adapters/jdtls` | Java adapter (validated, experimental tier); activates automatically when `jdtls` (+ a Java 21+ runtime) is on PATH; set `[lsp.java] enabled = false` to exclude |
| `internal/config` | TOML config, XDG path resolution, project-config merging; `config.Store` holds the live global base (atomic pointer + generation + observers) for hot-reload |
| `internal/domain` | Reserved for future shared domain types (currently empty) |
| `internal/workspace` | Reserved for future routing logic (currently empty) |

### The base adapter's exported surface

`internal/lsp/adapters/base` is the half of every adapter that is identical
across servers: the plumbing behind all 23 `lsp.Client` methods, the capability
cache, the notification fan-out, the server-request handler, and the
`"<server> <label>: <cause>"` error labelling. Adapters embed `*base.Adapter`
and shadow only what their server does differently.

**Its exported surface is exactly `lsp.Client`, and must stay that way.** Go
promotes an embedded type's exported methods into all nine adapters, and
`internal/cli` resolves optional capabilities *structurally* — so one extra
exported method here silently opts every language server into a capability it
does not have.

The consequences for anyone editing that package:

- Escape hatches are package-level **functions** (`base.Call`, `CallPtr`,
  `CallRaw`, `Notify`, `Wrap`), never methods.
- `base.OpenTracker` (lazy `didOpen` for swift/zig/html/kotlin) is held as a
  **named field**, never embedded.
- Four tests guard it: `TestExportedSurface_IsExactlyLSPClient`,
  `TestAdapters_OptionalInterfaceSurface`,
  `TestLazyOpenAdapters_LanguageIDAndExportedSurface`, and
  `TestLazyOpenAdapters_DidOpenMatrix` — the per-method `didOpen` count of each
  lazy-open adapter, in both directions. **The ensure-open set is asymmetric on
  purpose**, so making the two sides "consistent" is not tidying; it is a
  behaviour change the matrix will catch.

### Structural extractors and their memory discipline

`internal/topology/extractors/` holds three engines, and which one is
production for a language is a deliberate per-language choice
(`internal/langsupport` is the registry, and the seam for moving a language):

| Extractor | Status |
|---|---|
| `golang` | Go, via `go/parser`+`go/ast` (no CGo) |
| `treesitter` | gotreesitter (pure Go), pinned v0.51.0, embeds the `grammars` package (~+26 MB). Python, Ruby, C, C#, Elixir, Scala, PHP, JSON, CSS, SCSS, XML, Lua, C++, Objective-C, Dart, JavaScript, Rust, Zig, Kotlin, Swift, Java, Bash, HCL, SQL, Dockerfile, TOML, YAML, Markdown, HTML. JavaScript (`.js`/`.mjs`/`.cjs`) and TypeScript/TSX/JSX (`.ts`/`.tsx`/`.jsx`) are primary here since the v0.48.0 per-language flip — 435/435 corpus extraction parity |
| `wasmts` | Grammar-generic WASM extractor driven by wazero (pure Go). Production for **Swift only**: the canonical alex-pinkus `tree-sitter-swift` grammar plus its C external scanner (`swift.wasm`, ~3.5 MB, `make swift-wasm`), held until gotreesitter clears six residual Swift parse shapes. Its TypeScript + TSX bundle (`ts.wasm`, ~2.9 MB, `make ts-wasm`) is no longer wired into production and is kept as the parity-sweep reference. Each bundle has its own builder; both need Zig only to regenerate |

Config/IaC/markup grammars extract named declarations; TOML/YAML/Markdown/HTML
also index nesting via containment edges, and HTML and Markdown are flagged
`PreferStructuralOutline` so outline tools use the Map over the noisy LSP.

`internal/topology/extractors/typescript` is the legacy regex TS/JS extractor.
It is no longer production-reachable since the TS flip (it was the `wasmts` TS
init-failure fallback) and is retained only for the parity harness.

**Memory discipline.** Each extractor decodes its grammar **lazily** — a
`lazyGrammar` resolved on first `Extract`, not in the constructor — and
`defer tree.Release()`s its parse arena back to gotreesitter's pool after the
walk. So grammar memory scales with the languages a workspace actually
contains rather than the full supported set, and a resync recycles one arena
instead of allocating per file. This is why idle daemon RSS is dominated by
that pool rather than by a leak.

### Charm dependency rule

Plumb's UI stack is Bubble Tea v2 only. Use `charm.land/bubbletea/v2`,
`charm.land/lipgloss/v2`, and `charm.land/bubbles/v2` for all TUI and CLI
presentation work. Do not add or import the v1 module paths (`charm.land/bubbletea`,
`charm.land/lipgloss`, or `charm.land/bubbles`); mixing v1 and v2 Charm modules
causes incompatible model, command, and style types.

## Plumb Topology vs. LSP (The Dual-Engine Architecture)

Plumb pairs two complementary technologies to solve the context efficiency problem for AI agents: **Topology** and **LSP**. They do not compete; they handle different phases of the agent's workflow.

```mermaid
flowchart LR
    Q["Agent question"] --> TOPO["Topology (the Map)<br/>FTS5 search · BFS explore<br/>instant, syntactic"]
    TOPO -->|found where to work| LSP["LSP (the GPS)<br/>rename · diagnostics · references<br/>precise, type-aware"]
    LSP --> EDIT["Safe edit + verify"]
```

### 1. Plumb Topology (The Map)
Topology uses **Go AST plus pure-Go tree-sitter (gotreesitter) extractors for most languages, and a canonical-grammar WASM (wazero) extractor for Swift,** and a local **SQLite/FTS5** database to maintain a persistent semantic graph of the codebase (symbols, calls, imports). **On by default** (opt out with `[topology] enabled = false`). It is exposed through the `topology_*` tools (`topology_status`, `topology_search`, `topology_explore`, `topology_impact`, `topology_affected`, `topology_routes`) plus `structural_query`. See the dedicated [Topology guide](topology.md) for an accessible overview of what it is, why it exists, and how it works.
*   **Strengths:** Instant availability (no LSP boot time), minimal memory footprint, handles broken code gracefully, FTS5 ranked search, BFS neighbourhood exploration.
*   **Role in Plumb:** Discovery engine. When an agent asks "Where is the routing logic?" or needs to see a symbol's neighbourhood, Topology handles it without waiting for the language server to index.
*   **Trade-offs:** Syntactic extraction only — no type resolution. "Broad" recall, not compiler-level precision.

### 2. Language Server Protocol (The GPS)
LSP uses heavy, compiler-backed servers (like `gopls` or `pyright`) to provide 100% accurate semantic truth.
*   **Strengths:** Perfect type awareness, safe cross-file refactoring, real-time diagnostics, and compiler-level guarantees.
*   **Role in Plumb:** Precision engine. Once Topology has helped the agent find *where* to work, LSP tools (`rename_symbol`, `diagnostics`) safely manipulate the code and verify the change.
*   **Trade-offs:** Heavy resource usage, slow indexing times on large codebases ("startup gap"), and strictly limited to languages where the user has a validated LSP installed.

By combining the **Speed and Breadth of Topology** with the **Precision and Safety of LSP**, Plumb provides agents with the optimal balance of token efficiency and operational reliability.

## Tooling Ecosystem Integration

While Plumb focuses on providing the **Map** (Topology) and the **GPS** (LSP), it is designed to work within a broader ecosystem of language-specific tooling. This ecosystem ensures code quality, consistency, and correctness through several key categories:

1. **Formatters (fmt)**: These tools (e.g., `gofumpt`, `ruff format`, `prettier`) automatically adjust code layout. Plumb integrates with formatters via the `format_after: true` flag in tools like `find_replace`, ensuring that any text-based changes adhere to the project's style guide.
2. **Linters (lint)**: Linters (e.g., `golangci-lint`, `ruff check`, `eslint`) analyze code for bugs and convention violations. While LSP provides real-time feedback, linters often perform deeper or more opinionated checks that complement the LSP's diagnostics.
3. **Type Checkers**: For dynamic languages, type checkers (e.g., `mypy`, `pyright`, `tsc`) provide static guarantees. Plumb surfaces these guarantees by proxying the diagnostics emitted by the underlying language server.
4. **Test Runners**: Automated tests (e.g., `go test`, `pytest`, `jest`) validate the semantic correctness of changes. Plumb currently delegates test execution to the user or the MCP client, but the changes it makes are intended to be verified by these runners.
5. **Build Tools**: Orchestrators (e.g., `go build`, `maven`, `npm`) manage dependencies and compilation. The LSP relies on these tools to maintain an accurate internal project model, and Plumb ensures that filesystem writes are atomic and visible to the build system.

## Data flow: MCP tool call

```mermaid
sequenceDiagram
    participant C as MCP client
    participant S as mcp.Server
    participant T as tools (Tool.Execute)
    participant Ca as cache
    participant P as cli.routingProxy
    participant A as adapter (gopls/pyright)
    participant LS as language server

    C->>S: tools/call (JSON-RPC over stdio)
    S->>T: dispatch to Tool.Execute()
    T->>Ca: check cache
    alt cache hit
        Ca-->>T: cached result
    else cache miss
        T->>P: LSP request (per-session routing)
        P->>A: forward to pooled adapter
        A->>LS: JSON-RPC 2.0, Content-Length framed
        LS-->>A: response
        A-->>T: response (via proxy)
        T->>Ca: store result
    end
    T-->>C: text result
```

Cache invalidation runs in the opposite direction: when gopls sends a
`textDocument/publishDiagnostics` notification, `cache.Invalidator.Handle`
is invoked (via `adapter.Subscribe`) and evicts all cache entries whose key
contains the changed file's URI.

## Daemon architecture

`plumb serve` is a thin stdio proxy. The real server is `plumb daemon`, a
long-lived background process that owns the gopls subprocesses:

```mermaid
flowchart TD
    CC["Claude Desktop / Claude Code"] --> SV["plumb serve (per conversation)"]
    SV -->|Unix socket| SOCK["plumb.sock"]
    SOCK --> D["plumb daemon (one shared process)"]
    D --> WP["workspacePool — one language server per (root, language)"]
    WP --> PE1["poolEntry · (/projects/web, go) — gopls + cache + invalidator + refcount"]
    WP --> PE2["poolEntry · (/projects/web, html) — vscode-html-language-server (lazy secondary)"]
    WP --> PE3["poolEntry · (/projects/bar, python) — pyright + cache + invalidator + refcount"]
    D --> HC["handleConn — per-connection MCP session"]
```

Key design properties:

- **Single source of truth for runtime files** — socket, PID and locks live
  under `paths.RuntimeDir()`: `$XDG_RUNTIME_DIR/plumb` on Linux/BSD when that is
  usable, else `os.UserCacheDir()/plumb`, which keeps paths stable across
  GUI-app and terminal launches (macOS `$TMPDIR` is unreliable across these).
  "Single source" is literal — one function, consulted by the CLI, the TUI and
  both command sandboxes. The daemon log is the one exception: it lives in the
  OS log dir (`~/Library/Logs/plumb/` on macOS), not the runtime dir. See
  *Persistence layout* for the `$XDG_RUNTIME_DIR` validity checks.
- **Singleton enforced by `flock(2)`** — two advisory locks (`plumb.spawn.lock`
  held briefly by `plumb serve` around its dial-or-spawn block, and
  `plumb.daemon.lock` held by `plumb daemon` for its lifetime) guarantee at
  most one daemon process ever binds the socket. Without them, two `plumb
  serve` processes racing from a cold start could each spawn a daemon and the
  second would `os.Remove(socketPath); net.Listen(...)`, quietly stealing the
  path from the first. Lock release is automatic via fd close on process
  exit (clean or crash) — see `internal/cli/lock.go`.
- **One language server per (root, language)** — multiple MCP connections to
  the same project share each server, its cache, and its diagnostic stream. A
  root may bind several servers at once (e.g. Go + HTML for a web app): each
  file is routed to the server that owns its extension. The **primary** language
  (resolved from root markers — `go.mod` beats `index.html` when both are
  present) is pinned, and each connection that attaches a workspace as its
  primary holds a reference; when the last session on a root detaches, the
  primary is torn down after a 90 s idle grace so it stays warm across a quick
  disconnect-reconnect but is eventually reclaimed when the workspace is idle.
  **Secondary** servers start lazily on the first file of their language and
  live to daemon shutdown. A secondary activates automatically when its server
  binary is on PATH (`[lsp.<lang>] enabled = false` excludes it).
- **Per-connection sessions** — `handleConn` registers a `session.Info`
  immediately on connection (with `Folder=""` until workspace resolves). The
  session is then patched as workspace and client identity become known.
  This means `plumb sessions` and the TUI show new conversations *instantly*,
  not after the first LSP tool call.
- **Stop strategy** — `plumb stop` searches for the daemon in three stages:
  PID file → `lsof` on the socket → `pgrep -f "plumb daemon"`. The pgrep
  fallback covers binary upgrades that change the socket/PID path.

### Resilient proxy

`plumb serve` is a frame-aware reconnecting proxy (`internal/cli/serve_proxy*.go`) that survives a daemon crash or hang without the client noticing. On a daemon failure it keeps the client's stdio open, dial-or-spawns a fresh daemon, and **replays the captured MCP handshake** (the client only sends `initialize` once). In-flight requests get a synthesised retryable error (`code -32000`) instead of hanging; non-idempotent writes are never auto-replayed. A *hung* daemon is caught by an idle `ping` heartbeat, then `SIGTERM`→`SIGKILL`'d and respawned. Reconnects are bounded. The replayed handshake also carries a stable per-proxy session ID (in the `initialize` params' `_meta`) so the fresh daemon recognises the reconnected connection as a continuation and rehydrates its persisted per-connection state (see `[session]` `persist_state`) — making the restart transparent rather than merely non-fatal. Knobs: `PLUMB_PROXY_RECONNECT` (default on; off ⇒ legacy `io.Copy` proxy), `PLUMB_PROXY_HEARTBEAT` (`0` disables hang detection), `plumb serve --no-reconnect`.

After a reconnect the proxy also emits `notifications/tools/list_changed` to the
client. The daemon fires that itself when a connection's tool *profile* changes,
but it cannot for a restart — the restart is what destroys the connection that
would have sent it — and only the proxy survives the gap. Without it, a tool a
rebuilt daemon **gained** stays invisible: the client fetched its tool list once
at connect, and from its side the server never went away, so it never re-lists.
The notification is unconditional rather than gated on a version change, because
`serverInfo.version` is the `VERSION` file and the dev loop (`make reinstall`)
rebuilds the daemon with a new tool set at the *same* version — the case this
exists for, and the one a version check would miss.

**Upgrade caveat.** This behaviour lives in `plumb serve`, and a proxy process is
spawned per client connection and lives as long as that connection. Upgrading
plumb therefore does **not** upgrade the proxies of sessions already running:
each client keeps the binary it started with, and gains this from its *next*
session onward. So the first daemon rebuild after an upgrade still leaves a new
tool invisible to any client connected across it. `plumb daemon_info` reports the
proxy's own version alongside the daemon's when the two differ.

### Per-connection write deadline

Each MCP response write to the socket is bounded by a `SetWriteDeadline` (`internal/mcp/server.go`; default 30 s, `PLUMB_WRITE_TIMEOUT`; `0`/`off` disables). Without it a blocked `conn.Write` would hold the connection's write mutex forever and wedge every later reply on that connection (`daemon_info` included) to the client timeout. A lapsed deadline fails the write, marks the connection broken, and cancels its `Serve` loop so the connection is torn down with a clear error — the resilient proxy then reconnects — rather than hanging. Transports without `SetWriteDeadline` (test pipes) are unaffected.

### Memory bounds & introspection

At startup the daemon applies a *soft* heap limit via `debug.SetMemoryLimit` (`internal/cli/memlimit.go`): `PLUMB_MEMORY_LIMIT` (a byte size like `1500MiB`, or `0`/`off`/`unlimited` to disable) overrides a tight-but-comfortable 1 GiB anti-OOM backstop default — Go GCs harder near the limit and never hard-fails, so a transient spike is bounded. The active limit is logged. Three admin commands over `plumb.ctrl.sock` expose live state: `plumb debug mem` prints a `runtime.ReadMemStats` snapshot (`HeapAlloc`/`HeapInuse`/`HeapSys`/`HeapReleased`/`NumGC`/`Goroutines`), `plumb debug heap` forces a GC and writes a `runtime/pprof` heap profile to the cache dir (`plumb.heap.<ns>.pprof`) for `go tool pprof`, and `plumb debug stacks` writes a full goroutine stack dump (`plumb.stacks.<ns>.txt`, the pprof `goroutine` profile at `debug=2` — the non-destructive `SIGQUIT` equivalent) for diagnosing a live hang. A full topology resync ends with `debug.FreeOSMemory()` so the large transient working set returns to the OS rather than lingering as idle heap spans. Note: the TUI daemon widget's RSS row is the *current* sample, not a peak.

## Workspace detection

`workspacePool.Detect(dir)` walks up from `dir`:

1. **`.plumb/` marker** — explicit workspace. Returns `(dir, language)` if an LSP language is detectable here or in an ancestor; otherwise `(dir, "none")` (filesystem tools, stats, project config still work; LSP tools fail until a language attaches).
2. **A strong language root marker** (`go.mod`, `Cargo.toml`, `Package.swift`, `pyproject.toml`, …) at `dir` or any ancestor — returns `(dir, language)`.
3. **A `.git/` directory** — an unambiguous project boundary. Returns `(dir, "none")` so a multi-language repo with no language marker still resolves. `$HOME` is excluded; nearest-wins, so a `.plumb/` or language marker beats a `.git/` further up. **Content sniff (last resort):** before returning `"none"` at a `.git/` boundary (or resolving a `.plumb/` marker root), plumb scans that root — bounded, up to 2 levels deep, noise dirs pruned — for source files of an **active** language and, if one dominates, resolves that language instead (`extLangAt`, `pool_detect.go`). So a git repo full of `.py` files with **no** `pyproject.toml`/`setup.py` attaches Python when pyright is installed — matching the "install → on" philosophy for ecosystems with no mandatory manifest. It fires only after all strong/weak markers fail, is confined to the confirmed boundary (never ascends, never `$HOME`), and is gated on the language server being installed.

Walks to the parent otherwise; errors after passing the filesystem root — or on **reaching the home directory**, which terminates the walk. `$HOME` (matched by filesystem identity across both the `$HOME` environment variable and the OS user database, so a symlinked spelling or a repointed environment cannot slip past) is never a detection result and never ascended past: a dotfiles `.git` or a stray `~/go.mod` must not turn the whole home directory into a workspace, and nothing above it can legitimately be a project root. The one thing honoured *at* `$HOME` is a **deliberate** `.plumb/` marker — one carrying a `context.md` (`plumb init`) — `config.toml` is deliberately not accepted, since plumb writes it itself; a bare or purely machine-generated `.plumb/` there is treated as auto-attach residue and ignored, with a logged remediation.

**Child-marker discovery (multi-language monorepo).** Detection only walks *up*. A `.plumb/` root that carries no language marker of its own — a Ghostty-style monorepo where the languages live one level down (`core/build.zig` + `app/Package.swift` under a bare `.plumb/` root) — would otherwise resolve as `LanguageNone` with nothing attached. So on a `LanguageNone` attach the daemon additionally descends up to `[workspace] child_scan_depth` levels (default 2) for **strong** language root markers in subdirectories (`discoverChildLanguages`, `pool_detect.go`; prunes dotdirs/`node_modules`/build outputs, stops at a matched root, never scans `$HOME`). Each discovered child language attaches its own server **rooted at the subdirectory** (`core/` for zig, `app/` for swift) via the existing multi-LSP-per-root machinery; one is elected the connection **primary** (go-first, then alphabetical), the rest attach lazily on first file. All discovered languages are listed in the `session_start` identity line (e.g. `Language: Swift, Zig`) and `workspace_sessions`, and `workspace_symbols` **fans out** across them. Discovery runs only when the root has no language of its own — a root with its own `go.mod` is untouched, with its child languages attaching lazily as before.

**Strong vs weak root markers.** Promiscuous markers — `package.json` (typescript), `index.html` (html) — are **weak** (`weak_root_markers`): they name the language only of the directory they sit in directly (the resolution dir, or a `.git/`/`.plumb/` boundary), **never** an ancestor. So a stray tooling `package.json` up the tree (a docs build, or a global `~/package.json`) cannot hijack a Go/Swift/Rust workspace as TypeScript; a real JS/TS project — `package.json` at its own root or `.git` boundary — still resolves. Strong markers always beat weak ones at the same directory. The ancestor walks are additionally bounded at `$HOME`: a stray marker in the home directory never captures a workspace beneath it.

**Automatic enablement (install → on).** Every language is `enabled = true` by default; the *effective* set is gated on the server binary being installed (`exec.LookPath`, cross-platform — honours `PATHEXT` on Windows). So installing `rust-analyzer` activates Rust for every Cargo project with no config; a language whose server is absent stays dormant at zero cost and its markers never enter detection. Set `[lsp.<lang>] enabled = false` to exclude a language even when its server is installed. `plumb config show` prints an `active` row per language (`yes (installed)` / `no (… not installed)` / `no (disabled in config)`); `plumb doctor` reports the same.

**Detection uses global config, not project config.** `Detect`/`detectLanguageAt` consult the daemon's resolved **global** language set, *before* any `<root>/.plumb/config.toml` loads. So a language enabled **only** in a subfolder's project config (e.g. `[lsp.html] enabled = true` in `site/.plumb/config.toml`) does **not** make that subfolder resolve as that language — enable it in **global** config. With multi-LSP-per-root (0.9.0) this rarely bites: enable the secondary globally and per-file routing sends each extension to the right server within one workspace, no subfolder pin needed.

`LanguageNone` (`"none"`) keeps non-Go/non-Python projects fully attached minus LSP; the `.git/` fallback extends this to any git repo, so a repo without a language marker resolves on the first path-bearing tool call. **Auto-attach** (opt-in, `[workspace].auto_attach`) covers the residual case — a seed dir with *no* `.git/` above — via `SynthesiseRoot`; synthetic sessions are marked `(auto)`, and `auto_attach_persist` creates `<root>/.plumb/` on first attach. `SynthesiseRoot` carries the same home-directory rules as `Detect` — the ascent stops at `$HOME`, falling back to the seed — plus one of its own: a seed that *is* `$HOME` is honoured only for an **explicit** pin (a `session_start` `workspace` arg, live or replayed with that stored origin). An incidental seed — a tool-path argument, a client-reported root, a persisted pin from a weaker origin — is refused, because reading `~/.zshrc` is not a declaration that the home directory is the workspace. `auto_attach_persist` likewise refuses to materialise `~/.plumb`. **How the root is chosen is part of the boundary** (see `docs/threat-model.md`, B3): every path check downstream is only as tight as the root selection that fed it.

Cold-start resolution in `session_start`: the daemon's already-attached root → explicit `workspace` arg → `roots/list` query → the serve-proxy cwd hint → otherwise an honest "pass `workspace`" error. There is **no daemon-side `os.Getwd()` fallback** (the daemon is a singleton shared across connections) — but the per-conversation `plumb serve` proxy *is* cwd-aware and transports its working directory in the initialize `_meta` (`dev.plumbkit/workspace`) as an **advisory attach hint**: consulted only after client roots and the persisted pin, always validated through `Detect` (marker required, `$HOME` excluded), and never persisted as the sticky pin. So a client that reports no folder (Claude Desktop sends no `roots`) attaches automatically when its `plumb serve` was launched in the project directory; otherwise pin the project with an absolute `workspace`. Run `plumb init` to create a `.plumb/` marker.

**Reconnect resolution in `OnInit`** is a different, longer ladder — the connection is being re-established, so the caller is not there to pass anything. Highest wins, first-wins-idempotent (`internal/cli/conn_attach_oninit.go`):

1. the workspace the proxy watched an accepted `session_start` succeed with, replayed in the initialize `_meta` under `dev.plumbkit/pinned-workspace`;
2. the persisted pin whose stored source is `session_start`;
3. the client's `roots/list`;
4. a persisted pin whose source is `roots`, or a legacy row with no source;
5. the serve-proxy cwd hint above, then first-tool-call path seeding.

Rung 1 outranks client roots because it is the workspace the caller chose, not where the client happened to launch — ranking roots first is what silently undid a deliberate re-pin and sent relative-path writes into the wrong repository (issue #181). But rung 1 carries **rank, not full authority**: the daemon reads that `_meta` key from whatever process spoke to it and cannot tell a genuine proxy replay from any other client that set it, so it does not receive the home-containment exemption — neither at attach nor on a later path-policy rebuild — and a wide claim nothing else corroborates marks the session blocked (issue #318). See `docs/threat-model.md` for what the daemon can and cannot verify about a declaration.

**Forcing the primary language.** `session_start` also takes an optional `language` arg (the `[lsp.<lang>]` key) that forces the primary language server when detection cannot infer it — now rarely needed for Xcode since `*.xcodeproj`/`*.xcworkspace` are root markers; still useful when even those are absent (e.g. a loose `.swift` directory), where the workspace would otherwise be `LanguageNone` (per-file routing still attaches secondaries, but `workspace_symbols` and the hierarchies need a *primary*). It re-pins the current workspace (or pins alongside an explicit `workspace`) to that server and shows it in the identity line. The language must be active (installed + enabled); an unknown/uninstalled/disabled value is ignored and normal detection applies.

**Single-workspace-per-connection contract.** Once a connection has attached a workspace, every path-bearing tool refuses paths outside the connection's allowed roots with a `workspace boundary violation` error (the allowed set is the workspace plus `extra_roots` read-write and `read_roots` / Go dependency roots read-only). `rename_symbol` also boundary-checks each output URI before applying. To switch projects, call `session_start` with an explicit `workspace`: a deliberate `workspace` arg re-pins the connection (re-attaching LSP/topology/quality/config) rather than being refused — clients may reuse one `plumb serve` across chats, so a fresh chat is not a fresh connection. The pin is **sticky once explicit** (issue #182): after a pin was set by `session_start`, a conflicting re-pin to a different project is refused unless the caller passes `force: true`, and a `roots/list_changed` that drops the pinned root can no longer move it — a peer agent multiplexed over the same `plumb serve` connection must not silently steal another agent's workspace (the refusal error names the `force: true` remediation, so a deliberate switch still self-heals; a roots- or auto-attach-held pin is not sticky, so the first explicit pin always lands). A connection that hits a violation is marked `Health: blocked` for the TUI, as is a refused sticky re-pin (cleared by the next successful explicit `session_start`, same-root or forced). `git`'s `repo` arg defaults to the pinned workspace when omitted.

## Persistence layout

| Path | Owner | Purpose |
|---|---|---|
| `~/.config/plumb/config.toml` | user | LSP commands, cache TTL, log level |
| `~/.local/share/plumb/sessions/<id>.json` | daemon | Active session metadata (one file per MCP connection) |
| `~/.local/share/plumb/stats.db` | daemon (writer) / TUI + `plumb stats` (readers) | Global tool call statistics, SQLite WAL, row-scoped by workspace and session |
| `<runtime>/plumb.sock` | daemon | Unix socket for MCP proxy connections |
| `<runtime>/plumb.pid` | daemon | PID for `plumb stop` lookup |
| `<runtime>/plumb.version` | daemon | Build version; `plumb serve` warns on mismatch |
| `<runtime>/plumb.ctrl.sock` | daemon | Admin socket; line-based commands such as `set-level <level>` from `plumb log-level` and `web-start` from `plumb web` |
| `<runtime>/plumb.spawn.lock` | serve | Advisory `flock` serialising daemon spawn decisions across racing `plumb serve` processes |
| `<runtime>/plumb.daemon.lock` | daemon | Advisory `flock` held for the daemon's lifetime; rejects duplicate daemons |
| `~/.local/state/plumb/daemon.log` | daemon | slog text output (OS log dir; `~/Library/Logs/plumb/` on macOS) |
| `<workspace>/.plumb/context.md` | user | Project-wide context loaded at session start |
| `<workspace>/.plumb/memories/<name>.md` | LLM via memory tools | Per-workspace persistent notes |
| `<workspace>/.plumb/topology.db` | daemon (when `[topology] enabled`) | Per-workspace SQLite/FTS5 semantic code index (rebuildable) |
| `<workspace>/.plumb/memory.db` | daemon (when `[memory] enabled`) | Per-workspace SQLite/FTS5 index over `.plumb/memories/*.md` (rebuildable) |

`<runtime>` is `paths.RuntimeDir()`: `$XDG_RUNTIME_DIR/plumb` on Linux/BSD when
that variable is usable, otherwise `os.UserCacheDir()/plumb`
(`~/Library/Caches/plumb` on macOS, `~/.cache/plumb` on Linux). It is one
function, and the CLI, the TUI's daemon-liveness check and both command
sandboxes all resolve through it — a sandbox that disagreed would go on
protecting an empty directory.

`$XDG_RUNTIME_DIR` is preferred because that is what the spec designates for
sockets: a per-user tmpfs, mode 0700, owned and cleaned up by the login session.
plumb applies the checks the spec puts on the consumer — absolute, exists, is a
directory, mode 0700, owned by the caller — and falls back rather than trusting
the variable, since a world-readable runtime dir would expose the daemon socket.
The fallback is the cache dir and deliberately **not** `os.TempDir()`: on macOS
`$TMPDIR` differs between a GUI-app launch and a terminal launch, so the socket
would move depending on how the client started plumb. (`os.TempDir()` is the
last resort behind that, reached only when `os.UserCacheDir()` itself fails —
which means `$HOME` is unset.)

The runtime directory determines the socket, the control socket, the pid and
the version file **as a set**: every command resolves all of them from the one
directory it computed. That is a deliberate constraint. Connecting to one
directory's socket while reading another's files was tried and reverted — it
left `plumb web` and `plumb log-level` dialling a control socket that was not
there, doctor calling a version file missing when it existed one directory
over, and `plumb restart` spawning a duplicate.

The consequence is that the runtime directory identifies the daemon instance,
and `$XDG_RUNTIME_DIR` is absent under cron, systemd system units,
`docker exec` and ssh without `pam_systemd`. A plumb launched from one of those
contexts therefore uses the cache dir and will start its own daemon rather than
reusing a desktop session's. `plumb serve` warns before doing so and
`plumb doctor` names the other directory rather than only reporting "cannot
dial"; `plumb stop` consolidates, since it finds daemons by process name
regardless of which socket they opened. This is the same property the flock
singleton has always had — `plumb.daemon.lock` lives *in* the runtime dir, so it
has only ever guaranteed one daemon per directory.

XDG: `XDG_DATA_HOME` (sessions and stats) and `XDG_CONFIG_HOME` (config) are
respected when set.

plumb resolves these locations through `internal/paths`, which delegates to
`github.com/adrg/xdg` for config/data/state/cache. The daemon log is the sole
hand-rolled per-OS path: macOS keeps user logs in `~/Library/Logs/` (which no
XDG base maps to), so `paths.LogDir` special-cases it. The table above shows the
Linux layout. On macOS config and data (sessions, `stats.db`) live under
`~/Library/Application Support/plumb/`, the daemon log under `~/Library/Logs/plumb/`,
and cache files (socket, pid, locks) under `~/Library/Caches/plumb/`. A pre-0.9.8 config at
`~/.config/plumb/config.toml` is still read as a fallback.

### Databases at a glance

plumb persists to **three** SQLite databases — one **global**, two **per
project** — alongside plain files (config, sessions, markdown memories). The
split follows ownership: the daemon is a singleton shared across every
conversation, so global state lives in one file keyed by `workspace` and
`session_id`, while each project's semantic indexes live inside that project's
`.plumb/` directory.

| Database | Scope | Location (Linux / macOS) | Tables | Lifecycle |
|---|---|---|---|---|
| `stats.db` | **Global** — every project, one per daemon | `~/.local/share/plumb/` · `~/Library/Application Support/plumb/` | `tool_calls`, `episodic_memories` | Durable primary data; forward-migrated (`PRAGMA user_version`, currently 16) |
| `topology.db` | **Per project** | `<workspace>/.plumb/` | `topology_files`, `topology_nodes`, `topology_edges`, `topology_fts`, `topology_embeddings` | Rebuildable index; dropped & recreated on a version bump |
| `memory.db` | **Per project** | `<workspace>/.plumb/` | `memory_files`, `memory_records`, `memory_fts` | Rebuildable index over the markdown memories |

Only `stats.db` holds primary data, so it is the only one with data-preserving
migrations. The two per-project databases are *rebuildable* indexes — their
source of truth lives elsewhere (the working tree for `topology.db`, the
markdown files under `.plumb/memories/` for `memory.db`) — so `.plumb/.gitignore`
excludes them and a schema bump simply drops and rebuilds rather than migrating.
All three open in WAL mode.

```mermaid
flowchart TD
    D["plumb daemon (singleton)"]
    D --> SDB[("stats.db — GLOBAL<br/>~/…/share/plumb/")]
    D --> W1["workspace /projects/foo"]
    D --> W2["workspace /projects/bar"]
    W1 --> T1[(".plumb/topology.db")]
    W1 --> M1[(".plumb/memory.db")]
    W2 --> T2[(".plumb/topology.db")]
    W2 --> M2[(".plumb/memory.db")]
```

The per-database schemas, indexes, and relationships follow.

### Statistics database (`stats.db`)

Single global SQLite file with two tables. The primary `tool_calls` table
records every MCP tool invocation:

```sql
CREATE TABLE tool_calls (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id   TEXT    NOT NULL DEFAULT '',  -- session.Info.ID
    session_name TEXT    NOT NULL DEFAULT '',  -- session.Info.Name
    workspace    TEXT    NOT NULL DEFAULT '',  -- absolute project root
    tool         TEXT    NOT NULL,              -- e.g. "workspace_symbols"
    called_at    INTEGER NOT NULL,              -- Unix milliseconds
    duration_ms  INTEGER NOT NULL DEFAULT 0,    -- wall-clock execution time
    input_bytes  INTEGER NOT NULL DEFAULT 0,    -- raw JSON arg length
    output_bytes INTEGER NOT NULL DEFAULT 0,    -- text response length
    success      INTEGER NOT NULL DEFAULT 1,    -- 1 = ok, 0 = error
    error_msg    TEXT    NOT NULL DEFAULT '',
    input_json   TEXT    NOT NULL DEFAULT '',   -- raw tool args, capped
    output_text    TEXT  NOT NULL DEFAULT '',   -- tool output, capped
    client_name    TEXT  NOT NULL DEFAULT '',   -- MCP clientInfo.name
    client_version TEXT  NOT NULL DEFAULT '',   -- MCP clientInfo.version
    tokens_saved          INTEGER NOT NULL DEFAULT 0,  -- counterfactual savings total
    savings_model_version INTEGER NOT NULL DEFAULT 0,  -- scoring-model version (0 = pre-redesign, excluded)
    capability_tokens     INTEGER NOT NULL DEFAULT 0,  -- work a thin client couldn't do natively
    efficiency_tokens     INTEGER NOT NULL DEFAULT 0,  -- fewer tokens for the same result
    purpose               TEXT    NOT NULL DEFAULT ''  -- optional session purpose tag (session.Info.Purpose)
);
CREATE INDEX idx_tc_tool      ON tool_calls(tool);
CREATE INDEX idx_tc_called_at ON tool_calls(called_at);
CREATE INDEX idx_tc_session   ON tool_calls(session_id);
CREATE INDEX idx_tc_workspace ON tool_calls(workspace);
CREATE INDEX idx_tc_ws_session ON tool_calls(workspace, session_id);
CREATE INDEX idx_tc_tool_dur  ON tool_calls(tool, duration_ms);
```

The second table, `episodic_memories` (added in schema v8), stores the
rule-based summaries written when a session goes idle — the "last session"
recap surfaced at `session_start`:

```sql
CREATE TABLE episodic_memories (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace     TEXT    NOT NULL DEFAULT '',
    session_id    TEXT    NOT NULL DEFAULT '',
    session_name  TEXT    NOT NULL DEFAULT '',
    generated_at  INTEGER NOT NULL,              -- Unix milliseconds
    summary       TEXT    NOT NULL DEFAULT '',
    touched_files TEXT    NOT NULL DEFAULT '',
    read_count    INTEGER NOT NULL DEFAULT 0,
    write_count   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_em_ws ON episodic_memories(workspace, generated_at);
```

Neither table is FK-linked: both carry `workspace` + `session_id` as plain
columns so the single global store can be filtered down to one project or one
session at query time.

Concurrency model:

- **WAL journal mode** (`?_journal_mode=WAL`) is enabled at open time. This
  is essential because the daemon (writer) and the TUI / `plumb stats`
  (readers) run in separate OS processes — WAL allows concurrent readers
  while a single writer proceeds, without blocking either side.
- **Single writer** — the daemon process holds one connection
  (`SetMaxOpenConns(1)`) protected by an internal `sync.Mutex` around the
  insert path. There is no UPDATE or DELETE traffic.
- **Read-only readers** — the TUI opens the same file with `?mode=ro`. If
  the file does not yet exist (no calls recorded), the read open returns
  `(nil, nil)` and the caller renders "No statistics yet."
- **Best-effort writes** — `Record` returns insert errors so the daemon can log
  storage failures, but stats must never break a tool call.

Every successful or failed `tools/call` triggers `srv.OnAfterTool`, which the
daemon connects to `statsDB.Record(stats.Call{...})` capturing tool name,
workspace, session, timing, and I/O sizes. The workspace and session fields are
required row attributes because the single stats database contains all projects
served by the single daemon.

Schema versioning is driven by `PRAGMA user_version` (currently 16). `stats.Open()`
(the daemon — the single writer) applies forward migrations (`ALTER TABLE ADD
COLUMN`) when the on-disk version is older, then stamps the current version, so
existing history is preserved across upgrades. `OpenReadOnly()` (TUI, `plumb
stats`) does not migrate; it reports a schema-upgrade-required notice until the
daemon migrates the file.

### Topology database (`topology.db`)

Per-workspace semantic index of the code graph — **nodes** (symbols) and
**edges** (relationships between them) — plus an FTS5 search index and an
optional embedding cache. Built and kept live by the background indexer; it
backs the six `topology_*` tools and the LSP→topology fallback. Lives at
`<workspace>/.plumb/topology.db` (WAL, `PRAGMA foreign_keys = ON`). Defined in
`internal/topology/db.go`.

```sql
CREATE TABLE topology_meta (            -- key/value: schema version, last sync
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE topology_files (           -- one row per indexed file
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    path          TEXT    NOT NULL UNIQUE,       -- workspace-relative path
    language      TEXT    NOT NULL DEFAULT '',
    mtime_ns      INTEGER NOT NULL DEFAULT 0,    -- freshness anchor
    content_hash  TEXT    NOT NULL DEFAULT '',    -- SHA-256; reindex trigger
    extractor_ver TEXT    NOT NULL DEFAULT '',
    indexed_at    INTEGER NOT NULL DEFAULT 0,
    error_msg     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_tf_path ON topology_files(path);

CREATE TABLE topology_nodes (           -- one row per symbol
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id    INTEGER NOT NULL REFERENCES topology_files(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,                 -- function, type, method, …
    name       TEXT    NOT NULL DEFAULT '',      -- unqualified name
    qualified  TEXT    NOT NULL DEFAULT '',      -- fully-qualified name
    signature  TEXT    NOT NULL DEFAULT '',
    start_line INTEGER NOT NULL DEFAULT 0,
    end_line   INTEGER NOT NULL DEFAULT 0,
    docstring  TEXT    NOT NULL DEFAULT '',
    language   TEXT    NOT NULL DEFAULT '',
    has_bytes      INTEGER NOT NULL DEFAULT 0,   -- 1 ⇒ byte/col spans valid
    start_byte     INTEGER NOT NULL DEFAULT 0,
    end_byte       INTEGER NOT NULL DEFAULT 0,
    start_col      INTEGER NOT NULL DEFAULT 0,
    end_col        INTEGER NOT NULL DEFAULT 0,
    doc_start_byte INTEGER NOT NULL DEFAULT 0,   -- 0 ⇒ no doc comment
    doc_end_byte   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_tn_file ON topology_nodes(file_id);
CREATE INDEX idx_tn_name ON topology_nodes(name);
CREATE INDEX idx_tn_kind ON topology_nodes(kind);

CREATE TABLE topology_edges (           -- directed relationship between two nodes
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id    INTEGER NOT NULL REFERENCES topology_nodes(id) ON DELETE CASCADE,
    to_id      INTEGER NOT NULL REFERENCES topology_nodes(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,                 -- call, reference, contains, …
    confidence REAL    NOT NULL DEFAULT 1.0,
    source     TEXT    NOT NULL DEFAULT 'extractor'  -- 'extractor' | 'inferred'
);
CREATE INDEX idx_te_from ON topology_edges(from_id);
CREATE INDEX idx_te_to   ON topology_edges(to_id);

-- FTS5 search index; rowid mirrors topology_nodes.id
CREATE VIRTUAL TABLE topology_fts USING fts5(
    name, name_tokens, qualified, signature, docstring, path, kind,
    tokenize = 'unicode61 remove_diacritics 2'
);

-- opt-in embedding cache for semantic re-rank; keyed by content hash (not node
-- id) so a vector survives a resync and is shared across identical symbol text
CREATE TABLE topology_embeddings (
    model        TEXT    NOT NULL,
    content_hash TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    vector       BLOB    NOT NULL,               -- little-endian float32
    PRIMARY KEY (model, content_hash)
);
```

Relationships (every FK is `ON DELETE CASCADE`, so deleting a file removes its
nodes, and deleting a node removes its edges):

```mermaid
erDiagram
    topology_files ||--o{ topology_nodes : "file_id"
    topology_nodes ||--o{ topology_edges : "from_id"
    topology_nodes ||--o{ topology_edges : "to_id"
    topology_nodes ||--|| topology_fts   : "rowid = node id"
```

`topology_fts` is an FTS5 virtual table whose `rowid` equals the indexed
`topology_nodes.id`. `topology_embeddings` is deliberately **not** FK-linked to
a node — keying it on `(model, content_hash)` lets one vector serve every symbol
with identical text and outlive a reindex.

**Lifecycle.** `topology.db` is a *rebuildable* index, versioned by `PRAGMA
user_version` (currently 1). When the on-disk version is older the indexer DROPs
and recreates every table rather than migrating — the working tree is the source
of truth, so the resync that runs at each attach repopulates it. `topology.db`
(and its `-wal`/`-shm` siblings) is auto-added to `<workspace>/.plumb/.gitignore`.

### Memory index database (`memory.db`)

Per-workspace FTS5 index over the markdown memory files in
`<workspace>/.plumb/memories/`. The markdown files stay the source of truth;
`memory.db` is a rebuildable, ranked search index (plain `grep` is the fallback
when it is stale or absent). Lives at `<workspace>/.plumb/memory.db` (WAL,
`PRAGMA foreign_keys = ON`, a single serialised connection). Defined in
`internal/memory/index.go`.

```sql
CREATE TABLE memory_meta (              -- key/value metadata
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

CREATE TABLE memory_files (             -- one row per .plumb/memories/<name>.md
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    content_sha TEXT    NOT NULL DEFAULT '',     -- freshness anchor
    mtime_ns    INTEGER NOT NULL DEFAULT 0,
    size_bytes  INTEGER NOT NULL DEFAULT 0,
    indexed_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE memory_records (           -- parsed frontmatter + body, one per file
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id        INTEGER NOT NULL REFERENCES memory_files(id) ON DELETE CASCADE,
    name           TEXT    NOT NULL UNIQUE,
    description    TEXT    NOT NULL DEFAULT '',
    paths_json     TEXT    NOT NULL DEFAULT '',  -- path globs for hint injection
    source_paths   TEXT    NOT NULL DEFAULT '',  -- provenance …
    source_symbols TEXT    NOT NULL DEFAULT '',
    source_session TEXT    NOT NULL DEFAULT '',
    source_calls   TEXT    NOT NULL DEFAULT '',
    confidence     TEXT    NOT NULL DEFAULT 'user', -- user|generated|imported|inferred
    content_sha    TEXT    NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL DEFAULT 0,
    updated_at     INTEGER NOT NULL DEFAULT 0,
    last_used_at   INTEGER NOT NULL DEFAULT 0,
    supersedes     TEXT    NOT NULL DEFAULT '',  -- lineage, by name
    superseded_by  TEXT    NOT NULL DEFAULT '',
    stale_after    INTEGER NOT NULL DEFAULT 0    -- 0 ⇒ never expires
);
CREATE INDEX idx_mr_conf ON memory_records(confidence);
CREATE INDEX idx_mr_used ON memory_records(last_used_at);

-- FTS5 search index; rowid mirrors memory_records.id
CREATE VIRTUAL TABLE memory_fts USING fts5(
    name, name_tokens, description, body, path_globs,
    source_paths, source_symbols, provenance,
    tokenize = 'unicode61 remove_diacritics 2'
);
```

```mermaid
erDiagram
    memory_files   ||--o{ memory_records : "file_id"
    memory_records ||--|| memory_fts     : "rowid = record id"
```

`memory_records.supersedes` / `superseded_by` track memory lineage by `name`
(logical, not a FK). Like `topology.db`, `memory.db` is a rebuildable index
(`PRAGMA user_version` 1): on any markdown add/change/delete the index is
reconciled against the files (content-SHA + size freshness check), and a version
bump drops and rebuilds it. It is gitignored alongside `topology.db`.

### Session registry (`sessions/<id>.json`)

Each `handleConn` invocation writes one JSON file on connect and removes it
on disconnect (`defer session.Unregister(sessID)`). Fields:

```go
type Info struct {
    ID            string    // 12-hex-time + 8-hex-random
    PID           int       // daemon's PID, used for liveness check
    Language      string    // "go" today; one per connection
    Folder        string    // "" until workspace resolves; absolute path otherwise
    Adapter       string    // "gopls"
    StartedAt     time.Time
    ClientName    string    // from MCP `initialize.clientInfo.name`
    ClientVersion string    // from MCP `initialize.clientInfo.version`
}
```

`session.List()` filters out files whose PID is no longer running (it removes
them on the way out), so the on-disk view self-heals after daemon crashes.

`session.Patch(id, fn)` is the read-modify-write API used to update fields as
they become known: client identity arrives during `initialize`; folder
arrives later, when `OnInit` or `OnBeforeTool` resolves the workspace.

### Memory store (`<workspace>/.plumb/memories/<name>.md`)

Per-workspace markdown notes for persistent project context. Names are
constrained to `[A-Za-z0-9_-]+` to prevent path traversal. Files may carry
optional YAML-style frontmatter:

```markdown
---
name: auth-architecture
description: How the auth middleware composes with rate limiting
---

The auth middleware sits in front of …
```

The `description` field is surfaced by `list_memories` so the LLM can decide
whether to load the full body. Writes are atomic (`<file>.tmp` + rename).
Memory tools use a `WorkspaceFn` accessor to default to the connection's
resolved workspace when the caller doesn't pass `workspace` explicitly,
making cross-project memory access possible.

## Startup sequence

`plumb serve` is a resilient stdio proxy: it takes the spawn lock, dials the
daemon socket (spawning `plumb daemon` if none is running), and proxies MCP
frames between the client and the Unix socket. It is frame-aware and
reconnecting — on a daemon crash or hang it respawns the daemon and replays the
captured `initialize` handshake so the client never notices (`--no-reconnect`
falls back to a plain byte copy). It registers no tools and owns no LSP processes.

```mermaid
sequenceDiagram
    participant C as MCP client
    participant SV as plumb serve
    participant D as plumb daemon
    participant CS as connSession
    C->>SV: launch (stdio)
    SV->>SV: acquire spawn lock
    SV->>D: dial socket (spawn daemon if absent)
    C->>D: initialize
    D->>CS: handleConn → register session.Info
    Note over CS: workspace resolves lazily<br/>(roots/list, then cwd/path walk)
    CS->>CS: acquire language server, cache, project config
    CS->>CS: registerAllTools + lifecycle hooks
    C->>D: tools/call …
```

The daemon does the real work. For each accepted connection, `handleConn` builds
a `connSession` (`internal/cli/conn.go`) which:

1. Registers a `session.Info` immediately (Folder empty until the workspace resolves).
2. Resolves the workspace lazily — via `roots/list` on `initialize`, then by
   walking up from the first tool call's path argument.
3. On attach: acquires the shared primary language server for the workspace from
   `workspacePool` (one per (root, language); secondaries spin up lazily as
   files of other enabled languages are touched), opens the per-connection cache + invalidator,
   loads project config, and — when enabled — acquires the topology store and
   the quality runner.
4. Registers all MCP tools (`registerAllTools`) and lifecycle hooks
   (`registerHooks`: `OnInit`, `OnRootsChanged`, `OnBeforeTool`, `OnAfterTool`,
   `OnClientInfo`).

On daemon shutdown (SIGINT/SIGTERM) the workspace and topology pools are stopped,
the stats DB is closed, and the socket / PID / lock files are removed.

## Concurrency model

| Component | Contract |
|---|---|
| `mcp.Server.Serve` | One goroutine per in-flight request; responses serialised by a `sync.Mutex`. |
| `cache.Cache` | Sharded map; each shard has its own `sync.RWMutex`. Stats counters use `atomic.Int64`. |
| `cache.Invalidator` | Called from the adapter's notification goroutine; thread-safe via the cache's own locking. |
| `lsp/jsonrpc.Conn` | Write serialised by `sync.Mutex`; pending calls tracked in a `sync.Map`; read loop on a dedicated goroutine. |
| `cli.routingProxy` | Per-session proxy; `sync.RWMutex` around the primary pool-entry pointer; set on workspace attach. |
| `config.Store` | Live global config; `Current()`/`Generation()` are lock-free atomic loads; `publishMu` serialises reloads so generations/notifications stay ordered; listeners invoked outside the lock so they may re-enter `Current`/`LoadProject`. |
| `lsp.Supervisor` | Supervision loop on one goroutine; exported methods protected by `sync.RWMutex`. |
| `adapters/*.Adapter` | Capabilities stored under `sync.RWMutex`; subscribers stored under `sync.RWMutex`; notification dispatch copies the handler slice before releasing the lock. |

## Transport protocols

### MCP (client ↔ plumb)

Newline-delimited JSON-RPC 2.0 over stdio.  Each message is one UTF-8 line.
No Content-Length header.  Protocol version: negotiated per connection at `initialize` —
the client's offered revision when plumb implements it, otherwise the newest entry of the
supported set (currently `2024-11-05`).

Handled methods: `initialize`, `ping`, `tools/list`, `tools/call`.
Notifications (no `id` field) are accepted and silently discarded.

### LSP (plumb ↔ language server)

JSON-RPC 2.0 with LSP content framing over the subprocess's stdin/stdout:

```
Content-Length: <N>\r\n
\r\n
<N bytes of UTF-8 JSON>
```

`internal/lsp/jsonrpc.Conn` implements the framing and demultiplexes
responses by request ID using a `sync.Map` of pending channels.

## Configuration

Config file: `$XDG_CONFIG_HOME/plumb/config.toml`
(defaults to `~/.config/plumb/config.toml`).

Environment overrides: `PLUMB_LOG_LEVEL`, `PLUMB_LOG_FILE`, `PLUMB_LOG_FORMAT`
(and the other `PLUMB_*` variables). The running daemon's level can also be
changed live with `plumb log-level <level>` — there is no `--log-level` flag.

Configuration resolves in layers, each overriding the previous:

```mermaid
flowchart LR
    A["Compiled defaults"] --> B["Global config.toml"]
    B --> C["Project .plumb/config.toml"]
    C --> E["Environment variables"]
```

The global base config is held in a live `config.Store` (`internal/config/store.go`)
and **hot-reloaded** without a daemon restart. Three inputs trigger a reload: an
fsnotify watch on the global `config.toml` (debounced; the directory is watched so
the reload survives `config.Save`'s atomic temp-file→rename), the `reload-config`
control-socket command (used by `plumb config reload`), and the TUI settings editor
after a save. Each MCP session subscribes to the store and re-merges its per-project
view on a change, so `[edits]`, `[git]`, `[walk]`, and the rate limit apply live;
`[topology]` is reconciled live by the daemon (`topologyPool.Reconcile`). Settings
the daemon cannot apply live — LSP server definitions (`[lsp.*]`), `[cache]`, and
`log_format` (`config.RestartSensitiveEqual`) — are reported as restart-needed by
`Store.RestartNeeded()`: a daemon WARN on the offending reload, a line in the
`daemon_info` tool, and a "Reload behaviour" legend in `plumb config show`.

See [`docs/configuration.md`](configuration.md) for every section and field,
and `plumb config show` for the resolved values with per-field provenance.

## Cache key convention

Tools prefix cache keys with the document URI so that `cache.InvalidateByPath`
can evict all results for a changed file in one scan:

```
file:///project/main.go:docSymbols
file:///project/main.go:hover:10:5
file:///project/main.go:def:10:5
wsSymbols:Greeter
```

`cache.Invalidator` calls `cache.InvalidateByPath(uri)` which does a
`strings.Contains` scan over all shard entries.  This is O(total entries) but
only triggered on `textDocument/publishDiagnostics` notifications.

## Error handling

- LSP errors propagate as `fmt.Errorf("…: %w", err)` up through the tool and
  are returned to the MCP client as `isError: true` result payloads (not
  JSON-RPC error objects), per the MCP spec.
- JSON-RPC protocol errors (unknown method, bad params) are returned as
  JSON-RPC `error` objects.
- Supervisor restart errors are logged with `slog` and the supervisor retries
  with exponential backoff (base 500 ms, max 30 s).

## Adding features

- **New MCP tool**: follow the `add-mcp-tool` project skill in `.claude/skills/`
  (the full checklist), summarised in `docs/contributing.md`.
- **New LSP adapter**: see `docs/adding-an-lsp.md`.
- **New config field**: add to `config.Config`, update `defaults`, add
  validation in `validate()`, document in this file and in
  `docs/configuration.md`. If the daemon cannot apply the field without a
  restart (e.g. an LSP-process or cache setting), add it to
  `config.RestartSensitiveEqual` so `Store.RestartNeeded()` reports it;
  otherwise it is picked up live via the per-session store subscription.
  Consider exposing it as a row in the TUI Settings screen
  (`internal/tui/model_settings.go`).
