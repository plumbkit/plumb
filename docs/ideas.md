# Plumb Ideas

This file is for product and architecture ideas that are worth discussing before
they become TODOs or implementation plans.

## Real-time Code-quality Feedback for Agents

The idea: after an agent changes code through plumb, plumb should be able to
give the agent useful feedback about what the change did to the project. Not
only LSP diagnostics, but broader code-quality signals from tools such as
`golangci-lint`, `staticcheck`, `ruff`, `eslint`, `tsc`, or similar
project-owned analysers.

The goal is not to turn every edit into a full CI run. The goal is to make the
agent more proactive by giving it the same kind of local feedback a human
developer would get from an editor, test runner, or pre-commit hook.

### Why This Matters

Agents are strongest when the feedback loop is short. If an edit compiles but
quietly introduces a lint issue, unchecked error, unused branch, suspicious
concurrency pattern, or project-style regression, the agent usually only learns
about it later: after a user complains, after `make lint`, or after CI.

Plumb already sits in the right place to shorten that loop:

- It sees the exact files changed by `write_file`, `edit_file`, and
  `transaction_apply`.
- It already talks to the LSP and can return post-write diagnostics.
- The daemon stays alive across tool calls, so it can do work after the write
  response has returned.
- It can cache findings per workspace and surface them in later tool responses,
  `session_start`, status output, or the TUI.

The useful product shape is: "I changed this file, and here is what the project
now thinks about that change."

### The Balance

The hard part is latency. `golangci-lint` and similar tools can be slow,
especially on a cold cache or large repository. If every file edit blocks on a
full analyser run, plumb will feel worse even though the feedback is better.

The default should be background-first:

- A write succeeds.
- Plumb enqueues the touched file or package for analysis.
- The daemon coalesces rapid edits so one save storm does not spawn many lint
  processes.
- If fresh findings are already available, the write response includes them.
- If analysis is still running, the response includes a `quality: pending` marker.
  Results are surfaced automatically on the **next** write response,
  `session_start`, or `quality_report` tool call once analysis completes. The
  agent does not need to poll explicitly; the pending marker is the signal to
  check at a natural breakpoint.

Synchronous analysis should exist, but as an opt-in mode for strict workflows.
It is valuable before commits, in CI-like local checks, or when a user asks the
agent to "make this clean before you stop." It should not be the default inner
loop.

### What I Would Want as a User

If I were using this tool with an agent, I would want the feedback to be
specific, scoped, and hard to ignore without being noisy.

Useful feedback:

- New findings caused by the last change, separated from pre-existing debt.
- LSP diagnostics, lint findings, and test failures labelled differently.
- File, line, rule code, severity, and a short message.
- A clear stale/fresh marker so the agent does not act on old findings.
- A cap on the number of findings shown in a normal tool response.
- A way to ask for the full quality report when needed.
- A summary in `session_start` showing whether the workspace currently has
  pending or failed background checks.

Less useful feedback:

- Dumping hundreds of existing lint findings after a one-line edit.
- Blocking every write on a slow full-repo scan.
- Treating style suggestions as if they are build failures.
- Asking the agent to fix unrelated legacy debt unless the user requested it.
- Surfacing stale findings without making their revision clear.

The highest-value mode is a differential view:

```text
code quality:
  new findings from this edit:
    internal/foo.go:42 errcheck: unchecked error from Close

  existing findings in this file:
    internal/foo.go:18 gocyclo: function is complex

  analysis:
    golangci-lint completed in 1.4s (warm cache); cold start ~20s
```

"Existing findings" means findings in the same file as the edit. Findings in
other files are not surfaced in a normal write response; the agent must call
`quality_report` to see them. "Nearby" as a concept is deliberately bounded to
the same file to prevent noise from spreading across the workspace.

### Finding Structure

Before implementing multi-analyser support, the common finding type that all
analysers emit must be defined. All adapters emit this structure; there are no
per-analyser extensions at the surface layer.

