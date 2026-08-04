# Adding an LSP Adapter

Plumb adapters translate between the generic `lsp.Client` interface
(`internal/lsp/client.go`, 23 methods) and the quirks of a specific language
server binary.

**An adapter is small.** Everything identical across servers — the JSON-RPC
plumbing behind all 23 methods, the negotiated-capability cache, the
notification fan-out, the server-request handler that records watcher
registrations, and the `"<server> <label>: <cause>"` error labelling — lives in
`internal/lsp/adapters/base`. A language adapter embeds `*base.Adapter` and adds
only what its server genuinely does differently. The minimal adapter
(`internal/lsp/adapters/rust`) is 46 lines.

Three worked examples, in increasing order of difficulty:

| Example | Shows |
|---|---|
| `internal/lsp/adapters/rust` | The minimal case: embed the base, declare `DefaultInitParams`, done. |
| `internal/lsp/adapters/typescript` | Adding an optional capability (the document-pull surface) in the adapter's own package. |
| `internal/lsp/adapters/swift` | Lazy document opening via `base.OpenTracker`, plus a union decode over `base.CallRaw`. |

---

## Why the shape is what it is

Read this before writing code; two of the three rules below are non-obvious and
cost real debugging time when a reader gets them wrong.

### The promotion hazard — never add an exported method to `base.Adapter`

`base.Adapter` exposes **exactly** the 23 methods of `lsp.Client` and nothing
more. This is a hard constraint, not a style preference.

Go promotes every exported method of an embedded type into its embedder, and
plumb resolves optional adapter capabilities **structurally**, not by
declaration:

- `internal/cli/pool_adapters.go` type-asserts `lsp.PullInitializer`
  (`EnablePullDiagnostics`);
- `internal/cli/routing_proxy_pull.go` and
  `internal/lsp/conformance/conformance.go` assert the document-pull shape
  (`SupportsPullDiagnostics` + `Diagnostic`) and the workspace-pull shape
  (`WorkspaceDiagnostic`).

So one extra exported method on the base is promoted into **all nine adapters
at once**. A single stray `SupportsPullDiagnostics` would opt every language
server that answers `-32601` to `textDocument/diagnostic` into the pull model —
a regression that compiles, changes no call site, and surfaces only as broken
diagnostics at runtime.

That is why every escape hatch the base offers is a package-level **function**,
never a method: `base.Call[T]`, `base.CallPtr[T]`, `base.CallRaw`, `base.Notify`,
`base.Wrap`. Functions are never promoted. An adapter that needs a method beyond
`lsp.Client` declares it **in its own package**, implemented over those helpers —
see `typescript.Adapter.Diagnostic`.

The same reasoning applies to `base.OpenTracker`: the three lazy-open adapters
hold it as a **named field** (`open *base.OpenTracker`), never embedded, because
embedding would promote `Ensure` and `Refresh` into the adapter's exported
surface.

Four tests catch a breach:

| Guard | Where | What it pins |
|---|---|---|
| `TestExportedSurface_IsExactlyLSPClient` | `internal/lsp/adapters/base/base_test.go` | The base's exported method set is exactly `lsp.Client`. |
| `TestAdapters_OptionalInterfaceSurface` | `internal/lsp/conformance/optional_interfaces_test.go` | Which adapters expose the optional pull surfaces — in **both** directions. |
| `TestLazyOpenAdapters_LanguageIDAndExportedSurface` | `internal/lsp/conformance/lazyopen_guard_test.go` | The lazy-open adapters' `languageId` and exported surface. |
| `TestLazyOpenAdapters_DidOpenMatrix` | `internal/lsp/conformance/lazyopen_guard_test.go` | The per-method `didOpen` count for each lazy-open adapter, asserted in **both** directions. |

### `base.New` installs both transport handlers

`base.New(conn, "<server>")` registers the notification fan-out **and** the
server-request handler. `dispatch` and `handleServerRequest` are unexported, so
although they are promoted into an embedder they are not selectable from another
package: an adapter cannot forget to wire them, and cannot re-wire them
incorrectly. The old "forgot `SetRequestHandler`, server stalls waiting for a
`client/registerCapability` reply" bug is now unrepresentable.

### The ensure-open set is deliberately asymmetric

