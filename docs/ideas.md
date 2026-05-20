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
- If analysis is still running, the response says that quality feedback is
  pending and later calls can surface the result.

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

  existing findings nearby:
    internal/foo.go:18 gocyclo: function is complex

  analysis:
    golangci-lint completed in 1.4s from warm cache
```

That gives the agent enough context to fix what it caused while avoiding a
side quest through unrelated repository debt.

### Background Daemon Work

Because plumb runs as a daemon, it can do useful preparation before the agent
asks for it:

- Warm analyser caches shortly after a workspace attaches.
- Track changed files and run low-priority analysis after short debounce
  windows.
- Cache findings by file content hash or mtime so stale results are discarded.
- Keep the latest quality state available for the TUI and future status views.
- Learn which commands are configured for a workspace by inspecting project
  files such as `.golangci.yml`, `pyproject.toml`, `package.json`, `Makefile`,
  or local plumb config.
- Avoid concurrent analyser storms by allowing one active quality job per
  workspace and coalescing new requests into the next job.

The daemon should treat this as background assistance, not foreground control.
Active MCP tool calls should stay responsive.

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
timeout_ms = 2000
max_findings_per_response = 8
include_existing_findings = "nearby" # none | nearby | all
```

Start disabled or conservative until the ergonomics are proven. The failure mode
of this feature is not only slowness; it is the agent being nudged into fixing
noise instead of the user's actual request.

### First Slice

The first implementation should be Go-only and project-config aware:

1. Detect whether `golangci-lint` is available and whether the workspace has a
   config or Go module.
2. After Go file writes, enqueue a package/file analysis job in the daemon.
3. Parse `golangci-lint` JSON output into a small common finding structure.
4. Cache findings by workspace, file, and content revision.
5. Show only fresh, changed-file findings in write responses.
6. Add a status/session summary for pending, clean, failed, and stale quality
   states.

The first slice should avoid pretending to be CI. It should answer: "did the
agent's recent edit introduce anything the local project quality tools can
already see?"

### Open Questions

- Should the first version report only new findings, or also nearby existing
  findings for context?
- Should synchronous mode be per-call, per-session, or only config-driven?
- What is the right timeout for slow tools such as `golangci-lint` versus fast
  tools such as `ruff`?
- How should plumb distinguish quality warnings from correctness errors in MCP
  responses?
- Should background findings ever trigger client notifications, or only appear
  on the next user/tool interaction?
- How does this interact with rate limits if analysis itself calls tools or
  writes cache files?
- Should project maintainers be able to define custom quality commands in
  `.plumb/config.toml`?

### My Take

This is one of the most valuable directions for plumb because it matches the
way agents actually improve: they need immediate, structured, local feedback.
The important constraint is that the feedback must be differential and cheap
enough to keep the editing loop fluid.

I would build it as a background quality service owned by the daemon, with
synchronous checks reserved for explicit strict mode. I would start with
`golangci-lint` because this repository already uses it, but I would design the
finding model around multiple analysers from the beginning.

The feature succeeds if the agent can say, "I changed this, the compiler is
fine, but lint now reports one new issue caused by my edit, so I fixed it before
you had to ask."

### Would I Use It?

Yes, if it behaves like a background signal rather than a gate on every edit.

The version I would want:

- After an edit, tell me quickly if I introduced new findings.
- Keep pre-existing lint debt separate.
- Cache and coalesce work in the daemon.
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

AI agents have full filesystem access through Plumb. While this is powerful, it
creates a massive security surface. A hallucinating or malicious agent could:
- Read `~/.ssh/id_rsa` or `~/.aws/credentials`.
- Delete `~/Documents`.
- Exfiltrate sensitive data from unrelated projects.

OS-level sandboxing (Docker) solves this but at a high cost: it breaks LSP
dependency resolution (LSPs need your host's build tools/keys), destroys I/O
performance on macOS, and complicates the "one daemon" shared architecture.

### The Solution: `restrict_to_workspace`

Instead of isolating the entire process, Plumb can isolate the **tool calls**.
When a session is attached to a workspace (e.g., `/Users/me/projects/plumb`),
Plumb should be able to enforce that every file-based tool call targets a path
within that boundary.

#### Key Mechanisms

1.  **Strict Path Resolution:** Every incoming path (relative or absolute) must
    be cleaned and resolved against the workspace root.
2.  **Prefix Enforcement:** Reject any operation where
    `!strings.HasPrefix(target, workspaceRoot)`.
3.  **Symlink Safety:** If a tool targets a symlink that points outside the
    workspace, it must be rejected (or resolved and checked against the prefix).
4.  **Opt-in vs. Enforcement:** This should likely be a config toggle
    (`[edits].restrict_to_workspace = true`) that can be set globally or per-project.

### Implementation Nuances & Audit

The current `[walk].refuse_home_roots` setting is a good first step, but it
needs to be audited. It's easy for such checks to exist in "config intent" but
drift in actual implementation.

- **Audit Requirement:** We must ensure that the `refuse_home_roots` check is
  actually enforced in the `list_files`, `find_files`, and `search_in_files`
  implementation, not just in the config loader.
- **Integration Testing:** We need a dedicated test suite that attempts to
  "escape" the workspace using `../` traversal, absolute paths, and malicious
  symlinks to verify the confinement.

### Suggested Configuration

```toml
[edits]
restrict_to_workspace = true   # Rejects all I/O outside the root
allow_external_reads = false   # Optional: allow reading but not writing outside
```

### Recommendation

This is a "high-impact, low-cost" feature. It provides the security guarantees
users expect from a sandbox without the performance and dependency headaches of
containers. It should be the primary security model for Plumb.


