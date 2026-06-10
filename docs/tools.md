# Tools — MCP API Reference

Plumb exposes **48** structured tools to AI assistants. Every write tool is
concurrency-safe, atomic, and notifies the language server via
`workspace/didChangeWatchedFiles`.

This page documents each tool's purpose and inputs. For day-to-day workflow,
see [Getting Started](getting-started.md); for the bigger picture, see
[Architecture](architecture.md).

## Client capabilities and fallback behaviour

| Client | Native filesystem | Native shell/git | Notes |
|---|---|---|---|
| Claude Desktop | None | None | Plumb is the **only** interface — no fallback tools exist |
| Claude Code | `Read` / `Edit` / `Write` | `Bash` | Plumb adds LSP-semantic tools with no native equivalent |
| Codex | Shell (`shell` tool) | Yes | Plumb adds an LSP-semantic layer and concurrency-safe writes |
| Gemini CLI | Filesystem tools | Yes | Plumb adds an LSP-semantic layer and concurrency-safe writes |

## Conventions

These apply across many tools:

- **Paths and URIs.** Every tool accepts an absolute path *or* a `file://` URI
  for its file argument — the filesystem tools (`file_path` / `path`) and the
  LSP query/edit tools (`uri`) alike. Filesystem tools additionally accept a
  workspace-relative path, resolved against the session's workspace root; the
  `uri` tools need an absolute path or `file://` URI (the language server
  requires an absolute URI).
- **Positions are zero-based.** LSP query/edit tools that take a `line` and
  `character` use zero-based numbering (matching the LSP spec). Output line
  numbers are printed one-based.
- **Position or name.** `get_definition`, `find_references`, and `read_symbol`
  accept either a position (`uri` + `line` + `character`) or a symbol name
  (`symbol_name` / `name`, plain or dotted `ReceiverType.MethodName`).
- **`dry_run`.** The LSP semantic-edit tools (`rename_symbol`,
  `replace_symbol_body`, `insert_*`, `safe_delete_symbol`) default to
  `dry_run: true` — they preview the change. Pass `dry_run: false` to apply.
- **`dirty_ok`.** Filesystem write tools refuse to touch a file with
  uncommitted git changes unless you pass `dirty_ok: true`.
- **`expected_mtime` / `expected_sha`.** `read_file` and `read_symbol` emit a
  header line — `# plumb-read mtime=<RFC3339Nano> sha256=<hash> indent=<…>` —
  whose `mtime`/`sha256` you can pass back to **`edit_file` *or* `write_file`**
  for optimistic concurrency checks (the write is refused if the file changed
  since you read it).
- **Automatic staleness guard.** Even without `expected_mtime`, if this session
  read a file and it then changed on disk before your write, `write_file`
  **refuses** (pass `overwrite_changed: true` to override) and `edit_file`
  **warns** but still applies. A file the session never read — including a
  brand-new one — is never flagged, and a write you yourself just made is not a
  change, so consecutive edits to your own file are never flagged.
- **Line-number gutter.** `read_file` and `read_symbol` prefix every content
  line with its 1-based file line number and a tab (`cat -n` style), so range
  math is exact. The gutter is **display-only** — strip the leading `<n>\t`
  before using a line as an `edit_file` or `find_replace` `old_string`. (If you
  forget, `edit_file` strips an unambiguous pasted gutter for you and says so.)
- **`# plumb-note` / `# plumb-warn` lines** are out-of-band annotations plumb
  adds to a tool response (large-file `file_outline` nudges, staleness warnings,
  a one-shot "daemon reconnected" note after a transparent reconnect). They are
  informational — never part of file content.

---

## Session

### `session_start`
Bootstrap tool — **call first in every session.** Returns workspace path,
language, git branch, the first 200 lines of `.plumb/context.md`, memory
names/descriptions, top-5 tool usage, 5 recently-modified files, 3 recent
commits, the live git tool policy (whether commits/destructive/push are
enabled), and active diagnostics. Idempotent.
**Inputs:** `workspace` (string, optional — defaults to the daemon's resolved
workspace, then a cwd walk).

### `daemon_info`
Current session name and ID, daemon version, start time, and uptime; live config-store state (generation, last reload time, whether a restart is needed); and this session's tool-call count plus its slowest calls (per-call durations from recorded stats).
**Inputs:** none.