Some servers answer a per-document request only for a document opened via
`textDocument/didOpen`: sourcekit-lsp replies `-32001 "No language service for
<uri> found"`, zls resolves an unopened file to nothing, and
vscode-html-language-server has no filesystem access at all. plumb drives servers
with `didChangeWatchedFiles` rather than the open-document lifecycle, so those
adapters open lazily on first query and drop the copy when the file changes on
disk.

**Which methods need the open differs per server**, and that is why each
ensure-open site is written out in the concrete adapter rather than hidden in the
base. sourcekit-lsp needs it for `Rename` (a caller may skip `PrepareRename`);
zls deliberately does not. The HTML server needs it for the queries but not for
the rename or hierarchy prepares, which it answers from the document it already
holds. Copying another adapter's set without checking your server is how this
goes wrong.

Since the `base.Adapter` migration the asymmetry is carried by method
**absence** — an adapter that does not shadow a method inherits the base's plain
forward, with no ensure-open — so making the three adapters "consistent" looks
like tidying and is not: it changes what plumb puts on the wire to three real
servers, under cover of a refactor.
`TestLazyOpenAdapters_DidOpenMatrix`
(`internal/lsp/conformance/lazyopen_guard_test.go`) is the record of what each
server actually needs. It pins the `didOpen` count of every `lsp.Client` method
on every lazy-open adapter in **both** directions: a 0 that becomes a 1 fails as
loudly as a 1 that becomes a 0. Read its doc comment before you change an
ensure-open set — including your own adapter's.

---

## Validation levels

| Level | What it means | Example |
|---|---|---|
| **Validated** | Integration tests spawn the real binary and pass. | `internal/lsp/adapters/gopls`, `.../rust` |
| **Experimental** | Real Go code, unit-tested with a mocked transport; the integration test exists but has not run green against a real binary. | `internal/lsp/adapters/kotlin`, `.../html` |

Promote from experimental to validated by getting the integration test (step 6)
green against a real server binary, then updating the adapter's `doc.go` status
comment and the validation table in `AGENTS.md`.

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
// No integration test against a real <binary-name> binary has run green yet.
// To promote to validated, get the integration test in this package green
// against a real binary and update this comment.
package <name>
```

### 3. Write `adapter.go`

Four things, in this order. Nothing else is required.

```go
package <name>

import (
	"github.com/plumbkit/plumb/internal/lsp"
	"github.com/plumbkit/plumb/internal/lsp/adapters/base"
	"github.com/plumbkit/plumb/internal/lsp/jsonrpc"
	"github.com/plumbkit/plumb/internal/lsp/protocol"
)

// Adapter implements lsp.Client for <binary-name>.
//
// Document the server's workspace model and any quirk a reader would otherwise
// have to rediscover from a wire trace.
//
// Concurrency: all exported methods are safe for concurrent use.
type Adapter struct{ *base.Adapter }

// Compile-time contract check: a mis-signed method fails here, in this package,
// rather than as a confusing error wherever the adapter is used as an lsp.Client.
var _ lsp.Client = (*Adapter)(nil)

// New creates an Adapter wired to conn. The caller must call Initialize before
// any query method.
func New(conn jsonrpc.Caller) *Adapter {
	return &Adapter{Adapter: base.New(conn, "<binary-name>")}
}

