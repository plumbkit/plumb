# Adding an LSP Adapter

Plumb adapters translate between the generic `lsp.LSPClient` interface and the
quirks of a specific language server binary.  This guide walks through every
step, using `gopls` (validated) and `pyright` (experimental) as worked examples.

## Validation levels

| Level | What it means | Example |
|---|---|---|
| **Validated** | Integration tests spawn the real binary in CI. | `internal/lsp/adapters/gopls` |
| **Experimental** | Real Go code, unit-tested with a mocked transport; no binary in CI. | `internal/lsp/adapters/pyright` |

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
