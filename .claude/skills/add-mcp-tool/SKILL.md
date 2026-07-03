---
name: add-mcp-tool
description: Add a new MCP tool to plumb — file layout, Tool interface, WriteDeps, registration, lean-profile decision, tests, and every doc/table that must be updated. Use when creating, renaming, or removing a plumb MCP tool.
---

# Adding an MCP tool to plumb

## 1. File layout and the `Tool` interface

Create `internal/tools/<name>.go` implementing the `Tool` interface from `internal/mcp/tools.go`:
`Name`, `Description`, `InputSchema`, `Execute`.

The `Description` is the authoritative per-tool reference clients read via `tools/list` —
describe what the tool does and its params/quirks. **Workflow steering that spans multiple
tools (routing prose like "prefer this over grep") belongs in a skill, not the
description** — see the boundary rule in `private/docs/internal/skills-strategy.md`
(plumb-ops): "Tool descriptions describe the tool. Skills describe the workflow of
combining tools."

## 2. The thin-orchestrator `Execute()` pattern

Every `Tool.Execute()` must be a thin orchestrator over named, individually-testable steps
(parse/validate → domain logic → presentation). PRs that add a monolithic `Execute()` are
non-conforming. Each inner function stays under gocyclo 15.

```go
func (t *Foo) Execute(ctx context.Context, raw json.RawMessage) (string, error) {
    args, err := parseFooArgs(raw)        // JSON decode + shape validation only
    if err != nil { return "", err }
    if err := args.validate(); err != nil { return "", err }
    res, err := t.run(ctx, args)          // domain logic — no formatting
    if err != nil { return "", err }
    return formatFooResult(res), nil      // presentation — no logic
}
```

## 3. `WriteDeps` for write/edit tools

Write/edit tools take a single `WriteDeps` parameter — don't grow the constructor with
positional params. Adding a new cross-cutting concern (locking, LSP notify, quality
analysis, rate limiting, …) means adding a field to `WriteDeps`, not a new argument
threaded through every write tool's constructor.

## 4. Register the tool, and decide lean vs hidden

Register in `registerAllTools` (`internal/cli/conn_register.go`), called from `handleConn`
(`internal/cli/daemon.go`); write tools use the shared `writeDeps`.

Then decide whether the new tool belongs in the **lean** profile. Read the mutation-lane
rule in `internal/tools/profile.go`: a read-only commodity tool may be hidden freely under
the lean profile, but a mutation tool whose native fallback is unsafe (bypasses plumb's
per-path locks, LSP notify, or the transaction WAL) must stay in `LeanTools`. **`LeanTools`
doubles as the Claude Code `AlwaysLoad` (never-deferred) set** — editing it changes both
which clients see the tool under the lean profile *and* whether Claude Code pins its schema
up front instead of deferring it to `ToolSearch`. Don't add a tool to `LeanTools` casually;
it is a shared, high-stakes list.

## 5. Tests

Add `internal/tools/<name>_test.go`; `WriteDeps{}` is the nil-safe zero-value setup for
write-tool tests.

**Don't forget the integration-gated tool-list parity test.** `TestSmoke_ToolListParity`
(build-tagged `integration`) asserts the live `tools/list` against an expected tool set —
plain `go test ./...` skips it, so it's easy to add a tool, pass CI, and still leave this
test stale. Update its expected tool list when adding, renaming, or removing a tool.

## 6. Docs and tables to update

- `docs/tools.md` — full tool reference.
- `AGENTS.md`'s `## Available tools (N)` heading (bump the count) and its index bullet
  (which category/bullet the new tool belongs under).
- `CHANGELOG.md` — one entry for the change.