// DefaultInitParams returns InitializeParams suitable for <binary-name>.
// rootURI must be a file:// URI pointing to the <server's> workspace root.
func DefaultInitParams(rootURI string) protocol.InitializeParams {
	return protocol.InitializeParams{
		ProcessID:    protocol.ProcessID(),
		ClientInfo:   &protocol.ClientInfo{Name: "plumb", Version: "dev"},
		RootURI:      rootURI,
		Capabilities: protocol.DefaultClientCapabilities(),
	}
}
```

The string passed to `base.New` is the server label — normally the binary name.
It prefixes every error the adapter returns, so it is user-visible and pinned by
`conformance.RunErrorContract`; pick the name an operator would recognise
(`sourcekit-lsp`, not `swift`; `pyright` for `pyright-langserver`) and do not
change it casually.

`DefaultInitParams` is the one genuinely per-server function. Most servers need
no `initializationOptions` at all (rust-analyzer, sourcekit-lsp, zls,
typescript-language-server, vscode-html-language-server, kotlin-language-server
all send none). When a server does need them, declare an unexported options
struct next to it:

```go
// goplsOptions holds gopls-specific initialization options.
// See https://github.com/golang/tools/blob/master/gopls/doc/settings.md
type goplsOptions struct {
	Analyses    map[string]bool `json:"analyses,omitempty"`
	StaticCheck bool            `json:"staticcheck,omitempty"`
}
```

A user-configured `[lsp.<lang>] initialization_options` table replaces these
defaults verbatim (`internal/cli/pool_adapters.go`, `defaultInitParamsFor`), so
keep the adapter's own defaults conservative.

### 4. Shadow only what your server does differently

Every method not shadowed is inherited from the base. When you do shadow one,
call through with `a.Adapter.X(ctx, params)` so the base still does the
transport, the labelling, and the watcher filtering.

**Pre-flight before the base call** (lazy open, swift/zig/html):

```go
type Adapter struct {
	*base.Adapter

	// open is held as a NAMED field, never embedded: embedding it would promote
	// Ensure and Refresh into this adapter's exported surface.
	open *base.OpenTracker
}

func New(conn jsonrpc.Caller) *Adapter {
	b := base.New(conn, "<binary-name>")
	return &Adapter{Adapter: b, open: base.NewOpenTracker(b, "<languageId>")}
}

func (a *Adapter) Hover(ctx context.Context, params protocol.HoverParams) (*protocol.Hover, error) {
	if err := a.open.Ensure(ctx, params.TextDocument.URI); err != nil {
		return nil, err
	}
	return a.Adapter.Hover(ctx, params)
}
```

`<languageId>` is the LSP `languageId` the server expects (`swift`, `zig`,
`html` for the three above) — servers key their parser off it, so a wrong one
compiles and fails silently at runtime rather than at build time.

Pair it with a `DidChangeWatchedFiles` shadow that calls
`a.open.Refresh(ctx, params.Changes)` before delegating — and pass the
**unfiltered** change list: plumb's copy of a document is stale whatever the
server's watcher globs say.

**A reply shape the base cannot decode** (union types) — use `base.CallRaw` and
label your own decode failure with `base.Wrap`:

```go
raw, err := base.CallRaw(ctx, a.Adapter, "definition", protocol.MethodDefinition, params)
if err != nil {
	return nil, err
}
locs, err := protocol.DecodeLocations(raw)
if err != nil {
	return nil, base.Wrap(a.Adapter, "definition", err)
}
return locs, nil
```

**An optional capability beyond `lsp.Client`** — declare it here, never on the
base (see the promotion hazard):

```go
func (a *Adapter) SupportsPullDiagnostics() bool {
	caps := a.Capabilities()
	return caps != nil && caps.PullDiagnosticsEnabled()
}

func (a *Adapter) Diagnostic(ctx context.Context, params protocol.DocumentDiagnosticParams) (*protocol.DocumentDiagnosticReport, error) {
	return base.CallPtr[protocol.DocumentDiagnosticReport](ctx, a.Adapter, "diagnostic", protocol.MethodDiagnostic, params)
}
```

If the adapter implements an interface `internal/cli` resolves structurally, add
a compile-time assertion for that too — gopls asserts
`_ lsp.PullInitializer = (*Adapter)(nil)` alongside `_ lsp.Client`.

Pick the helper by return shape: `base.Call[T]` for a value (a `null` reply
still yields a nil slice for a slice `T`), `base.CallPtr[T]` for a pointer to a
decoded struct, `base.CallRaw` for an undecoded reply, `base.Notify` for a
notification, `base.Wrap` for an adapter-side failure that never reached the
transport.

### 5. Write tests

Three harnesses, all in `package <name>_test`. No real binary is needed for any
of them.

**Unit tests** (`adapter_test.go`) — use `internal/lsp/jsonrpc.MockCaller` as the
transport. Do **not** re-test the inherited base methods method-by-method: the
base has its own suite. Test what this adapter adds — its
`DefaultInitParams`, each shadowed method, and any optional capability.
`internal/lsp/adapters/swift/adapter_test.go` is the reference for a lazy-open
adapter.

```go
mock := jsonrpc.NewMockCaller()

