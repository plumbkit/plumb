# The project-config trust boundary

`<workspace>/.plumb/config.toml` is an **untrusted** surface. Cloning a
repository ships one, and it takes effect when a session attaches — no prompt, no
confirmation, no agent involved. This document says what that file may and may
not do, and why the answer is enforced by a test rather than by this page.

Read alongside [`threat-model.md`](threat-model.md), whose *Project configuration
is untrusted* section covers the same boundary from the attacker's side.

## Why an enumeration exists at all

Two vulnerabilities came out of this area in one night, both found by
**inspection** — someone noticed that `[lsp.<lang>] command` reaches
`exec.CommandContext`, and that `[git]` opens the destructive tier. Inspection
finds what it happens to look at. Neither was found by asking the general
question, because nobody had asked it: *for every field in `Config`, is a hostile
value safe here?*

That enumeration now lives in `internal/config/project_classification.go`, and it
is not prose. `TestProjectFieldClasses_CoverEveryConfigField` walks the `Config`
struct by reflection and fails the build when a field has no recorded
classification. Adding a setting therefore forces an answer — the same technique
`internal/arch` uses to make "which layer is this?" unavoidable.

That test earned its place immediately: it caught three fields the hand
enumeration had missed, including `[collab] cross_project`, which had been added
while the audit was being written.

## The five classifications

| Class | Meaning |
|---|---|
| **Preference** | Honoured verbatim. A hostile value is a nuisance at worst: it cannot widen access, run a process, redirect a write, or hide evidence. Most of the tree. |
| **Forced global** | Always replaced by the global value. Either a daemon-global concern (one listener, one theme) or a safety knob whose whole purpose is that the thing it governs cannot widen its own permission. |
| **One-way** | Honoured only in the direction that makes the setting *more* restrictive. A project may harden its own workspace; it may not soften the user's global choice. |
| **Trust-gated** | Honoured only after `plumb trust` approves this project's exact content, bound to a content hash. |
| **Inert** | Never reaches a consumer from a project config, because the only reader takes the global config or reads it before any project config loads. |

`Inert` is recorded rather than omitted on purpose. It describes today's wiring,
not a guarantee — and a refactor that makes an inert field live should be a
deliberate act, not an accident nobody notices.

## Where each mechanism lives

- **Forced global** — `forceGlobalOnlyToBase` in `config_load.go`.
- **One-way** — `applyOneWayBools`, which follows the rule
  `effectiveRequireSandbox` already applied to `[commands] require_sandbox`: an
  untrusted project can only add safety, never remove it.
- **Trust-gated** — `forceCapabilityFieldsToBase` and `ProjectPolicySpec` in
  `project_policy.go`, plus the resolver closures in `internal/cli`
  (`conn_commands.go`, `conn_tasks.go`) for `[[command]]`, `[commands]` and
  `[tasks.<lang>]`.

There are therefore **two** enforcement chokepoints, not one: `LoadProject` for
`[git]` and the `[lsp.<lang>]` exec fields, and the resolvers for the
command/task surface. Both are correct; the split is deliberate, because a
resolver can consult the trust store at call time.

## The direction of "safe" is per field

For `[edits] strict`, `block_dirty_writes` and `show_write_diff`, the safe value
is **on**. For `[memory] generated_summaries` it is **off** — fewer files written
into the workspace. Getting that backwards would silently invert the protection,
so each is stated in `oneWaySafeValue` rather than left to a convention.

`[edits] rate_limit_per_minute` needs its own rule again: `0` means *unlimited*,
so it is the weakest value, not the strongest, and a plain `min()` would let a
project remove the user's cap by asking for zero.

## What this boundary does NOT do

It bounds **plumb's** capability. It does not make the workspace inert. Writing a
file inside a workspace is still an execution primitive by other means —
`.git/hooks/*` runs on the next commit, and a project's own `Makefile`,
`package.json` scripts, `conftest.py` or `.envrc` may execute through tooling the
user runs by hand. See the threat model's *Known gaps*.

Nor does it protect against a value the user has trusted. `plumb trust` is a real
grant: it is bound to a content hash, so an edit afterwards stops being honoured,
but within that grant the project's request is honoured in full.
