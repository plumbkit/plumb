# Adding an LSP Adapter

Plumb adapters translate between the generic `lsp.LSPClient` interface and the
quirks of a specific language server binary.  This guide walks through every
step, using `gopls` and `pyright` (both validated) as worked examples.

## Validation levels

| Level | What it means | Example |
|---|---|---|
| **Validated** | Integration tests spawn the real binary in CI. | `internal/lsp/adapters/gopls` |
| **Experimental** | Real Go code, unit-tested with a mocked transport; no binary in CI. | `internal/lsp/adapters/kotlin` |

Promote an adapter from experimental to validated by adding integration tests
(see step 6) and updating its `doc.go` status comment.

---

## Step-by-step checklist

### 1. Create the package

```
internal/lsp/adapters/<name>/
```

Use the language name as the directory name (e.g. `rust` for rust-analyzer,
`typescript` for typescript-language-server).

### 2. Write `doc.go`

```go
// Package <name> is the plumb adapter for <binary-name>, the <description>.
//
// Validation status: experimental — unit-tested with mocked transport.
// No integration test against a real <binary-name> binary exists yet.
// To promote to validated, add integration tests in this package that spawn a
// real binary against testdata/<lang>-fixture/ and update this comment.
package <name>
```

### 3. Implement `LSPClient`

Create `adapter.go`.  The `Adapter` struct must implement every method of
`internal/lsp.LSPClient` (currently 15 methods).  No per-adapter extension
methods — if a language server needs something not in the interface, extend
the interface and update **all** adapters.

Minimum struct shape (mirror gopls or pyright):

```go
type Adapter struct {
    conn   jsonrpc.Caller

    capsMu sync.RWMutex
    caps   *protocol.ServerCapabilities

    subMu  sync.RWMutex
    subID  atomic.Int64
    subs   map[int64]func(string, json.RawMessage)
}

func New(conn jsonrpc.Caller) *Adapter {
    a := &Adapter{conn: conn, subs: make(map[int64]func(string, json.RawMessage))}
    conn.SetNotificationHandler(a.dispatch)
    return a
}
```

The `dispatch` / `Subscribe` pair fans out server-initiated notifications to
registered handlers (cache invalidator, TUI, etc.).  Copy the implementation
verbatim from gopls or pyright — it is identical for every adapter.

### 4. Implement `DefaultInitParams`

Each language server has its own initialisation options.  Provide a
`DefaultInitParams(rootURI string) protocol.InitializeParams` function that
sets sensible defaults for that server.  Examples:

**gopls** (`goplsOptions`):
```go
type goplsOptions struct {
    Analyses    map[string]bool `json:"analyses,omitempty"`
    StaticCheck bool            `json:"staticcheck,omitempty"`
}
// DefaultInitParams enables staticcheck and all default analyses.
```

**pyright** (`pyrightInitOptions`):
```go
type pyrightInitOptions struct {
    PythonVersion    string `json:"pythonVersion,omitempty"`
    TypeCheckingMode string `json:"typeCheckingMode,omitempty"`
}
// DefaultInitParams sets Python 3.12 and "basic" type checking.
```

### 5. Write unit tests

Create `adapter_test.go` in `package <name>_test`.  Use
`internal/lsp/jsonrpc.MockCaller` as the transport — no real binary is needed.

Every `LSPClient` method must be covered.  Required test cases:

| Test | What to verify |
|---|---|
| `TestAdapter_Initialize` | `Initialize` calls the server, stores capabilities, `Capabilities()` returns non-nil. |
| `TestAdapter_Initialized` | `Initialized` sends the notification. |
| `TestAdapter_DidOpenDidClose` | Both notifications are sent in order. |
| `TestAdapter_DocumentSymbols` | Result is decoded and returned. |
| `TestAdapter_WorkspaceSymbols` | Result is decoded and returned. |
| `TestAdapter_Definition` | Result is decoded and returned. |
| `TestAdapter_References` | Result is decoded and returned. |
| `TestAdapter_Hover` | Result is decoded and returned. |
| `TestAdapter_PrepareRename` | Result is decoded and returned. |
| `TestAdapter_Rename` | Result is decoded and returned. |
| `TestAdapter_Subscribe` | Notification delivered to subscriber; not delivered after `unsubscribe()`. |
| `TestAdapter_Capabilities_NilBeforeInitialize` | `Capabilities()` returns nil before `Initialize` is called. |