// Register a handler that returns a canned response.
mock.HandleOK(protocol.MethodDocumentSymbols, []protocol.DocumentSymbol{ /* … */ })

// Register a handler that returns an error.
mock.HandleErr(protocol.MethodHover, errors.New("not supported"))

// Simulate a server-initiated notification.
_ = mock.Push(protocol.MethodPublishDiagnostics, protocol.PublishDiagnosticsParams{ /* … */ })

// Inspect recorded calls.
calls := mock.Calls() // []jsonrpc.RecordedCall{{Method: "...", Params: ...}, ...}
```

**Protocol conformance** (`conformance_test.go`) — `conformance.RunConformance`
drives the adapter against the deterministic fake server in
`internal/lsp/lsptest`, covering lifecycle, document queries and the
diagnostics round-trip in the mode your server actually uses:

```go
func TestConformance_PushBaseline(t *testing.T) {
	conformance.RunConformance(t,
		func(c jsonrpc.Caller) lsp.Client { return rust.New(c) },
		rust.DefaultInitParams, rustConformanceScenario(t))
}
```

Build the `lsptest.Scenario` around a fixture written to disk with
`conformance.WriteFixture` — a lazy-open adapter reads the document itself, so an
in-memory-only URI fails on a read error before reaching the contract under test.

**Error contract** (`errorcontract_test.go`) — `conformance.RunErrorContract`
drives every `lsp.Client` method against an always-failing transport and asserts
the exact `"<server> <label>: <cause>"` string plus `errors.Is`. These strings
are what plumb surfaces to agents and nothing else pins them:

```go
func TestErrorContract(t *testing.T) {
	conformance.RunErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return swift.New(c) },
		swift.DefaultInitParams, "sourcekit-lsp", "main.swift", "print(\"hello\")\n")
}
```

A **lazy-open** adapter adds one more, pinning the `open <uri>` label the
request-label pass deliberately cannot reach:

```go
func TestLazyOpenErrorContract(t *testing.T) {
	conformance.RunLazyOpenErrorContract(t,
		func(c jsonrpc.Caller) lsp.Client { return swift.New(c) },
		"sourcekit-lsp", "absent.swift")
}
```

Call it from the lazy-open adapters and nowhere else: the per-adapter call is
what makes an adapter's participation visible at its own call site.

**Two shared guard tables also need a row.** Each hard-gates on its own row
count, so a partial edit — the row without the gate bump, or the bump without
the row — fails with a directive message. Skipping a table entirely does NOT
fail on its own: a row-count gate cannot see a package it was never told
about. The row is not optional paperwork — it is the only thing pinning your
adapter's optional capability surface (and, for lazy-open adapters, its
didOpen matrix) against future drift:

- `internal/lsp/conformance/optional_interfaces_test.go` — add a case to
  `TestAdapters_OptionalInterfaceSurface` stating which optional pull surfaces
  the adapter exposes, and bump the gate (`if want := 9; len(cases) != want`,
  ~line 90). Every adapter goes here, whether or not it has any capability.
- `internal/lsp/conformance/lazyopen_guard_test.go` — **lazy-open adapters
  only**: add a row to `lazyOpenAdapters` with the adapter's `languageId` and
  its fixture document, state its expected `didOpen` count in every row of
  `lazyOpenMethods`, and bump the gate (`if want := 3; len(adapters) != want`,
  ~line 86).

### 6. Add integration tests

Put them in `integration_test.go`, gated with `//go:build integration` so they
are excluded from the default `go test ./...` run:

```go
//go:build integration

package rust_test
```

The test should:

1. Check the binary is on PATH and usable; skip if not. Prefer probing
   `--version` over a bare `exec.LookPath` — `requireRustAnalyzer` in
   `internal/lsp/adapters/rust/integration_test.go` explains why (a rustup shim
   is on PATH even when the component is absent).
2. Spawn the binary via `exec.Command` and pipe stdin/stdout into
   `jsonrpc.NewConn`.
