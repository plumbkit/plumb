---
name: plumb-diagnose
description: Work out why a plumb call was refused, or why a change looks broken — read the rejection, get compile truth from diagnostics, and check the daemon and index before blaming the code. Use when a tool call fails or a result looks wrong.
---

Almost every plumb failure is one of three things: a guard doing its job, a language server that has not warmed up, or a lane mix-up between plumb and the client's own tools. Work out which before changing any code.

Under a lean tool profile `daemon_info`, `topology_status`, and `file_status` are not advertised; set `[tools] profile = "full"` in `.plumb/config.toml` to see them.

## 1. Read the rejection — it names its own fix

Plumb's refusals are contract errors with a stated remedy, not crashes:

- **"has not been read"** — strict mode wants a `read_file` with a matching mtime before the edit, or the read happened in the *other* lane. Read with plumb, then edit with plumb.
- **"uncommitted changes"** — the dirty-write guard: the file carried changes plumb did not make this session. Commit it, or pass `dirty_ok` deliberately.
- **a throttled write** — the per-session write budget. Wait for the window to slide, or raise `PLUMB_WRITE_RATE_LIMIT`.
- **"workspace boundary violation"** — the path is outside this connection's roots. Re-pin on the right workspace, or have the user add the directory to `[workspace] extra_roots` in the **global** config. Setting it in a project's `.plumb/config.toml` does nothing: those roots widen filesystem access with no per-call confirmation, so a project file that sets them is discarded on merge and a cloned repo cannot grant itself access.
- **a refused re-pin** — this connection's workspace was pinned explicitly and you asked for a different one. Pass `force` when the switch is intended.

**An error blaming an unread or stale file may not be plumb's at all.** A client that does its own read-before-edit tracking keeps a record separate from plumb's, so a plumb `read_file` followed by that client's own edit is refused by the client — wording it as a file that "has not been read yet" or "has been modified since read", or similar. Check which side spoke before hunting for a plumb fault, and stay in one lane.

## 2. Get compile truth, not test truth

Passing tests do not prove the tree compiles, and a clean-looking answer may have come from a server that had nothing to say yet.

    diagnostics(uris=["internal/tools/git.go"])
    edit_file(file_path="/abs/path/internal/tools/git.go", await_diagnostics=true)

`await_diagnostics` blocks briefly for the language server's authoritative post-write pass; only `edit_file` and `write_file` take it. A report labelled **INCOMPLETE** means the server was still indexing, so a clean result then is not evidence — ask again.

## 3. Separate "broken" from "not ready"

    daemon_info()
    topology_status()

- **`daemon_info`** — daemon version, config state, and whether this language's server is ready or still warming. Check it before concluding a tool is broken.
- **`topology_status`** — index freshness and file counts. An empty index answer on a fresh clone or after a large rebase means "not indexed yet", not "absent".

While a server warms, the structural tools still answer from the tree-sitter index and the precise ones cannot. An empty result from a query tool is never proof of absence in that state — say so rather than reporting the absence as a finding.

## 4. Ask who touched the file

    file_status(paths=["internal/tools/git.go"])

Content-free, so it costs almost nothing: per path it reports whether the file is git-dirty, whether it changed since plumb last wrote it, and who wrote it last. That settles "did my edit land?", "is a peer editing this?", and "why is the guard refusing?" in one call, without re-reading anything.

## 5. Re-orient rather than infer

    session_start(workspace="/abs/path/to/project")

If the workspace, language, or git policy looks wrong, re-run `session_start` with an explicit absolute workspace. It re-attaches the language server, topology, and config, and prints the resolved state — cheaper and more reliable than inferring the environment from a sequence of failures.