```go
type Finding struct {
    File     string   // absolute path
    Line     int      // 1-based
    Col      int      // 1-based; 0 if the analyser does not report a column
    Rule     string   // analyser-specific rule ID, e.g. "errcheck", "gocyclo"
    Severity Severity // error | warning | info | hint
    Message  string   // short human-readable description
    Analyser string   // "golangci-lint", "ruff", etc.
    Revision string   // hex content hash of File at analysis time
}
```

The `Revision` field drives staleness: before surfacing any cached finding,
compare its `Revision` against the current content hash of the file. A mismatch
means the file changed after analysis ran; discard the finding rather than act
on it.

**Dependency-aware invalidation:** File-level content hashing handles the
directly edited file but not its dependents. If you change an interface in
`internal/foo.go`, lint findings for `internal/bar.go` (which implements it)
are now stale even though `bar.go` was not touched. The first implementation
can accept this limitation — document it clearly — but the finding cache schema
must include a `dependentOf []string` edge so package-level invalidation can be
added without a schema migration.

### Background Daemon Work

Because plumb runs as a daemon, it can do useful preparation before the agent
asks for it:

- Warm analyser caches shortly after a workspace attaches. A cold
  `golangci-lint` run on a medium Go project takes 15–30 seconds; the first
  agent edit should not be the trigger for that cost.
- Track changed files and run low-priority analysis after short debounce
  windows.
- Cache findings by workspace, file, and content revision so stale results are
  discarded automatically.
- Keep the latest quality state available for the TUI and future status views.
- Learn which commands are configured for a workspace by inspecting project
  files such as `.golangci.yml`, `pyproject.toml`, `package.json`, `Makefile`,
  or local plumb config.
- Allow one active quality job per workspace and coalesce new requests into the
  next job to avoid analyser storms.

**golangci-lint and gopls conflict:** `golangci-lint` runs its own internal
type-checker, which can conflict with plumb's live gopls instance over the same
workspace. Symptoms include stale type information or phantom errors in lint
output. Run golangci-lint as a separate process with its own build cache
directory (`GOCACHE`), and schedule it during idle windows rather than
immediately after a write when gopls is most likely actively re-indexing. Never
run them concurrently on the same package.

The daemon should treat this as background assistance, not foreground control.
Active MCP tool calls must stay responsive.

### Suggested Product Surface

Possible places to expose the feature:

- Write responses: append a compact `code quality` section when fresh results
  are available.
- `session_start`: include the latest workspace quality summary and pending job
  status.
- `plumb status`: show whether background quality checks are clean, pending, or
  stale.
- TUI: add a quality panel or badge for the selected session/workspace.
- Future MCP tool: `quality_report` for the full cached report with filtering.
- Config: a `[quality]` section for enabling analysers, selecting background
  versus synchronous mode, timeouts, caps, and severity filters.

Example config shape:

```toml
[quality]
enabled = false
mode = "background"              # background | sync
analysers = ["golangci-lint"]
timeout_ms = 5000                # conservative default; cold golangci-lint can exceed 2000ms
max_findings_per_response = 8
include_existing_findings = "same_file" # none | same_file | all
```

Start disabled or conservative until the ergonomics are proven. The failure mode
of this feature is not only slowness; it is the agent being nudged into fixing
noise instead of the user's actual request.

### First Slice

The first implementation should be Go-only and project-config aware:

1. Detect whether `golangci-lint` is available and whether the workspace has a
   config or Go module.
2. After Go file writes, enqueue a package/file analysis job in the daemon.
3. Parse `golangci-lint` JSON output into the common `Finding` structure.
4. Cache findings by workspace, file, and content hash. Accept that dependent
   packages are not invalidated in the first version; document this as a known
   limitation. Design the cache schema with a `dependentOf` edge so this can be
   added later.