3. Wrap the conn in the adapter, `Initialize` + `Initialized`.
4. Assert `DocumentSymbols` against a file in the language's fixture. Fixtures
   live at the **repo root** — `testdata/<lang>-fixture/`, not a package-local
   `testdata/`; there is none anywhere under `internal/lsp/adapters/`. Reach it
   with the per-package `repoRoot(t)` helper, which walks up to `go.mod`:
   `filepath.Join(repoRoot(t), "testdata", "<lang>-fixture")` — see
   `internal/lsp/adapters/rust/integration_test.go`. Copy the fixture into a
   temp workspace first if the test mutates it, so a run cannot dirty
   `testdata/`.
5. Assert the `DidChangeWatchedFiles` + `DidOpen` → `publishDiagnostics`
   round-trip. **This is the promotion gate** (see the rule in
   `.claude/skills/add-lsp-adapter/SKILL.md`).

```sh
go test -tags integration ./internal/lsp/adapters/<name>/...
```

### 7. Register the adapter

Four places, all mechanical — on top of the two shared guard tables in step 5:

- `internal/langsupport/langsupport.go` — add a `Language` row with the
  extensions and the `LSPAdapter` binary name. This is the single source of
  truth for extension → language routing (`langsupport.ByPath`).
- `internal/cli/pool_adapters.go` — add a case to `newAdapter` (constructor) and
  to `adapterInitParams` (`DefaultInitParams`).
- `internal/cli/conn_attach_language.go` — add a case to `adapterForLanguage`
  for the display name.
- `internal/arch/layers.go` — add the package to the transport layer
  (`LayerTransport`, alongside the other nine adapter entries).
  `TestEveryPackageHasALayer` fails until you do, and the layering violations
  cascade into `TestFoundationIsSelfContained`.

If the tree-sitter language name differs from the config LSP key, fold it in
`normaliseLangName` (`internal/cli/pool_detect.go`) — that is how the
`tsx`/`jsx`/`javascript` dialects fold onto the `typescript` adapter.

> **New language vs new server.** The list above assumes the *language* is new
> to the tree. A new `Language` row also trips two more gates:
> `TestLanguageAndClientSourceCountsPinned`
> (`internal/cli/doc_counts_test.go`) — bump `wantLanguages` and update the
> language counts in `README.md` and `site/index.html` per that test's
> directive — and `TestBuildExtractorsCoversRegistry`
> (`internal/cli/topology_pool_test.go`), which needs a tree-sitter extractor
> entry (`extractorCtors`) for the language; without a grammar for it in the
> tree that gate is not mechanical. If the language already HAS a `Language`
> row (an existing language gaining its first server), edit that row's
> `LSPAdapter` field instead of adding a duplicate.

> **Primary vs secondary.** A workspace root may bind several language servers at
> once (e.g. Go + HTML). Routing keys the pool by `(root, language)` and sends
> each file to the server that owns its extension. An adapter needs nothing extra
> to work as a *secondary* in a root whose primary is another language: it starts
> lazily the first time a file of its language is touched.

### 8. Add the config defaults

Add an `LSPConfig` entry to `internal/config/config_defaults.go`:

```go
"rust": {
	Command:     "rust-analyzer",
	Args:        []string{},
	RootMarkers: []string{"Cargo.toml"},
	Enabled:     true,
},
```

Every language is enabled by default and activates automatically when its server
binary is on PATH; `enabled = false` is the knob a user reaches for to exclude
one. Use `WeakRootMarkers` for promiscuous markers that must not identify the
language of an *ancestor* directory (`package.json`, `index.html`).

### 9. Document it

Add a row to the adapter reference below and to the validation table in
`AGENTS.md` ("## Adapter validation status"), describing any server-specific
behaviour: workspace model, sync requirements, cold-start cost, and any
per-document quirk that forced a shadowed method.

---

## Adapter reference

### gopls (Go)

- **Binary**: `gopls` — install with `go install golang.org/x/tools/gopls@latest`
- **Status**: validated — integration tests in `internal/lsp/adapters/gopls/`
- **Workspace model**: requires `rootUri` pointing to the module root (the
  directory containing `go.mod`); `go.work` is also a strong root marker, since
  it mounts a multi-module workspace whose modules may live in subdirectories.
- **Init options**: `analyses` (`unusedresult`, `unusedparams`) on by default,
  `staticcheck` off; `pullDiagnostics` is set only under a forced pull mode.