See `internal/lsp/adapters/pyright/adapter_test.go` for a complete reference
implementation.

**MockCaller quick reference:**

```go
mock := jsonrpc.NewMockCaller()

// Register a handler that returns a canned response.
mock.HandleOK(protocol.MethodDocumentSymbols, []protocol.DocumentSymbol{...})

// Register a handler that returns an error.
mock.HandleErr(protocol.MethodHover, errors.New("not supported"))

// Simulate a server-initiated notification.
_ = mock.Push(protocol.MethodPublishDiagnostics, protocol.PublishDiagnosticsParams{...})

// Inspect recorded calls.
calls := mock.Calls() // []jsonrpc.RecordedCall{{Method: "...", Params: ...}, ...}
```

### 6. Add integration tests (for promoted adapters)

Gate with `//go:build integration` so they are excluded from the default
`go test ./...` run:

```go
//go:build integration

package gopls_test

import (
    "os/exec"
    "testing"
    ...
)

func requireBinary(t *testing.T, name string) {
    t.Helper()
    if _, err := exec.LookPath(name); err != nil {
        t.Skipf("%s not on PATH: %v", name, err)
    }
}
```

The test should:
1. Check the binary is on PATH (`requireBinary`); skip if not.
2. Spawn the binary via `exec.Command`.
3. Pipe its stdin/stdout into `jsonrpc.NewConn`.
4. Wrap the conn in the adapter and call `Initialize` + `Initialized`.
5. Call `DocumentSymbols` (or similar) against a file in `testdata/<lang>-fixture/`.
6. Assert the expected symbol names appear in the result.

See `internal/lsp/adapters/gopls/adapter_test.go` for a complete example.

Run integration tests with:

```sh
go test -tags integration ./internal/lsp/adapters/<name>/...
```

### 7. Register in workspace routing

Add the adapter as a candidate in `internal/workspace/detect.go` (once that
package is implemented).  Map the language ID (e.g. `"rust"`) to the adapter
constructor and the config key.

> **Primary vs secondary.** A workspace root may bind several language servers
> at once (e.g. Go + HTML). Routing keys the pool by `(root, language)` and
> sends each file to the server that owns its extension (`langsupport.ByPath`).
> An adapter needs nothing extra to work as a *secondary* in a root whose
> primary is another language: it simply starts lazily the first time a file of
> its language is touched. Just make sure the language's extensions are listed
> in `internal/langsupport` (and, if the name differs from the config key, that
> `normaliseLangName` folds it — as the tsx/jsx/javascript dialects fold onto
> the typescript adapter).

### 8. Update config defaults

Add a default `LSPConfig` entry in `internal/config/config.go`:

```go
"rust": {
    Command:     "rust-analyzer",
    Args:        []string{},
    RootMarkers: []string{"Cargo.toml"},
    Enabled:     false, // disabled until validated
},
```

### 9. Document in this file

Add a row to the adapter status table below and describe any
language-server–specific behaviour (workspace model, sync requirements, etc.).

---

## Adapter reference

### gopls (Go)

- **Binary**: `gopls` — install with `go install golang.org/x/tools/gopls@latest`
- **Status**: validated — integration tests in `internal/lsp/adapters/gopls/`
- **Workspace model**: requires `rootUri` pointing to the module root (the
  directory containing `go.mod`).
- **Init options**: `staticcheck: true` enables additional static analysis.
- **Sync**: supports both full (`SyncFull`) and incremental (`SyncIncremental`)
  document sync.  Plumb currently sends full-document changes.
- **Notifications**: emits `textDocument/publishDiagnostics` after each
  document change; plumb's cache invalidator uses this to evict stale entries.

