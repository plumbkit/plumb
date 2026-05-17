# Claude Desktop end-to-end smoke test

Manual checklist to confirm plumb is production-ready against real Claude Desktop.
Run this after any significant change to the daemon, MCP server, session handling,
or write-tool pipeline.

**Time:** ~30 minutes.  
**Prerequisites:** Claude Desktop installed, `gopls` on PATH, the plumb binary built
from the current tree.

---

## Setup

```sh
plumb stop
make build
plumb setup claude-desktop
```

Restart Claude Desktop completely (Quit, not just close the window).

Open a terminal and tail the daemon log before starting:

```sh
tail -f ~/Library/Logs/plumb/daemon.log
```

> macOS: `~/Library/Logs/plumb/daemon.log`  
> Linux: `$XDG_STATE_HOME/plumb/daemon.log` (fallback: `~/.local/state/plumb/daemon.log`)

---

## Checklist

### 1 — Daemon starts and workspace resolves

Open Claude Desktop. Open a Go project (e.g. this repo).

**Expected in `daemon.log`:**
```
daemon: session attached  folder=/path/to/project  language=go
```

**Failure mode:** log shows `daemon: cannot determine workspace root`.
Root cause: Claude Desktop's `roots/list` returned "Method not found" and the
cwd-walk fallback didn't find a marker. Fix: check `rootsFn` / `applyProjectConfig`
in `internal/cli/daemon.go`.

---

### 2 — `session_start` / `/orient` works

Type `/orient` in the Claude Desktop chat (or ask Claude to call `session_start`).

**Expected:** Claude responds with a 3–5 sentence summary including:
- project language (Go)
- current git branch
- any active diagnostics

**Failure mode:** Claude says it has no tools, or `session_start` errors.

---

### 3 — `read_file` returns the mtime header

Ask Claude to read a small file, e.g.:

> Read `internal/session/session.go` for me.

**Expected:** Claude's tool response begins with:
```
# plumb-read mtime=2026-...T... sha256=... indent=tabs
```

**Failure mode:** no header, or raw file content with no metadata.

---

### 4 — `edit_file` applies and reports a change

Using the mtime from step 3, ask Claude to make a trivial edit (e.g. add a blank
comment line) and pass the mtime as `expected_mtime`.

**Expected response includes:**
```
applied 1 edit to internal/session/session.go (N bytes)
lines changed: L…
```

Revert the edit afterwards.

**Failure mode:** strict-mode rejection (file wasn't "read in this session") — check
that `read_file` in step 3 registered the mtime in `ReadTracker`.

---

### 5 — Post-write diagnostics fire after a syntax error

Ask Claude to introduce a deliberate syntax error via `edit_file`:

> Add `func broken( {` at the top of `internal/session/session.go`.

**Expected:** within ~300 ms the tool response includes:
```
diagnostics after write:
  session.go:N:M error: syntax error: …
```

This is the load-bearing test for the LSP write pipeline:
`safeWrite` → `didChangeWatchedFiles` → gopls republishes → plumb polls → appended to response.

Revert the edit immediately after.

**Failure mode — no diagnostics section:** either `didChangeWatchedFiles` isn't
reaching gopls, or the 300 ms poll window is too narrow. Check
`[edits].post_write_diagnostics_ms` in `.plumb/config.toml`; raise it to 1000 and retry.
If that fixes it, the default needs tuning for your machine.

---

### 6 — MCP Prompts appear as menu items

Open the slash-command menu in Claude Desktop (type `/`).

**Expected:** `orient`, `whats-broken`, and `recent-changes` are accessible. In Claude Desktop they appear under **Connectors → plumb → Add from plumb**, not in the `/` autocomplete picker.

**Failure mode:** prompt errors on execution or Claude reports no tools available.

---

### 7 — Memory resources accessible

Ask Claude to list memories:

> Call `list_memories` and tell me what you find.

**Expected:** Claude calls the tool and returns the list (empty is fine if no memories exist yet).

> **Note (2026-05-17):** Claude Desktop has no UI panel for MCP resources — the "Add from plumb" submenu only shows prompts. Resources work at the protocol level; verify via the tool instead.

**Failure mode:** `list_memories` errors or Claude reports no such tool.

---

### 8 — Session name appears in TUI

Run `plumb` (TUI) in a separate terminal while the Claude Desktop session is active.

**Expected:** the left panel shows the session with its generated name, e.g.:
```
SWIFT-FALCON  go: …/plumb
```

**Failure mode:** no name prefix — the session file may predate the `Name` field.
Stop and restart the daemon (`plumb stop && plumb serve`) so a fresh session is
registered with a name.

---

## Results

Record the outcome of each step below when you run the checklist.

| Step | Date | Result | Notes |
|---|---|---|---|
| 1 — workspace resolves | 2026-05-17 | ✓ pass | `daemon: session attached root=…/plumb language=go adapter=gopls` |
| 2 — orient works | 2026-05-17 | ✓ pass | `/orient` returned Go, branch main, version 0.5.30, project summary |
| 3 — read_file header | 2026-05-17 | ✓ pass | `# plumb-read mtime=2026-05-17T17:38:47… sha256=f2ff2583… indent=tabs` |
| 4 — edit_file applies | 2026-05-17 | ✓ pass | `applied 1 edit … (5619 bytes) lines changed: L1-218` |
| 5 — post-write diagnostics | 2026-05-17 | ✓ pass | `diagnostics after write: error L1: expected 'package', found 'func'` |
| 6 — prompts in menu | 2026-05-17 | ✓ pass | Orient, Whats-broken, Recent-changes visible under Connectors → plumb → Add from plumb |
| 7 — resources in sidebar | 2026-05-17 | ⚠ n/a | Claude Desktop has no UI panel for MCP resources. Memories are accessible via `list_memories`/`read_memory` tools; `resources/list` works at the protocol level but is not surfaced in the UI |
| 8 — session name in TUI | 2026-05-17 | ✓ pass | `BRAVE-DEER  go: ~/Projects/plumb` shown in left panel |