- **Adapter-specific surface**: the only `lsp.PullInitializer` in the tree
  (`EnablePullDiagnostics`), plus `SupportsPullDiagnostics`, `Diagnostic` and
  `WorkspaceDiagnostic`. `Diagnostic` normalises a gopls v0.23 wire quirk (a
  clean full report arrives with no `kind`).
- **Notifications**: emits `textDocument/publishDiagnostics` after each document
  change; plumb's cache invalidator uses this to evict stale entries.

### pyright (Python)

- **Binary**: `pyright-langserver` — install with `npm install -g pyright`
- **Status**: validated — integration tests in `internal/lsp/adapters/pyright/`
- **Root markers**: `pyproject.toml`, `setup.py`, `pyrightconfig.json`.
- **Workspace model**: requires `rootUri` pointing to the Python project root.
  Reads configuration from `pyrightconfig.json` or `pyproject.toml` if present.
- **Init options**: `pythonVersion: "3.12"`, `typeCheckingMode: "basic"`.
- **Sync**: pyright requires full-document sync (`SyncFull`); the base sends the
  complete text in every `DidChange`, so nothing adapter-specific is needed.
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
  `internal/cli/pool_adapters.go argsFor`; no manual configuration is needed.
- **Init options**: `settings.java.home` is populated from `$JAVA_HOME` when
  set; otherwise jdtls uses its own JDK detection. Leave `JAVA_HOME` unset to
  let jdtls discover the JDK (recommended with SDKMAN).
- **Diagnostics**: jdtls publishes `textDocument/publishDiagnostics` for open
  documents. Unlike gopls and pyright, `DidChangeWatchedFiles` alone updates
  the project model but does not reliably trigger immediate diagnostics — a
  subsequent `DidOpen` is needed to request analysis of a specific file.
- **Notifications**: sends `client/registerCapability` during init to register
  file-watcher patterns. The base answers `null` (OK) so jdtls's project model
  stays consistent with on-disk state.
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
  (repo-root `testdata/rust-fixture/`).
- **Root markers**: `Cargo.toml`.
- **Workspace model**: requires `rootUri` pointing at the Cargo workspace root
  (the directory containing `Cargo.toml`). Reads configuration from
  `rust-analyzer.toml` and the Cargo manifest.
- **Init options**: none — rust-analyzer reads its configuration from the
  workspace, so `DefaultInitParams` sends no `initializationOptions`.
- **Adapter-specific surface**: none. This is the minimal adapter: 46 lines,
  every `lsp.Client` method inherited from `base.Adapter`.
- **Notifications**: emits `textDocument/publishDiagnostics`. Syntax errors are
  reported from rust-analyzer's own front end (no `cargo check` needed); the
  slower `cargo check` flycheck supplies type/borrow diagnostics.
- **Cold-start warning**: rust-analyzer loads the sysroot and runs
  `cargo metadata` on first attach. On a large workspace this can take
  **minutes** — the canonical "unavailability" case the structural (tree-sitter)
  layer covers while the server warms. The adapter tolerates a long `initialize`
  by not imposing its own deadline on the handshake.

### sourcekit-lsp (Swift)

- **Binary**: `sourcekit-lsp` — ships with the Swift toolchain (Xcode or a
  standalone swift.org toolchain). On macOS it lives at `/usr/bin/sourcekit-lsp`.
- **Status**: validated — integration tests in `internal/lsp/adapters/swift/`
  (repo-root `testdata/swift-fixture/`, a SwiftPM package).
- **Root markers**: `Package.swift`, `*.xcodeproj`, `*.xcworkspace` (the last two
  glob-matched, for Xcode-app projects with no SwiftPM manifest).
- **Workspace model**: requires `rootUri` pointing at the SwiftPM package root.
  sourcekit-lsp derives per-file compiler arguments from the package build plan;
  for Xcode projects it can use a build-server `compile_commands.json` instead.
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Adapter-specific surface**: **lazy open**. sourcekit-lsp replies
  `-32001 "No language service for <uri> found"` for an unopened document, so
  `DocumentSymbols`, `Definition`, `References`, `Hover`, `PrepareRename`,
  `Rename`, `PrepareCallHierarchy` and `PrepareTypeHierarchy` each
  `open.Ensure` first. `Rename` is included deliberately — a caller may invoke it
  without a preceding `PrepareRename` (zls, by contrast, does not need it).
  `Definition` also decodes the `Location | Location[] | LocationLink[] | null`
  union over `base.CallRaw`.