### pyright (Python)

- **Binary**: `pyright-langserver` — install with `npm install -g pyright`
- **Status**: validated — integration tests in `internal/lsp/adapters/pyright/`
- **Workspace model**: requires `rootUri` pointing to the Python project root.
  Reads configuration from `pyrightconfig.json` or `pyproject.toml` if present.
- **Init options**: `pythonVersion: "3.12"`, `typeCheckingMode: "basic"`.
- **Sync**: requires full-document sync (`SyncFull`).  Always send the complete
  document text in `DidChange` — incremental diffs are not supported by default.
- **Notifications**: emits `textDocument/publishDiagnostics`.

### jdtls (Java)

- **Binary**: `jdtls` — install jdtls and ensure it is on PATH. Requires Java 21 or later.
  macOS: `brew install jdtls`. SDKMAN: `sdk install java 21-tem`.
  Other platforms: download from https://download.eclipse.org/jdtls/ (`milestones/`
  for stable builds, `snapshots/jdt-language-server-latest.tar.gz` for the rolling
  latest). The GitHub repo does not publish language-server tarballs as releases.
- **Binary name on non-Homebrew installs**: the compiled default is
  `command = "jdtls"`. A manual install may ship the launcher under a different
  name or only as a script — `jdtls.sh` (Linux), `jdtls.bat`/`jdtls.exe`
  (Windows), or an absolute path inside the extracted tarball. Point plumb at it
  with a `command` override:
  ```toml
  [lsp.java]
  enabled = true
  command = "/opt/jdtls/bin/jdtls"   # or "jdtls.bat" on Windows
  ```
- **Status**: validated — unit-tested with mocked transport and integration-tested
  against a real jdtls binary in `internal/lsp/adapters/jdtls/` (gated with
  `//go:build integration`).
- **Root markers**: `pom.xml`, `build.gradle`, `build.gradle.kts`, `.classpath`
- **Workspace model**: requires `rootUri` pointing to the project root (where
  `pom.xml` or `build.gradle` lives). Unlike gopls and pyright, jdtls also
  requires a `-data <dir>` process argument pointing to an Eclipse workspace
  storage directory. Plumb computes a per-workspace data directory automatically
  at `~/.cache/plumb/jdtls-data/<root-hash>` — this is handled in
  `internal/cli/pool.go argsFor`; no manual configuration is needed.
- **Init options**: `settings.java.home` is populated from `$JAVA_HOME` when
  set; otherwise jdtls uses its own JDK detection. Leave `JAVA_HOME` unset to
  let jdtls discover the JDK (recommended with SDKMAN).
- **Sync**: supports full-document sync. Plumb sends full-document changes.
- **Diagnostics**: jdtls publishes `textDocument/publishDiagnostics` for open
  documents. Unlike gopls and pyright, `DidChangeWatchedFiles` alone updates
  the project model but does not reliably trigger immediate diagnostics — a
  subsequent `DidOpen` is needed to request analysis of a specific file.
- **Notifications**: sends `client/registerCapability` during init to register
  file-watcher patterns. The adapter responds `null` (OK) so jdtls's project
  model stays consistent with on-disk state.
- **Enable in config**:
  ```toml
  [lsp.java]
  enabled = true
  ```
- **Cold-start warning**: jdtls starts a JVM and loads Eclipse plugins on first
  run. Initial startup can take 30–60 seconds. Subsequent runs within the same
  daemon lifetime are fast because the JVM stays alive.
- **Resource budget**: jdtls is heavyweight (~0.8–1.5 GB RSS per project), so the
  pool reclaims idle JVMs. After `[lsp.java] idle_timeout` (default 20 m) without
  a tool call, the server is *hibernated* — its process is stopped while the warm
  cache is kept, and the next tool call restarts it transparently. `max_workspaces`
  (default 2) caps concurrent Java JVMs, hibernating the least-recently-used one
  before starting another. Inspect live servers with `plumb debug lsp` (state,
  PID, RSS, idle time); stale `jdtls-data` dirs are pruned after 30 days unused.
  Both knobs are read at pool construction — change them and restart the daemon.