5. Show only fresh, changed-file findings in write responses.
6. Add a status/session summary for pending, clean, failed, and stale quality
   states.

The first slice should avoid pretending to be CI. It should answer: "did the
agent's recent edit introduce anything the local project quality tools can
already see?"

### Open Questions

- Should the first version report only new findings, or also same-file existing
  findings for context? The config supports both; the default needs a decision.
- Should synchronous mode be per-call (a `quality_sync: true` tool param),
  per-session, or only config-driven?
- What is the right timeout for slow tools such as `golangci-lint` versus fast
  tools such as `ruff`? Should each analyser have its own timeout field?
- How should plumb distinguish quality warnings from correctness errors in MCP
  responses so the agent treats them with appropriate urgency?
- Should background findings ever trigger MCP client notifications, or only
  appear on the next user/tool interaction?
- How does this interact with rate limits if analysis itself uses system
  resources shared with active tool calls?
- Should project maintainers be able to define custom quality commands in
  `.plumb/config.toml`?
- When an MCP session ends before background analysis completes, should findings
  be preserved and surfaced in the next session, or discarded? If preserved,
  what is the TTL before they are considered too stale to show?

### My Take

This is one of the most valuable directions for plumb because it matches the
way agents actually improve: they need immediate, structured, local feedback.
The important constraint is that the feedback must be differential and cheap
enough to keep the editing loop fluid.

I would build it as a background quality service owned by the daemon, with
synchronous checks reserved for explicit strict mode. I would start with
`golangci-lint` because this repository already uses it, but I would design the
`Finding` structure around multiple analysers from the beginning so the cache
and surface layer do not need reworking when `ruff` or `tsc` are added.

The feature succeeds if the agent can say, "I changed this, the compiler is
fine, but lint now reports one new issue caused by my edit, so I fixed it before
you had to ask."

### Would I Use It?

Yes, if it behaves like a background signal rather than a gate on every edit.

The version I would want:

- After an edit, tell me quickly if I introduced new findings.
- Keep pre-existing lint debt separate.
- Cache and coalesce work in the daemon, and warm the cache on workspace attach.
- Show "analysis pending" instead of blocking when the tool is slow.
- Let me opt into synchronous mode when I am finishing a change or preparing a
  commit.

If every `edit_file` waited on `golangci-lint run`, it would slow the agent down
and make plumb feel worse. If the daemon runs analysis in the background and
returns fresh, differential findings on a later response, it would make the
agent better. The key question should be: "did this change make things worse?",
not "what is every problem in the repository?"

## Workspace Confinement (Programmatic Sandboxing)

The idea: prevent AI agents from accessing files outside the detected project
root by enforcing path boundaries at the application layer. This serves as a
lightweight, high-performance alternative to OS-level containers (Docker) or
macOS App Sandboxing.

### Why This Matters

AI agents have full filesystem access through plumb. While this is powerful, it
creates a large security surface. A hallucinating or misconfigured agent could:
- Read `~/.ssh/id_rsa` or `~/.aws/credentials`.
- Delete `~/Documents`.
- Exfiltrate sensitive data from unrelated projects.