- **Notifications**: emits `textDocument/publishDiagnostics`. Syntax errors are
  reported from the Swift front end once a file is opened.

### zls (Zig)

- **Binary**: `zls` — install from https://github.com/zigtools/zls (or
  `brew install zls`).
- **Status**: validated (promoted 2026-06-17) — unit-tested with a mocked
  transport, and the integration test (`internal/lsp/adapters/zig/`, repo-root
  `testdata/zig-fixture/`) runs green against a real zls 0.16: document-symbol
  extraction plus the `DidChangeWatchedFiles`+`DidOpen` → `publishDiagnostics`
  round-trip both pass, once plumb advertised the `textDocument.publishDiagnostics`
  client capability (the earlier "zls is pull-only" hypothesis was wrong).
- **Root markers**: `build.zig`, `build.zig.zon`.
- **Workspace model**: requires `rootUri` pointing at the project root (the
  directory containing `build.zig`); zls resolves the build graph from it.
- **Init options**: none by default. Real compile/semantic diagnostics need
  build-on-save, which a user opts into via `[lsp.zig] initialization_options`
  (e.g. `enable_build_on_save`); without it zls surfaces only its ast-check
  syntax diagnostics.
- **Adapter-specific surface**: **lazy open** (an unopened file resolves to
  nothing), a `Definition` union decode, and the optional document-pull surface.
  zls does not implement `prepareCallHierarchy`; `call_hierarchy` for Zig falls
  back to the topology call graph.
- **Notifications**: emits `textDocument/publishDiagnostics`.
- **Maintenance note**: Zig is pre-1.0; `zls` and `tree-sitter-zig` track the
  language version, so this adapter (and the tree-sitter Zig extractor) are an
  ongoing maintenance surface.

### typescript-language-server (TypeScript / JavaScript)

- **Binary**: `typescript-language-server` — install with
  `npm install -g typescript-language-server typescript`.
- **Status**: validated (promoted 2026-06-16) — unit-tested with a mocked
  transport, and the integration test (`internal/lsp/adapters/typescript/`,
  repo-root `testdata/typescript-fixture/`) runs green against a real
  typescript-language-server 5.3.0. It publishes nothing unless the client
  advertises `textDocument.publishDiagnostics` — it does not implement pull
  diagnostics despite the earlier assumption.
- **Root markers**: `tsconfig.json`, `jsconfig.json`; `package.json` is a *weak*
  marker.
- **Serves both languages**: this one server provides the semantic GPS for
  TypeScript *and* JavaScript, so both the `typescript` and `javascript`
  `langsupport` rows name it. A JS-only project (just `package.json`) resolves to
  the `typescript` daemon language and is served fine.
- **Workspace model**: requires `rootUri` at the project root; drives `tsserver`
  underneath.
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Adapter-specific surface**: the optional document-pull surface
  (`SupportsPullDiagnostics` + `Diagnostic`), declared in the adapter's own
  package. In practice the server advertises no `diagnosticProvider` and answers
  `-32601`, so the capability check keeps it on the push path.
- **Notifications**: emits `textDocument/publishDiagnostics`.
- **Package-name note**: the adapter package is `typescript`, which collides by
  name (not import path) with the topology `typescript` *extractor* package; the
  daemon imports the adapter aliased as `tsls` in `internal/cli/pool_adapters.go`.

### kotlin-language-server (Kotlin)

- **Binary**: `kotlin-language-server` — install with
  `brew install kotlin-language-server` or build from
  https://github.com/fwcd/kotlin-language-server (needs a JDK).
- **Status**: experimental — unit-tested with a mocked transport; the
  integration test (`internal/lsp/adapters/kotlin/`, repo-root
  `testdata/kotlin-fixture/`)
  is written and gated `//go:build integration`. The 2026-06-10 real-binary
  retest passed document-symbol extraction but failed the
  `DidChangeWatchedFiles`+`DidOpen` → `publishDiagnostics` round-trip: the
  server needs a real Gradle/Maven project to publish diagnostics at all, not a
  bare temp workspace. The binary is not on the current validation machine, so
  the test skips rather than fails. Promote once it runs green.