### rust-analyzer (Rust)

- **Binary**: `rust-analyzer` — install with `rustup component add rust-analyzer`
  (the rustup proxy at `~/.cargo/bin/rust-analyzer` dispatches to the toolchain
  component; a bare proxy without the component installed errors).
- **Status**: validated — integration tests in `internal/lsp/adapters/rust/`
  (`testdata/rust-fixture/`).
- **Root markers**: `Cargo.toml`.
- **Workspace model**: requires `rootUri` pointing at the Cargo workspace root
  (the directory containing `Cargo.toml`). Reads configuration from
  `rust-analyzer.toml` and the Cargo manifest.
- **Init options**: none — rust-analyzer reads its configuration from the
  workspace, so `DefaultInitParams` sends no `initializationOptions`.
- **Sync**: full-document sync. Registers file watchers dynamically via
  `client/registerCapability`; the adapter answers and records the glob patterns
  so `DidChangeWatchedFiles` events are filtered to them.
- **Notifications**: emits `textDocument/publishDiagnostics`. Syntax errors are
  reported from rust-analyzer's own front end (no `cargo check` needed); the
  slower `cargo check` flycheck supplies type/borrow diagnostics.
- **Enable in config**:
  ```toml
  [lsp.rust]
  enabled = true
  ```
- **Cold-start warning**: rust-analyzer loads the sysroot and runs
  `cargo metadata` on first attach. On a large workspace this can take
  **minutes** — the canonical "unavailability" case the structural (tree-sitter)
  layer covers while the server warms. The adapter tolerates a long `initialize`
  by not imposing its own deadline on the handshake.

### sourcekit-lsp (Swift)

- **Binary**: `sourcekit-lsp` — ships with the Swift toolchain (Xcode or a
  standalone swift.org toolchain). On macOS it lives at `/usr/bin/sourcekit-lsp`.
- **Status**: validated — integration tests in `internal/lsp/adapters/swift/`
  (`testdata/swift-fixture/`, a SwiftPM package).
- **Root markers**: `Package.swift`.
- **Workspace model**: requires `rootUri` pointing at the SwiftPM package root
  (the directory containing `Package.swift`). sourcekit-lsp derives per-file
  compiler arguments from the package build plan; for Xcode projects it can use a
  build-server `compile_commands.json` instead.
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Sync**: full-document sync. Registers file watchers dynamically via
  `client/registerCapability`; the adapter answers and records the glob patterns
  so `DidChangeWatchedFiles` events are filtered to them.
- **Notifications**: emits `textDocument/publishDiagnostics`. Syntax errors are
  reported from the Swift front end once a file is opened.
- **Enable in config**:
  ```toml
  [lsp.swift]
  enabled = true
  ```

### zls (Zig)

- **Binary**: `zls` — install from https://github.com/zigtools/zls (or
  `brew install zls`).
- **Status**: validated (promoted 2026-06-17) — unit-tested with a mocked
  transport, and the integration test (`internal/lsp/adapters/zig/`,
  `testdata/zig-fixture/`) now runs green against a real zls 0.16: document-symbol
  extraction plus the `DidChangeWatchedFiles`+`DidOpen` → `publishDiagnostics`
  round-trip both pass, once plumb advertised the `textDocument.publishDiagnostics`
  client capability (the earlier "zls is pull-only" hypothesis was wrong).
- **Root markers**: `build.zig`, `build.zig.zon`.
- **Workspace model**: requires `rootUri` pointing at the project root (the
  directory containing `build.zig`); zls resolves the build graph from it.
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Sync**: full-document sync.
- **Notifications**: emits `textDocument/publishDiagnostics`.
- **Enable in config**:
  ```toml
  [lsp.zig]
  enabled = true
  ```
- **Maintenance note**: Zig is pre-1.0; `zls` and `tree-sitter-zig` track the
  language version, so this adapter (and the tree-sitter Zig extractor) are an
  ongoing maintenance surface.