OS-level sandboxing (Docker) addresses this but at a high cost: it breaks LSP
dependency resolution (LSPs need the host's build tools and credentials),
destroys I/O performance on macOS, and complicates the single-daemon shared
architecture. The right solution is to enforce boundaries at the tool call layer,
not the process layer.

### The Solution: `restrict_to_workspace`

When a session is attached to a workspace (e.g., `/Users/me/projects/plumb`),
plumb enforces that every file-based tool call targets a path within that root.

#### Key Mechanisms

1. **Strict Path Resolution:** Every incoming path must be cleaned with
   `filepath.Clean` and resolved to an absolute path before any boundary check.
   Relative paths are resolved against the workspace root.

2. **Prefix Enforcement:** Reject any operation where the resolved target path
   does not fall within the workspace root. Do not use
   `strings.HasPrefix(target, workspaceRoot)` alone — this passes for
   `/projects/plumb-extra` against a root of `/projects/plumb`. Instead, use
   `filepath.Rel(workspaceRoot, target)` and reject if the result begins with
   `..` or equals `..`.

3. **Symlink Safety:** Symlinks that resolve outside the workspace must be
   rejected. However, resolving via `filepath.EvalSymlinks` followed by a prefix
   check is not atomic — a symlink can be retargeted between the check and the
   actual `open()` call (TOCTOU race). For the first version, accept this
   limitation and document it clearly. A fully hardened implementation requires
   `O_NOFOLLOW` and manual path walking, which introduces platform-specific
   complexity; treat that as a follow-up hardening item, not a blocker for the
   initial feature.

4. **Opt-in Configuration:** Off by default; enabled per-project or globally
   via config.

#### Scope: File Tools Only

Confinement applies to the file-based tools: `read_file`, `write_file`,
`edit_file`, `delete_file`, `rename_file`, `transaction_apply`,
`list_directory`, `list_files`, `find_files`, `search_in_files`.

LSP-derived results (`get_definition`, `find_references`, `call_hierarchy`,
etc.) are **not** confined. The language server legitimately navigates to stdlib
sources, `$GOPATH/pkg/mod/...`, and other dependencies outside the workspace.
Blocking those results would break core navigation. Confinement means the agent
cannot read or write arbitrary files via file tools; it does not prevent the LSP
from reporting symbol locations in external paths.

#### Edge Cases

**Auto-attach sessions:** When `auto_attach = true` resolves the workspace root
to `$HOME` or a broad ancestor directory, confining to that root is meaningless.
The feature should refuse to activate — or emit an explicit warning — unless the
session is attached to a real `.plumb/` marker workspace, not a synthetic root.

**Multi-workspace pool:** The daemon manages a pool of workspaces. If the active
session is attached to `/projects/foo`, a file tool targeting `/projects/bar`
must be blocked even though the daemon manages that workspace too. Confinement
is per-session, not per-daemon.

**Missing `refuse_home_roots`:** The config schema documents a
`[walk].refuse_home_roots` setting as a partial predecessor to this feature.
Before treating it as a foundation, audit the actual tool implementations
(`list_files.go`, `find_files.go`, `search_in_files.go`) to verify the check
exists in code, not only in config intent. If it is missing, workspace
confinement replaces and supersedes it.

### Suggested Configuration

The confinement toggle belongs in `[workspace]`, not `[edits]`, because it
applies to reads as well as writes:

```toml
[workspace]
restrict_to_workspace = true    # Rejects all file I/O outside the workspace root.
                                # Has no effect on auto_attach synthetic roots (warns instead).
allow_external_reads = false    # When true, permits reads outside the root but
                                # not writes. Useful for shared config files
                                # (e.g. a global .golangci.yml or shared schema)
                                # that live outside any single project root.
```

`allow_external_reads` exists because strict write confinement with unrestricted
reads covers the most important threat (destructive or exfiltrating writes) while
still allowing common patterns like reading a global config file or a vendored
schema from a path the organisation standardises outside of individual project
roots.

### Integration Testing Requirements

A dedicated test suite must attempt to escape the workspace boundary and verify
each attempt is rejected:

- `../` traversal: `/projects/plumb/../sensitive`
- Absolute paths outside the root
- Symlinks pointing outside the root
- Paths that pass `strings.HasPrefix` but fail `filepath.Rel` (the
  `/projects/plumb-extra` case)
- Auto-attach session with root at `$HOME` — confinement should warn or refuse
  to activate

### Recommendation

This is a high-impact, low-cost feature. It provides the security guarantees
users expect from a sandbox without the performance and dependency cost of
containers. It should be the primary security model for plumb, and it is a
better investment than OS-level isolation for this architecture.