- **Root markers**: `settings.gradle.kts`, `build.gradle.kts`. Note the
  `build.gradle.kts` overlap with Java's markers — with both `[lsp.java]` and
  `[lsp.kotlin]` active, the alphabetical detect order makes Java win for a
  shared marker. Both activate automatically when their server is on PATH; if
  both are present, force Kotlin with `session_start({"language": "kotlin"})` or
  set `[lsp.java] enabled = false`.
- **Workspace model**: requires `rootUri` at the project root; resolves the
  classpath from the Gradle/Maven build files (slow on first attach).
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Adapter-specific surface**: none.
- **Notifications**: emits `textDocument/publishDiagnostics`.

### vscode-html-language-server (HTML)

- **Binary**: `vscode-html-language-server` — ships with
  `vscode-langservers-extracted` (`npm install -g vscode-langservers-extracted`).
- **Status**: experimental — unit-tested with a mocked transport; the integration
  test (`internal/lsp/adapters/html/`) skips until the binary is on the
  validation machine.
- **Root markers**: `index.html`, as a *weak* marker only.
- **Workspace model**: the server has **no filesystem access** — it answers only
  from documents the client has opened.
- **Init options**: none — `DefaultInitParams` sends no `initializationOptions`.
- **Adapter-specific surface**: **lazy open** for the per-document queries, plus
  a `DocumentSymbols` union decode (the server answers the legacy
  `SymbolInformation` shape). The rename and hierarchy prepares are *not*
  wrapped: the server answers those from the document it already holds.
- **Coverage**: document symbols, hover, completion, embedded-CSS/JS validation.
  It implements no `workspace/symbol`, call hierarchy, or type hierarchy — those
  forward and return the server's empty/unsupported response, which satisfies
  `lsp.Client` structurally. Its rename is document-local (the server scopes
  renames to matching tag pairs within one document), so a "workspace-wide"
  rename here only ever edits one file. Its
  `documentSymbol` output is noisy and its legacy ranges land at line 1, so HTML
  is flagged `PreferStructuralOutline` and the topology Map remains the better
  HTML outline.

---

## Common pitfalls

**Do not add an exported method to `base.Adapter`.** It is promoted into all nine
adapters and can silently opt them into a capability `internal/cli` resolves
structurally. Declare it in your own package over the `base.Call*` helpers. See
*The promotion hazard* above.

**Do not embed `base.OpenTracker`.** Hold it as a named field. Embedding promotes
`Ensure` and `Refresh` into the adapter's exported surface, the same hazard.

**Do not copy another adapter's ensure-open set.** Which methods need the
document open first differs per server, and getting it wrong shows up as an
empty result or a `-32001`, not as a test failure in the adapter you copied.

**Root markers vs. root URI**: Many language servers expect `rootUri` to be the
workspace root (where the manifest file lives), not an arbitrary subdirectory.
Use `RootMarkers` for markers that identify the language, `WeakRootMarkers` for
promiscuous ones that must not claim an ancestor directory.

**Full vs. incremental sync**: plumb sends full-document changes for every
adapter (`base.Adapter.DidChange`). Check
`ServerCapabilities.TextDocumentSync` after `Initialize` if you need to know what
the server negotiated, but do not send incremental diffs.

**BoolOrOptions capabilities**: LSP capability fields can be `true` (boolean) or
a detailed options object. `protocol.BoolOrOptions` handles both; the field is a
pointer, so check `caps.HoverProvider != nil && caps.HoverProvider.Enabled`.

**Handler registration is not yours to do.** `base.New` installs both the
notification handler and the server-request handler before any LSP method can
run. There is no `SetRequestHandler` call to forget — and no way to add one, as
the handler methods are unexported.

**Test binary path (macOS + Airlock Digital)**: On macOS with Airlock Digital,
`go test` compiles test binaries to a temp directory that is blocked.  Run
tests with `GOTMPDIR=$(pwd)/.testcache go test ./...`.  The Makefile `test`
and `test-race` targets already set this.