### typescript-language-server (TypeScript / JavaScript)

- **Binary**: `typescript-language-server` — install with
  `npm install -g typescript-language-server typescript`.
- **Status**: validated (promoted 2026-06-16) — unit-tested with a mocked
  transport, and the integration test (`internal/lsp/adapters/typescript/`,
  `testdata/typescript-fixture/`) now runs green against a real
  typescript-language-server 5.3.0: document-symbol extraction plus the
  `DidChangeWatchedFiles`+`DidOpen` → `publishDiagnostics` round-trip both pass.
  It publishes nothing unless the client advertises `textDocument.publishDiagnostics`
  — it does not implement pull diagnostics despite the earlier assumption.
- **Root markers**: `tsconfig.json`, `jsconfig.json`, `package.json`.
- **Serves both languages**: this one server provides the semantic GPS for
  TypeScript *and* JavaScript, so both the `typescript` and `javascript`
  `langsupport` rows name it. A JS-only project (just `package.json`) resolves to
  the `typescript` daemon language and is served fine.
- **Workspace model**: requires `rootUri` at the project root; drives `tsserver`
  underneath.
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Sync**: full-document sync.
- **Notifications**: emits `textDocument/publishDiagnostics`.
- **Enable in config**:
  ```toml
  [lsp.typescript]
  enabled = true
  ```
- **Package-name note**: the adapter package is `typescript`, which collides by
  name (not import path) with the topology `typescript` *extractor* package; the
  daemon imports the adapter aliased as `tsls` in `internal/cli/pool.go`.

### kotlin-language-server (Kotlin)

- **Binary**: `kotlin-language-server` — install with
  `brew install kotlin-language-server` or build from
  https://github.com/fwcd/kotlin-language-server (needs a JDK).
- **Status**: experimental — unit-tested with a mocked transport; the
  integration test (`internal/lsp/adapters/kotlin/`, `testdata/kotlin-fixture/`)
  is written and gated `//go:build integration` and would exercise the same
  round-trip as zls/typescript-language-server above, but the binary isn't
  installed on the validation machine, so it skips rather than fails. Promote
  once it runs green against a real server.
- **Root markers**: `settings.gradle.kts`, `build.gradle.kts`. Note the
  `build.gradle.kts` overlap with Java's markers — with both `[lsp.java]` and
  `[lsp.kotlin]` active, the alphabetical detect order makes Java win for a
  shared marker. Both activate automatically when their server is on PATH; if
  both are present, force Kotlin with `session_start({"language": "kotlin"})` or
  set `[lsp.java] enabled = false`.
- **Workspace model**: requires `rootUri` at the project root; resolves the
  classpath from the Gradle/Maven build files (slow on first attach).
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Sync**: full-document sync.
- **Notifications**: emits `textDocument/publishDiagnostics`.
- **Enable in config**:
  ```toml
  [lsp.kotlin]
  enabled = true
  ```

---

## Common pitfalls

**Root markers vs. root URI**: Many language servers expect `rootUri` to be the
workspace root (where the manifest file lives), not an arbitrary subdirectory.
Use `os.Getwd()` + a marker check, or let the user configure it.

**Full vs. incremental sync**: Always check `ServerCapabilities.TextDocumentSync`
after `Initialize` to confirm which sync mode the server negotiated.  Pyright
requires full sync; gopls accepts both.

**BoolOrOptions capabilities**: LSP capability fields can be `true` (boolean)
or a detailed options object.  `protocol.BoolOrOptions` handles both; use
`caps.HoverProvider.Enabled` to check support.

**Notification handler registration**: Call `conn.SetNotificationHandler` from
within `New()`, before any LSP methods are called.  If you set it after
`Initialize`, you may miss notifications that arrive during initialisation.

**Test binary path (macOS + Airlock Digital)**: On macOS with Airlock Digital,
`go test` compiles test binaries to a temp directory that is blocked.  Run
tests with `GOTMPDIR=$(pwd)/.testcache go test ./...`.  The Makefile `test`
and `test-race` targets already set this.
