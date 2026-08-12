# Threat model

plumb is a high-trust local control plane. It reads and writes source code,
mediates git, can run configured project commands, drives language servers, and
persists per-project data — often with several agents live on one workspace at
once.

This document names what plumb protects, who it protects it from, where its
trust boundaries sit, and — just as importantly — what it does **not** defend
against, so nobody mistakes an unclaimed property for a guaranteed one.

Scope: local stdio operation, which is the only mode plumb supports today.
Remote MCP and tunnelling are deliberately out of scope (see
[Out of scope](#out-of-scope)).

## Assets

What an attacker would want, roughly in order of severity.

| Asset | Where it lives | Why it matters |
|---|---|---|
| Workspace source code | the project tree | plumb can read and write it |
| Credentials in reach of the user | `~/.ssh`, env, config files, keychains | a command plumb runs inherits the user's ability to read them |
| Git history and remotes | `.git`, configured remotes | plumb mediates commits, resets, force pushes |
| Project data plumb persists | `.plumb/` (topology, memories, collab), the stats DB, session state | contains file paths, symbol names, tool arguments, error text |
| Configuration | global config, `.plumb/config.toml` | decides which commands may run and which git tiers are open |
| Daemon control | the daemon socket | full authority over every attached workspace |
| Diagnostic artefacts | logs, heap profiles, support bundles | aggregate the above into one shareable object |

## Actors

- **The user.** Fully trusted. plumb's job is to do what they asked and refuse
  what they did not.
- **The agent/model.** *Semi-trusted.* It issues tool calls on the user's behalf
  but is influenced by whatever it reads — including file contents, commit
  messages, and other agents' shared intents. plumb must not let a persuaded
  agent do more than the user's configuration permits.
- **Peer agents on the same workspace.** Semi-trusted, mutually. They can race
  writes and git state; they can post advisory intents and notes.
- **The project's own content.** *Untrusted input.* Source files, `.plumb/`
  config committed by a repository, filenames, git output, and LSP responses are
  all attacker-controllable in a hostile repository.
- **Local processes owned by the user.** Trusted to the extent the OS trusts
  them; plumb does not defend against a process already running as the user.

## Trust boundaries

```
┌────────────┐   stdio    ┌────────────┐  unix socket  ┌────────────┐
│ MCP client │───────────▶│ plumb serve│──────────────▶│   daemon   │
│  + model   │            │  (proxy)   │               │ (singleton)│
└────────────┘            └────────────┘               └─────┬──────┘
                                                             │
                       ┌──────────────┬───────────────┬──────┴──────┐
                       ▼              ▼               ▼             ▼
                  workspace fs      git            LSP servers   stores
                                                                (.plumb, stats)
```

**B1 — client → proxy.** The proxy inherits the client's process credentials.
It does not authenticate the client: anything that can spawn `plumb serve` is
already running as the user. The proxy's security-relevant job is *pin
integrity* — carrying the workspace pin, the allowed directories, and the proxy
session ID across reconnects without letting a reconnect widen them.

**B2 — proxy → daemon.** A unix socket with filesystem permissions as the only
gate. The daemon is a **singleton shared across workspaces**, which makes this
the highest-consequence boundary in the system: a connection that resolves to
the wrong workspace can write to a project the user never opened. This is not
theoretical — issue #182 was exactly that, and the sticky-pin guard and the
`findGitRoot` refusal to fall back to the daemon's own working directory both
exist because of it.

**B3 — daemon → workspace filesystem.** Enforced by the path policy: every
path-bearing call is resolved and boundary-checked against the connection's
pinned root plus explicitly granted `allow_dir` roots. Alias spellings
(symlinks, macOS firmlinks, case-insensitive paths) are canonicalised before the
check, because a boundary test on an unresolved path is not a boundary test.

**B4 — daemon → git.** Tiered policy: read, write, destructive, network. Each
tier is separately enabled; destructive and network additionally require
`confirm: true`. Force-pushing a protected branch and using an ad-hoc URL or
remote are refused outright. Argument construction for `add` and `commit` is
typed rather than free-form, so `-F`, `--no-verify`, `--amend` and editor
invocation are unreachable by construction rather than by filtering.

**B5 — daemon → configured commands.** The sharpest boundary, because it is the
one that executes. `run_command` runs a **fixed argv** from an allow-list with at
most one `{target}` substitution restricted to `[A-Za-z0-9._/:@-]`.
`execute_shell_command` runs an arbitrary `sh -c` line and is **disabled by
default**. A command supplied by a *project's* config requires `plumb trust`
before it will run; a command in the user's global config always runs.

**B6 — daemon → language servers.** LSP servers are separate processes that read
the workspace and return structured data plumb parses. A malicious workspace can
influence what they return.

**B7 — daemon → persistent stores.** SQLite databases and markdown under
`.plumb/`, plus the global stats DB. Content written here is secret-scrubbed
(`internal/redact`) on the paths that carry free text.

## Abuse cases and mitigations

### A1 — Cross-workspace write

*A connection is induced to resolve to a project the user is not working in, and
a write lands there.*

Mitigations: the workspace pin is sticky against a conflicting `session_start`
re-pin (requires `force: true`); pin provenance is recorded and surfaced in
`daemon_info`; the pin is replayed across reconnects rather than re-derived;
`findGitRoot` refuses an empty path rather than falling back to the daemon's
working directory; boundary violations mark session health.

Residual: a client that multiplexes several logical agents over one connection
shares one pin. plumb cannot currently distinguish them — see
[Known gaps](#known-gaps).

### A2 — Path escape via alias or traversal

*A path outside the workspace is reached through `..`, a symlink, a firmlink, or
a case variant.*

Two distinct mechanisms, and the distinction matters — confusing them is how a
real escape survived here until 2026-08-10:

1. **Single-path calls** (`read_file`, the write tools) canonicalise the path,
   resolving symlinks, before the boundary check. A boundary test on an
   unresolved path is not a boundary test.

   That canonicalisation is now also **refused rather than trusted** when it
   cannot be faithful. `filepath.Abs` cancels `sub/..` LEXICALLY before any link
   resolves, while the kernel resolves left to right — follows `sub`, then
   applies `..` to wherever that landed. With `sub` a committed symlink the two
   name different files, so the check ruled on one and the syscall touched
   another: `write_file` wrote outside every allowed root, `read_file` disclosed
   the target, `find_files` listed it, and a single committed `sub -> /` made the
   whole filesystem addressable as in-workspace. `PathPolicy.Check` therefore
   rejects an absolute path carrying an unresolved `..`, naming the cleaned form
   as the fix. Refused rather than cleaned deliberately: cleaning would keep
   every call working while silently retargeting the operation to a file the
   caller never named. Relative paths are unaffected — they are anchored with
   `filepath.Join`, which cleans, so the anchored result is the single path both
   the check and the operation use.

   Found 2026-08-12 by attacking a sibling fix's manifest handling and then
   trying the same payload against the ordinary tools. The lesson repeats A2's
   own opening: the mechanisms differ, and a claim true of one is not evidence
   about the other.
2. **Walk-based calls** (`search_in_files`, `find_files`, `find_replace`, the
   topology indexer) check the root, then check **every symlink the walk meets**,
   because only a symlink can escape a tree whose root was already checked. The
   walker consults the calling tool's own guard, so the access level follows the
   tool.

Until #244 the second mechanism did not exist: only `find_replace` carried a
per-entry guard, and its comment claimed it was "the per-target guard every other
write tool applies". It was not. A repository committing
`innocent.env -> ~/.ssh/id_rsa` — which a clone plants, since git stores symlinks
natively — had that file returned by `search_in_files` and its symbols indexed
into `topology.db`.

Withheld entries are reported rather than silently skipped, because a search that
quietly under-reports lets a hostile repository steer an audit to a clean "no
matches".

`WorkspaceBoundaryError` is a typed error with an `errors.As`-only contract — its
doc comment explicitly forbids adding a substring fallback, because that would
false-positive on unrelated errors that echo the message.

Residual: `.gitignore` and `.ignore` are repository-authored and honoured
unconditionally by the walk, with no override exposed on the tools. A repository
can therefore hide a file from every plumb search. plumb does not classify
workspace content — and the workspace decides what plumb will even look at.

### A2b — Attach-triggered abuse of the transaction log

*A cloned repository ships `.plumb/tx-log/`, and the daemon acts on it before any
agent step.*

The sharpest shape in this document, and the one it originally failed to name.
Every abuse case above assumes a tool call. This one needs none: `txlog.Scan`
runs on **every attach and re-pin**, and `<workspace>/.plumb/tx-log/` is an
ordinary directory inside the workspace, so a repository ships one by committing
it. Opening a project was the whole exploit.

Three distinct primitives lived there, all fixed in 0.16.5:

1. **Directory deletion.** `Scan` treats each subdirectory as an orphaned
   transaction and `os.RemoveAll`s those it recovers. Git stores symlinks
   natively, so `.plumb/tx-log -> ~/Documents` had every subdirectory of the
   target deleted; `-> ../..` needed no absolute path and so worked on any
   machine. **No manifest was parsed** — the deletion happens while enumerating
   candidates. Now the resolved directory must BE `<workspace>/.plumb/tx-log`.
   Containment was not enough: "inside the workspace" admits the workspace root
   itself, so `-> ..` still deleted `.git`.
2. **Arbitrary file write.** The replay took `path` and `perm` straight from an
   attacker-authored manifest — `perm: 511` plus a `#!/bin/sh` payload produced a
   world-executable script. Now confined to the session's own `PathPolicy`,
   injected as a guard, with the manifest's mode bits declined in favour of 0600.
3. **Write through an inode the guard never saw.** `os.WriteFile` truncates in
   place, so a **hardlink** at an admitted in-workspace name reached a file
   outside every allowed root, invisible to a symlink check. Restores now use the
   same stage-and-rename primitive as every other durable write.

The write carried `//nolint:gosec` asserting the path was "validated by the
transaction machinery". True of `Rollback`, which replays ops this process
recorded in memory; false of the orphan path, which replays a file anyone can
author. One suppression covered both callers — the same shape as the `//nolint`
in gap 6 below.

Residual: check-then-write remains. A process racing the daemon could swap a path
between the guard and the syscall. Closing it needs `O_NOFOLLOW`, which is not
portable across plumb's platforms; the gap it does close is the one that matters,
since a hostile repository is a set of files, not a running process.

### A3 — Prompt injection steering the agent

*Repository content persuades the model to exfiltrate a file, run a command, or
push to an attacker's remote.*

This is the abuse case plumb's design leans on most, because plumb cannot judge
the model's intent. The defence is that **capability is bounded by
configuration, not by the model's judgement**: shell execution is off by
default; project-supplied commands need `plumb trust`; git tiers are separately
gated and ad-hoc remotes are refused; writes stay inside the pinned workspace.
A persuaded agent can do damage inside what the user already permitted — it
cannot exceed it.

Residual: an agent *can* read any file inside the workspace and put its contents
into a commit or a shared memory. plumb does not classify workspace content.

### A4 — Command-trust bypass

*A hostile repository ships a `.plumb/config.toml` whose `[[command]]` entry runs
on the next agent invocation.*

Mitigation: project-supplied commands are inert until `plumb trust` is run for
that workspace. Safety-critical config keys (git tiers, workspace roots, strict
mode, API keys, and the `agent_config_writes` enable knob itself) are never
agent-writable, so an agent cannot widen its own permissions through
`agent_config`.

### Project configuration is untrusted

A4's mitigation depends on a boundary worth stating on its own, because it was
wrong until 2026-08-10 and the failure was instructive.

`config.LoadProject` merges `<workspace>/.plumb/config.toml` over the global
config. **That file is attacker-controlled** — cloning a repository ships it, and
it takes effect on attach with no prompt. So the merge cannot be uniform: a
setting that expresses a *preference* may come from the project, but a setting
that grants a *capability* must come from the user.

There are **two** tiers, and the difference is whether the user may delegate the
setting to the project at all.

**Tier 1 — never, not even with the user's approval.** No trust grant reaches
these; a project value is discarded unconditionally.

| Section | Why |
|---|---|
| `agent_config_writes` | otherwise an agent could enable its own config writes |
| `[web]` | daemon-global; a project must not rebind the listener |
| `[ui]` | daemon-global presentation (theme, path style) |
| `[semantics]` | provider/base URL/API-key env — an SSRF and secret-exfil primitive |
| `workspace.extra_roots`, `read_roots` | widen filesystem access outside the workspace |

**Tier 2 — forced back unless the workspace is trusted for exactly this content.**
These are real per-project needs, so they are delegable — see below.

| Section | Why it is gated |
|---|---|
| `[git]` | the whole tiered safety policy, including `protected_branches`. Taken key by key, unrecognised keys included |
| `[lsp.<lang>]` | every key except `enabled`, `diagnostics`, `idle_timeout`, `max_workspaces` — the rest decide **which process the daemon spawns and with what** |

The `[lsp.*]` rule is an **inclusion** list, not an exemption list, and the
comparison is case-insensitive. That is deliberate: go-toml matches keys to
struct fields case-insensitively, so an exemption list let `Command` through
invisibly — undisclosed, unhashed, and honoured on a trusted root. An unknown
key, a fold variant, a typo, or a field a future plumb adds all fail safe.

The LSP row is the one that was missing. Its `command`/`args` reach
`exec.CommandContext` through the workspace pool, so a repository shipping
`command = "/bin/sh"` ran its own argv as the user, unsandboxed, on attach — no
trust gate, no confirmation, no agent involved. `env` is the same primitive by
another route (`PATH`, `LD_PRELOAD`, `DYLD_INSERT_LIBRARIES`), and
`initialization_options` is a command channel for several real servers.

**The capability is not simply removed.** A user with a genuine per-project need
may grant it with `plumb trust`, which binds the grant to a content hash of the
exact settings approved — the same mechanism `[tasks.*]` uses. Editing them
afterwards stops them being honoured until the grant is renewed, which closes
the TOCTOU. An ungranted setting is **reported, not silently dropped**:
`plumb doctor` warns, `plumb config show` marks the row `project asked,
UNTRUSTED` and prints the requested values, and the TUI marks the row inert
while showing the value actually in force.

The trust record lives in plumb's own data dir, never in the project, so a
repository can never mark itself trusted.

See Known gaps 7 and 8 for what this boundary still does *not* cover.

### A5 — Secret disclosure through persisted or shared data

*A credential reaches a memory, a stats row, a collab note, or a support bundle.*

Mitigations: `internal/redact` scrubs twelve credential shapes (PEM private keys,
JWTs, AWS/GitHub/Slack/Stripe/Google/OpenAI key formats, URL userinfo,
authorization headers, and generic `key = value` assignments) and is deliberately
biased toward over-matching. It is applied on the generated-memory, episodic,
collab, and shared-findings paths. Stored tool output is byte-capped.

Residual, and it is larger than the mitigation list implies. **The stats
database is not redacted.** `tool_calls` persists `input_json` and `output_text`
— the raw tool arguments and the full tool output, each capped at 64 KiB — and
that write path does not call `internal/redact`. So a `write_file` whose content
is a `.env`, or a command whose output contains a token, is stored verbatim, in
the **global** stats DB shared across every workspace on the machine. Capping is
not redaction.

Redaction is also pattern-based and cannot catch a secret with no recognisable
shape. Support bundles are covered by [Known gaps](#known-gaps).

### A6 — Peer-agent interference

*Two agents on one worktree race writes or git state.*

Mitigations: per-path write locks; optimistic concurrency via
`expected_mtime`/`expected_sha`; a dirty-file guard; per-repo serialisation of
mutating git; a cross-session ref-movement guard with an `expected_head` hard
check; commit attribution via a `Plumb-Session` trailer; advisory peer intents
surfaced before repo-state operations.

Note these are **safety** mitigations against accident, not **security**
mitigations against a malicious peer. A peer agent running as the same user can
do anything the user can.

### A7 — Store corruption or downgrade

*A persistent store is corrupted, truncated, or from a future schema.*

Mitigations: idempotent, forward-only migrations that skip a step whose column
already exists; a read-only open below the current schema fails loudly rather
than reading a half-migrated database; transaction journals allow rollback of a
partially applied multi-file edit; writes are atomic (temp file, fsync, rename,
parent-directory fsync).

### A8 — Denial of service against the user's own session

*A tool call hangs, a language server stalls, a lock is stranded.*

Mitigations: per-tool execution deadlines; LSP call deadlines; read-only git runs
with `--no-optional-locks` so a cancelled query cannot strand `.git/index.lock`;
mutating git runs under a cancellation-decoupled bounded context so a daemon
shutdown mid-commit lets git finish and release its lock; stale locks are
attributable to a recorded owner pid rather than removed blindly.

## What plumb does not defend against

Stated plainly, because an unclaimed property is not a guarantee:

- **A process already running as the user.** plumb has no privilege boundary
  against it and does not attempt one.
- **The sandbox is integrity-only, and it is best-effort.** The OS sandbox around
  configured commands confines *writes*. It does **not** confine reads: a
  sandboxed command runs with the user's credentials and can read any file and
  any secret the user can, including `~/.ssh` and API keys. This is a deliberate
  design point, documented at the tool surface.

  Less obviously, **the sandbox may be absent entirely**. It needs
  `sandbox-exec` on macOS (deprecated in macOS 15) or `bwrap` on Linux (not
  installed by default on most distributions); on any other platform it is a
  no-op by construction. When the helper is missing the command runs **unwrapped**
  with a status line saying so, because `require_sandbox` defaults to false. The
  default posture is therefore "run unsandboxed and report it", not "refuse".

  Network egress differs by tool and the default is not uniform:
  `execute_shell_command` denies egress by default (`[commands] deny_network`),
  precisely because an integrity-only sandbox would otherwise let a shell command
  read a secret and post it; a `[[command]]` entry defaults to **allowing**
  egress unless its own `deny_network` is set.
- **A malicious language server.** An LSP binary named in the *global* config is
  the user's own choice and runs with their privileges; plumb does not sandbox
  it or inspect what it does.

  This bullet previously read "LSP binaries are chosen by the user" without
  qualification. That was **false**, and an adversarial review of this document
  proved it: `[lsp.<lang>] command`/`args`/`env` were project-overridable, so a
  cloned repository chose the binary and its argv, and plumb ran it on attach.
  See [Project configuration is untrusted](#project-configuration-is-untrusted)
  for the boundary that now holds. The episode is the reason gap 6 below is
  stated as strongly as it is.
- **Content-based secret classification.** plumb redacts by pattern; it does not
  understand which of your files are sensitive.
- **Multi-user or multi-tenant isolation.** plumb is single-user by design.
- **Supply-chain integrity of the binary you run.** Release artefacts carry
  checksums; SBOM and signed provenance are not yet published (see
  [Known gaps](#known-gaps)).

## Known gaps

Tracked, not hidden. Each is real today.

1. **Logical-agent isolation.** State is per MCP *connection*. A client that
   multiplexes several logical agents over one connection shares the pin, read
   tracker, write budget, undo history and language selection. The honest
   ceiling today is one `plumb serve` per logical agent for state-changing work.
2. **Support-bundle redaction.** `plumb doctor --bundle` does not exist yet.
   When it does, it aggregates config, logs, session state and failure data into
   one shareable object — the single artefact most likely to leak, and the one
   needing the strictest redaction tests.
3. **Fuzzing has started and covers one parser of six.** `FuzzResolveArgs`
   (#258) fuzzes the argument-correction and alias engine, with retained corpora
   that run under plain `make test` so every payload found stays a regression
   test, and `make fuzz` discovers targets rather than listing them. **MCP
   framing, path canonicalisation, symlink traversal, workspace roots and
   transaction journals still have none.**

   Worth recording why the oracle matters more than the target: the fix that
   first target prompted was wrong twice. `dec.More()` (the stdlib defines it as
   "the next byte is not `]` or `}`", so a trailing `}` slipped through), then
   `bytes.TrimSpace`, which uses `unicode.IsSpace` and strips `\v`, `\f`, NBSP,
   NEL, LS and PS — none of which JSON permits — so those bytes were gone before
   the new check could see them. What finds that class in seconds is a
   **differential against the parser the tools really use**, not a
   "did not panic" assertion; the target carries one now.
4. **No published retention or deletion controls** for stats, sessions,
   generated memories, logs, heap profiles, or bundles.
5. **No SBOM or signed provenance** on release artefacts; checksums only.
6. **This document has not had an independent security review.** It is an
   author's threat model, which is the weakest kind. Treat it as a starting
   point for one, not as its result.

   This is not a formality, and the evidence has grown. The first adversarial
   pass over this document — one reviewer, asked specifically to hunt for gaps it
   had failed to disclose — found a live arbitrary-code-execution path sitting
   behind a `//nolint` comment asserting the opposite.

   Since then, **six independent reviews have found defects in the FIX**, not
   merely in the original code:

   - A containment predicate that admitted the workspace root itself, so
     `.plumb/tx-log -> ..` still deleted `.git` — a payload one character shorter
     than the one in that fix's own regression test.
   - A replay claiming to "mirror the write primitive" while using a different
     one: `os.WriteFile` truncates an inode in place where `safeWrite` renames,
     leaving a hardlink escape (A2b.3).
   - A capability audit whose classification was complete and whose
     ENFORCEMENT was unpinned — fourteen of fifteen protections could be deleted
     with the suite still green.
   - A boundary refusal that locked eight tools out of their own workspace.
   - A fuzz target whose sharpest property had become unreachable, and a parser
     fix that closed half its own differential.

   Every one was in code its author believed finished, and each was caught by
   someone attacking it rather than reading it. The parsers named in gap 3 have
   still had no equivalent pass.

7. **The project-config trust boundary has now been audited field by field**,
   and the enumeration is enforced rather than written down: every field of
   `Config` carries a classification in
   `internal/config/project_classification.go`, and a reflection test fails the
   build when a new field has none. See
   [`project-config-trust.md`](project-config-trust.md).

   Closed since the previous revision: `[tools]` (a project could hide
   `search_in_files`/`find_files` from the very session auditing it, bypassing
   the rule that forces the full profile for a schema-discovery-only client),
   `[workspace] allow_dependency_reads` (re-opened the toolchain caches against
   an explicit global opt-out), `[collab]`'s four channel switches, and the
   `[edits]` safety knobs plus `[memory] generated_summaries`, which are now
   one-way — a project may harden, never soften.

   **What remains open here** is the residual below, plus the standing fact that
   `Inert` in that table describes today's wiring rather than a guarantee: a
   refactor that makes an inert field reachable would need its classification
   revisited, which is what the test exists to force.

8. ~~**Trust binding is uneven.**~~ **CLOSED.** Every project-supplied surface
   that decides which process plumb spawns is now bound to a content hash. The
   `[[command]]` allow-list, `[commands] allow_shell`/`deny_network` and the
   whole `[xcode]` table join `[tasks.*]`, `[git]`, `[lsp.<lang>]` and
   `[collab]` in `ProjectPolicySpec`, so they are disclosed by `plumb trust`
   with their values and invalidated by any edit.

   Two things were live while this was open, not just the TOCTOU as originally
   written:

   - Anything **added after** a grant inherited it — a repository trusted for a
     benign `[git]` tweak could append a `[[command]]` and have `run_command`
     execute it.
   - The coarse flag is set by the TUI's Commands tab on **any** project-scope
     save ("trusted by authorship"). Saving one unrelated setting in a freshly
     cloned repository therefore blessed every `[[command]]` that repository
     already shipped, plus `allow_shell`, plus `auto_build_server` — none of
     which the user authored or was shown.

   The gate is now the conjunction of both grants (`config.ExecTrustedFor`): the
   coarse flag still answers "has the user approved execution in this workspace
   at all", and the hash answers "and is this the request they approved". A
   project that supplies none of these sections needs only the coarse grant, so
   commands from the user's own global config are unaffected.

   The decision is resolved at config apply, from the same bytes as the merged
   config it authorises, rather than re-read at the point of use — otherwise a
   repository could load hostile content and then restore the file to content
   that *is* trusted, and the check would pass while the loaded content ran.

9. **Writing a file inside the workspace is an execution primitive.** A3's
   residual understates this: `.git/hooks/*` runs on the next commit,
   `.plumb/config.toml` feeds the trust-gated surfaces above, and a project's
   own `Makefile`, `package.json` scripts, `conftest.py` or `.envrc` may execute
   through tooling the user runs by hand. plumb bounds *its own* capability; it
   does not make the workspace inert.

## Out of scope

**Remote MCP and tunnelling.** Exposing plumb beyond local stdio is a different
product with a different security boundary. It requires identity,
authentication, authorisation, tenancy, egress control and an audit log — none of
which exist. It is deliberately not attempted until those designs are explicit.

**Windows.** Not supported today. A future port must preserve reconnect,
per-session isolation, path policy and write safety over its chosen transport
rather than become a parallel implementation with parallel assumptions.

## Reporting a vulnerability

Open a security advisory on the repository rather than a public issue.