### `rename_session`
Rename the current MCP session. **Inputs:** `name` (string — letters, digits,
and `-` only; user-provided case is preserved; max 25 chars).

### `workspace_sessions`
Same-workspace peer awareness: lists active sessions on this workspace and recent
mutating operations. Useful before editing a file a concurrent agent may have
touched.

**Inputs:** `recent_limit` (integer, optional, 1–50, default 10) — max
recent-write entries to return.

**Output sections:**
- `you` — this session's name.
- `active_sessions` — sessions currently connected to this workspace with their
  client identity and idle status. A single session with `is_self=true` means
  you are the only agent here — your view of the workspace is authoritative.
- `recent_writes` — the last N write/edit/rename/git/… operations by any
  session on this workspace, showing the session name, tool, relative file path,
  and age. If a file you are about to edit appears here, re-read it first.

Read-only. Workspace-boundary-guarded (no other workspace's session data is
ever exposed). Backed by `internal/session` (session files) and
`stats.RecentWritesByWorkspace` (the `tool_calls` stats table). Both data
sources are read under a 500 ms hard timeout so the tool never blocks the MCP
response.

---

## LSP queries

### `find_symbol`
Search symbols by name within a **single document** (case-insensitive
substring). **Inputs:** `query` (string, required), `uri` (string, required).
When the language server errors or times out and `[topology]` is enabled, falls
back to the topology index, returning approximate results annotated
`source=topology, mode=indexed-approximate`.

### `workspace_symbols`
Search symbols by name across the **entire workspace** via the LSP index;
stdlib/dependency hits are filtered out. Prefer over text search for name
lookups. **Inputs:** `query` (string, required). Falls back to the topology
index (annotated `source=topology, mode=indexed-approximate`) when the LSP
errors or times out and `[topology]` is enabled.

### `get_definition`
Source location where a symbol is defined. **Inputs:** `uri` (required), and
either `line` + `character` or `symbol_name`.

### `explain_symbol`
Hover documentation and type information for a symbol. **Inputs:** `uri`
(required) plus a position (`line` + `character`).

