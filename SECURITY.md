# Security Policy

## The trust model — read this first

Plumb gives an LLM (via an MCP client such as Claude Desktop, Claude Code, Codex, or
Gemini CLI) **scoped, structured access to your filesystem and git repository** so it
can navigate and modify code on your behalf. That power is deliberate, and it is
bounded. Understanding the boundaries is part of using Plumb safely.

### What an agent connected to Plumb can do
- Read, search, and analyse files within the **allowed roots** of the attached
  workspace.
- Write, edit, rename, copy, and delete files **inside read-write roots**.
- Run git operations **at the tiers you have enabled**.

### The controls that bound it
Plumb is built so that an agent cannot quietly exceed the access you granted:

- **Workspace boundary (`PathPolicy`).** Every path-bearing tool is checked against a
  per-connection allowlist of roots, each tagged read-only or read-write. A write
  outside a read-write root is refused by construction — not by convention. The
  detected workspace is read-write; `extra_roots` add read-write roots; `read_roots`
  (and, for Go, the module cache + `GOROOT`) add read-only roots. `extra_roots`/
  `read_roots` in a **project** config are ignored (forced back to the global base)
  so a cloned repo cannot widen its own access on attach; to grant one workspace
  extra roots, add them manually in the TUI — recorded out-of-repo in plumb's data
  dir, keyed by the workspace root (the VS Code "workspace trust" model), so only a
  deliberate user action ever widens the boundary.
- **Single-workspace-per-connection.** Once a connection attaches a workspace, paths
  outside its allowed roots raise a `workspace boundary violation`; the connection is
  marked blocked. Switching projects requires an explicit, deliberate re-pin.
- **Tiered git gating.** Git is classified safe-biased into read / write / destructive
  / network tiers. Destructive (`reset`, `clean`, `checkout`, `rebase`, …) and network
  (`push`, `fetch`, `pull`) operations require both the tier to be enabled **and**
  `confirm: true` on the call. Protected branches are never force-pushable. `add` and
  `commit` are typed, not pass-through; a denylist blocks global flags that could
  reconfigure git, and there is no shell.
- **Strict mode** (`[edits].strict`) requires a fresh read (matching mtime) before an
  edit, closing stale-write races.
- **Write rate-limiting** caps writes per session (and per client+workspace), bounding
  runaway loops.
- **Optimistic concurrency.** Writes are atomic (`tmpdir → rename`), symlink-aware,
  per-path locked, and guarded by mtime/sha checks.

You decide how much of this is enabled, at three config layers (global,
`<workspace>/.plumb/config.toml`, environment). Run `plumb config show` to see the
resolved policy with provenance, and `session_start` returns the live git policy for
the current session.

### Recommended posture
- Keep `allow_destructive` and `allow_push` **off** unless you specifically need them.
- Use `extra_roots` sparingly; prefer the narrowest workspace that does the job.
- Review diffs (`show_write_diff = true`) and commit through the `git` tool so changes
  are auditable.

## Supported versions

Security fixes target the latest released version. Until v1.0.0, please run a recent
release before reporting.

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Report privately via GitHub's **"Report a vulnerability"** flow on the repository's
**Security** tab (Security advisories). If that is unavailable to you, email
**hello@getplumb.sh** with the details.

Please include: affected version (`plumb version`), platform and client, a description
of the issue, and a minimal reproduction if possible.

### What to expect
- **Acknowledgement** within 3 business days.
- An initial assessment and severity triage within 7 business days.
- Coordinated disclosure: we will agree a timeline with you, fix in private, release,
  and credit you (if you wish) once users have had a reasonable window to update.

Thank you for helping keep Plumb and its users safe.