### `list_symbols`
Full symbol outline of a file — names, kinds, line ranges, children.
**Inputs:** `uri` (required), `include_signatures` (bool — appends each
function/method/constructor's declaration line). Falls back to the topology
index (annotated `source=topology, mode=indexed-approximate`) when the LSP
errors or times out and `[topology]` is enabled.

### `file_outline`
A token-cheap skeleton of a file: every function, type, method, class, and
constant rendered as its **signature line with the body collapsed**, nested by
containment, with byte-precise 1-based line ranges — a 2000-line file's shape in
a few hundred tokens, so you can decide what to read without reading it.
**Inputs:** `uri` (required), `include_docs` (bool, default true — prepends the
first line of each symbol's leading doc comment). Symbols come from the language
server (`documentSymbol`) when one answers; when the server is cold or does not
cover the file it falls back to the **tree-sitter topology index** (so the
outline still works for files no warm LSP serves), and the output is annotated
`source=lsp` or `source=topology`. Multi-line signatures are joined; the body
opener (`{`) and everything after it is stripped. Shares the documentSymbol
cache with `list_symbols`. Distinct from `list_symbols`, which lists names/kinds/
ranges; `file_outline` shows the actual signature of every symbol as a skeleton.

### `find_references`
All usages of a symbol across the workspace, each with its source line.
**Inputs:** `uri` (required), either `line` + `character` or `symbol_name`,
`include_declaration` (bool, default true).

### `call_hierarchy`
Incoming and outgoing calls for a function. **Inputs:** `uri` (required) plus a
position (`line` + `character`).

### `type_hierarchy`
Supertypes and subtypes of a class or interface. **Inputs:** `uri` (required)
plus a position (`line` + `character`).

### `diagnostics`
LSP errors, warnings, and hints. **Inputs:** `uris` (array of `file://` URIs —
omit or pass `[]` for all files with issues; one URI for a single file; many
URIs to batch). A single call replaces multiple per-file calls.

---

## LSP semantic edits

All default to `dry_run: true`.

### `rename_symbol`
Workspace-wide rename via LSP — scope- and type-aware, updates every reference.
**Inputs:** `uri`, `line`, `character`, `new_name` (all required), `dry_run`
(default true). Provide the position of the identifier to rename.

### `replace_symbol_body`
Replace a symbol's entire declaration. **Inputs:** `uri`, `name_path`,
`content` (required), `include_doc_comment` (bool), `dry_run`.

### `insert_before_symbol`
Insert text immediately before a symbol's declaration. **Inputs:** `uri`,
`name_path`, `content` (required), `include_doc_comment` (bool), `dry_run`.

### `insert_after_symbol`
Insert text immediately after a symbol's declaration. **Inputs:** `uri`,
`name_path`, `content` (required), `dry_run`.

### `safe_delete_symbol`
Delete a symbol only if it has no external references (reports them and refuses
otherwise). **Inputs:** `uri`, `name_path` (required), `include_doc_comment`
(bool), `dry_run`.

> `name_path` is a slash-separated symbol path within the file, e.g.
> `"ClassName/methodName"` or just `"funcName"` for a top-level symbol. This is
> distinct from `rename_symbol`, which takes a cursor position.

---

## Filesystem reads

### `read_file`
Read a file's text. **Inputs:** `file_path` (required), plus an optional line
window — either plumb's `start_line` + `end_line` (1-based, inclusive) or Claude
Code's native `offset` (first line) + `limit` (line count). `start_line` and
`offset` are synonyms; `limit` and `end_line` are mutually exclusive. Binary
files rejected; output capped at 200 KiB. Emits the `# plumb-read …` header.
Each content line carries a display-only 1-based line-number gutter (`<n>\t`,
`cat -n` style) — strip it before reusing a line as an edit `old_string`.

### `read_symbol`
Read the source body of a named symbol in one call (LSP `documentSymbol` +
file read). **Inputs:** `path` (required), `name` (required — plain or dotted
`ReceiverType.MethodName`). Returns all matches when ambiguous. Body lines
carry the same display-only line-number gutter as `read_file`.

### `read_multiple_files`
Read up to 20 files in parallel; per-file errors reported inline. **Inputs:**
`paths` (array, 1–20, required).

### `list_directory`
Immediate children of a directory (`[FILE]`/`[DIR]`, sizes, mtimes) —
non-recursive. **Inputs:** `path` (required), `pattern` (glob),
`include_hidden` (bool), `sort_by` (`name` | `size` | `modified`).

### `list_files`
Recursive file listing relative to a root. **Inputs:** `root`, `pattern`
(glob), `max_depth` (default 8), `include_hidden`. Honours `.gitignore` and
skips `.git`, `vendor`, `node_modules`, …

### `find_files`
Glob/regex file or directory finder. **Inputs:** `pattern` (required), `path`,
`type` (`file` | `dir` | `any`, default `file`), `extension`, `max_depth`,
`max_results` (default 500), `include_hidden`, `use_regex`.

### `search_in_files`
ripgrep-style content search; smart-case; honours `.gitignore`. **Inputs:**
`pattern` (required, regex), `path`, `glob`, `exclude` (array of globs),
`case_sensitive`, `context_lines` (0–10), `max_results` (default 200),
`include_hidden`, `max_file_bytes`, `include_enclosing_symbol` (bool —
annotates each hit with the deepest enclosing LSP symbol; requires LSP).

---

## Filesystem writes

All hold per-path locks, write atomically (`tmpdir` → rename), notify the LSP,
invalidate the symbol cache, consume one rate-limit slot, and accept
`dirty_ok` (default false).

### `write_file`
Create or overwrite a file atomically; post-write diagnostics appended.
`expected_mtime` / `expected_sha` (from a prior `read_file` header) reject the
write if the file changed since you read it — the same optimistic-concurrency
guard `edit_file` has, so a whole-file overwrite never silently clobbers a
concurrent change. Even without those, a write is **refused** when this session
read the file and it then changed on disk (a peer or human edited under you);
pass `overwrite_changed: true` to override. A never-read / new file is never
flagged.
**Inputs:** `file_path`, `content` (required), `expected_mtime` / `expected_sha`
(optional concurrency check), `overwrite_changed`, `dirty_ok`.

### `edit_file`
Targeted `str_replace` with a uniqueness lock and CRLF tolerance. When this
session read the file and it then changed on disk (with no `expected_mtime`
passed), the response carries a `# plumb-warn` note — the edit still applies
(the `old_string` anchor protects the edited region) but surrounding context may
have moved, so re-read before further edits. **Inputs:**
`file_path` (required), `edits` (array of `{old_string, new_string}` — each `old_string` must
appear exactly once), `expected_mtime` / `expected_sha` (optional concurrency
check), `apply_partial` (bool — apply each edit independently), `dirty_ok`.

### `delete_file`
Delete a file (refuses directories). **Inputs:** `file_path` (required), `dirty_ok`.

### `rename_file`
**Primary move tool.** Atomic move/rename. **Inputs:** `from`, `to` (required),
`overwrite` (bool — required to clobber an existing target), `dirty_ok`.

### `copy_file`
Duplicate a file, preserving permissions; cross-device safe. **Inputs:**
`from`, `to` (required), `overwrite`, `dirty_ok`.

### `transaction_apply`
Multi-file atomic edits with rollback (up to 50 ops). Validates everything in
memory, then writes under locks, rolling back on partial failure. **Inputs:**
`operations` (array of `{file_path, edits, expected_mtime?}`), `dirty_ok`.

---

## Memory

Per-workspace markdown notes at `<workspace>/.plumb/memories/`, also exposed as
MCP resources. Names are constrained to `[A-Za-z0-9_-]+`.

| Tool | Purpose | Inputs |
|---|---|---|
| `list_memories` | List all memory names + descriptions. | optional workspace |
| `read_memory` | Read one memory. | memory name |
| `write_memory` | Create or overwrite a memory. | name, content, optional description |
| `delete_memory` | Remove a memory. | memory name |
| `search_memories` | Pattern search across memory bodies. | search pattern |
| `relevant_memories` | Memories relevant to a given file path. | file path |

---

## Topology

A persistent SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`.
Enabled via `[topology] enabled = true`; all tools degrade gracefully when it's
off. See the [Topology guide](topology.md).

### `topology_status`
Index health: file count, entity count, DB size, indexed languages, last sync,
last error. **Inputs:** none.

### `topology_search`
FTS5 ranked symbol/file search. **Inputs:** `query` (required), `kinds`
(filter), `language` (filter), `limit` (default 20), `include_snippets`
(default true), `rerank` (optional). When `[semantics]` is enabled (opt-in; an
embedding API or a self-run OpenAI-compatible endpoint), results are re-ranked
by semantic similarity to the query — the output is annotated
`mode=fts+semantic`. FTS5 stays the authoritative spine: re-rank only re-orders
its candidates and falls back to plain FTS5 (`mode=ranked`) on any error. Pass
`rerank:false` to force the plain ranking, `rerank:true` to force re-rank when
configured. See `docs/internal/semantic-search-design.md`.

### `topology_explore`
BFS neighbourhood around a named symbol. **Inputs:** `name` (required), `depth`
(default 2, max 4), `max_nodes` (default 50, max 200), `max_bytes` (default
30000, max 100000), `include_source` (`none` = name only | `signatures`
(default) | `snippets`/`full` = signature plus docstring), `edge_kinds`.
Budgeting is on **symbol boundaries**: each whole symbol is costed against
`max_bytes` for the chosen source mode and added only if it fits in full, so a
truncated result is always a set of whole, coherent symbols — never a fragment
of a function. `none` omits the signature bytes, so more neighbours fit under
the same budget.

### `topology_impact`
Bidirectional blast-radius: what a symbol depends on and what depends on it.
**Inputs:** `name` (required), `depth` (default 3, max 4), `max_nodes` (default
100, max 200), `max_bytes` (default 30000), `edge_kinds` (default
`["imports","calls"]`).

### `topology_affected`
Given changed files/symbols, return likely affected files and tests. **Inputs:**
`files` (array), `symbols` (array), `max_results` (default 50).

### `topology_routes`
Framework-aware entry-point scanner (Go HTTP handlers, Cobra commands, Python
`@app.route`). Results annotated with confidence — heuristic. **Inputs:**
`framework` (optional: `go` | `python` | `cobra`), `path_prefix` (optional),
`limit` (default 20).

### `structural_query`
Find symbols by **shape**, not name — a curated set of named structural checks
over the topology index, complementing `topology_search` (by name) and
`search_in_files` (by text). No raw tree-sitter S-expression queries are exposed
(an LLM cannot reliably name per-grammar node types); the surface is a small
vetted set. **Inputs:** `query` (required, one of `undocumented-exports` |
`long-functions` | `unused-context`), `language` (optional filter), `min_lines`
(long-functions threshold, default 80), `limit` (default 50). The checks:
- `undocumented-exports` — exported functions/methods/types/constants with no
  doc comment (index-only; "exported" = leading-uppercase, or non-`_`-prefixed
  for Python).
- `long-functions` — functions/methods spanning ≥ `min_lines` lines, longest
  first — decomposition candidates (index-only).
- `unused-context` — Go functions taking a `context.Context` parameter whose
  body never references it (reads the body under the pinned workspace; skips
  grouped/anonymous params rather than false-flag).
Results are `source=topology` (approximate). Returns a clear message when the
index is disabled or empty.

---

## VCS & utilities

### `git`
Unified tiered git tool. **Read** subcommands always run (`status`, `log`,
`diff`, `show`, `blame`, `shortlog`, and branch/tag/stash listing). **Write**
needs `[git] allow_writes` (`add` via `files`, `commit` via `message`, `switch`,
branch/tag create, stash push/pop). **Destructive** (`reset`, `clean`,
`checkout`, `restore`, `rebase`, …) needs `allow_destructive` + `confirm:true`.
**Network** (`push`, `fetch`, `pull`) needs `allow_push` + `confirm:true`.
Force-push to a protected branch and ad-hoc URL pushes are always refused. See
[Configuration → `[git]`](configuration.md#git--tiered-git-tool-gating).

**Ambiguous subcommands are classified by their arguments**, biased towards the
safer-to-deny higher tier:

- `checkout -b`/`-B` (branch creation) is **write**; any other `checkout` is
  **destructive** (it can discard the working tree or detach HEAD). Prefer
  `switch` for safe branch changes.
- `switch` is **write**, but `switch -f`/`--force`/`--discard-changes` is
  **destructive**.
- `restore --staged` (index only) is **write**; `restore --worktree` (or no
  flag) is **destructive**.
- `branch`/`tag`: creating or renaming is **write**, `--delete`/`-d`/`-D` is
  **destructive**, and `--list`/`-a`/`-r`/… is **read**.
- `stash`: bare `git stash`, `push`, `pop`, `apply`, `save`, `create`, `store`
  are **write**; `list`/`show` are **read**; `drop`/`clear` are **destructive**;
  an unknown `stash` sub-subcommand is rejected with the valid list.

`add` and `commit` are **typed, not pass-through**: `commit` only ever runs
`commit -m <message>` (so `--amend`, `--no-verify`, `-F`, and the editor are
unreachable) and `add` only runs `add -- <files>` (no globs, no free-form
paths). Pre-commit hooks always run. Every non-read call consumes one
write-rate-limit slot. Output is capped (200 lines for `log`/`blame`, 100 KiB
overall); `add` and `commit` return a concise summary (staged file count, or
`<short-hash> <subject>`) rather than raw git output.

**Inputs:** `subcommand` (required), `args` (array), `files` (array, for `add`),
`message` (string, for `commit`), `confirm` (bool).

### `git_init`
Initialise a git repository at a path. **Inputs:** path, `init_plumb` (bool —
also creates `.plumb/context.md`).

### `find_replace`
Text/regex find-and-replace across files; **dry-run by default.** **Inputs:**
`pattern`, `replacement` (required), `path`, `glob`, `use_regex`, `dry_run`
(default true), `dirty_ok`, `format_after` (run the workspace formatter),
`case_sensitive`, `max_files`, `max_file_bytes`. Prefer `rename_symbol` for
renaming identifiers — it understands scope and types.

### `file_diff`
Unified diff between two files (system `diff -U`). **Inputs:** two file paths.

### `version`
Plumb version, Go runtime, OS/arch. **Inputs:** none.
