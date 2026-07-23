# Tools — MCP API Reference

Plumb exposes **60** structured tools to AI assistants. Every write tool is
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
- **Position or name.** `get_definition`, `find_references`, `call_hierarchy`,
  `rename_symbol`, and `read_symbol` accept either a position (`uri` + `line` +
  `character`) or a symbol name (`symbol_name` / `name`, plain or dotted
  `ReceiverType.MethodName`). Prefer names when available; plumb resolves them
  to the identifier's `SelectionRange.Start` and avoids hand-computed positions.
- **`dry_run`.** The LSP semantic-edit tools (`rename_symbol`,
  `replace_symbol_body`, `insert_*`, `safe_delete_symbol`) default to
  `dry_run: true` — they preview the change. Pass `dry_run: false` to apply.
- **`dirty_ok`.** Filesystem and semantic write tools refuse to touch a file
  with uncommitted git changes that plumb did not write this session, unless
  you pass `dirty_ok: true`. Disable the guard entirely with
  `[edits] block_dirty_writes = false` (or `PLUMB_BLOCK_DIRTY_WRITES=0`).
- **`expected_mtime` / `expected_sha`.** `read_file` and `read_symbol` emit a
  header line — `# plumb-read mtime=<RFC3339Nano> sha256=<hash> indent=<…>` —
  whose `mtime`/`sha256` you can pass back to **`edit_file` *or* `write_file`**
  for optimistic concurrency checks (the write is refused if the file changed
  since you read it). **Sole-agent fast path:** for a burst of sequential
  `edit_file`s to one file you can *omit* `expected_mtime` and rely on the
  exactly-once `old_string` match as the safety check — each successful edit
  returns a fresh `mtime`, so threading it forward is needless friction (and a
  stale value you carry over would be rejected). Reach for
  `expected_mtime`/`expected_sha` only when a concurrent writer may touch the
  file between your read and your write.
- **Atomicity on transport failure.** Every write stages to a temp file then
  `rename`s it into place (atomic on POSIX), so if a call dies with a
  transport/connection error (`Connection closed`) the file on disk is either
  fully written or untouched — **never partially written**. Re-read to see
  which side of the rename it landed on rather than assuming corruption.
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
enabled), and active diagnostics. When `[collab] peer_awareness` is on and other
sessions are active on the workspace, it also appends an "Active peers" digest
naming them and the areas (directories/packages) they recently touched. Idempotent.
**Inputs:** `workspace` (string, optional — defaults to the daemon's resolved
workspace, then a cwd walk); `language` (string, optional — force the primary
LSP language when detection cannot infer it); `session_id` (string, optional —
links the plumb session to the caller's own session for name inheritance);
`purpose` (string, optional — a human-readable tag for this session, e.g.
`deploy-fix`; letters, digits, and `-` only, max 32 chars; surfaced in the TUI
session list, `daemon_info`, and `workspace_sessions`. An invalid value is
rejected with a clear error).

### `daemon_info`
Current session name and ID, daemon version, start time, and uptime; the
session's `purpose` tag when set; live config-store state (generation, last
reload time, whether a restart is needed); and this session's tool-call count
plus its slowest calls (per-call durations from recorded stats).
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
  client identity, optional `purpose` tag, and idle status. A single session with
  `is_self=true` means you are the only agent here — your view of the workspace is
  authoritative.
- `recent_writes` — the last N write/edit/rename/git/… operations by any
  session on this workspace, showing the session name, tool, relative file path,
  and age. If a file you are about to edit appears here, re-read it first. When
  `[collab] peer_awareness` is on and the topology index has the file, each entry
  is annotated with its enclosing package/symbol (best-effort, `source=topology`).

Read-only. Workspace-boundary-guarded (no other workspace's session data is
ever exposed). Backed by `internal/session` (session files) and
`stats.RecentWritesByWorkspace` (the `tool_calls` stats table). Both data
sources are read under a 500 ms hard timeout so the tool never blocks the MCP
response.

When `[collab] intents` is on, the output also lists each active session's live
intent (an unverified claim, distinct from the observed `recent_writes`); when
`[collab] mailbox` is on it lists the notes addressed to the caller (pending, not
consumed here — `session_start` delivers them). Neither ever creates `collab.db`.

---

## Cross-agent sharing (`[collab]`)

Three opt-in, advisory tools for concurrent agents on one workspace. Each is gated
by its own `[collab]` flag (default off) and refuses with a clear enable hint
when the flag is off. Everything is **advisory** — nothing here ever blocks a
write — **secret-scrubbed** (`internal/redact`) before storage, **byte-budgeted**
when injected, and **strictly per-workspace**. What an agent *says* is a **claim**,
rendered distinctly from what the daemon *observed* (phase-1 peer awareness).
`share_intent` and `leave_note` rows live in `<workspace>/.plumb/collab.db` (WAL,
auto-gitignored like `topology.db`), created lazily on first use, expiring per
`[collab] intent_ttl_minutes` and pruned on the daemon session-reaper tick;
`share_findings` instead writes a durable generated memory. Delivery is by polling
and hint injection only — plumb cannot push to another agent.

### `share_intent`
Broadcast what you are working on so peers can steer around it (e.g. "refactoring
the rate limiter — avoid `internal/tools/ratelimit*`"). You have at most one live
intent — a new call replaces it — and it is cleared automatically when your
session ends. Peers see it in `workspace_sessions`, and a peer whose write touches
a path matching your `path_globs` gets a bounded advisory hint labelled
`[Peer intent (claim, unverified): …]`. Requires `[collab] intents = true`.

**Inputs:** `body` (string, required — free text); `path_globs` (array of
strings, optional — workspace-relative globs for the area you are working on);
`ttl_minutes` (integer, optional — expiry override; defaults to `[collab]
intent_ttl_minutes`).

### `leave_note`
Leave a short message for a named peer session, or for `next` (whoever attaches to
this workspace next). A minimal mailbox — notes only, no tasks, threads, or
replies. An addressed note is delivered at that peer's `session_start` and listed
in its `workspace_sessions` until it expires; a `next` note is delivered once, to
the first session that attaches, then consumed. Requires `[collab] mailbox = true`.

**Inputs:** `body` (string, required — the message); `to` (string, optional — a
peer session name, or `next` (default) for whoever attaches next).

### `share_findings`
Hand off what you have just learned as a durable, searchable memory *now*, instead
of waiting for the idle episodic summary that fires when your session ends. It
rides plumb's generated-memory pipeline end-to-end: the body is secret-scrubbed,
stamped with your session and the date as provenance (`confidence=generated`),
written under `<workspace>/.plumb/memories/` as `finding-<timestamp>-<session>`,
and FTS-indexed. Peers discover it through the ordinary channels —
`search_memories`, `workspace_search`, `relevant_memories`, memory hint injection,
and the next `session_start`. It is **agent-generated** content: labelled
lower-confidence than a user-written memory and never displacing one in a capped
hint slot, and it counts against the same `[memory] generated_memory_keep`
retention pool as an idle `episodic-*` summary. Rule-based only — you supply the
text; there is no LLM summarisation. Requires `[collab] knowledge_handoff = true`.

**Inputs:** `summary` (string, required — a one- or two-line headline, stored as
the memory body); `description` (string, optional — longer detail appended below
the summary); `paths` (array of strings, optional — workspace-relative globs the
finding is about, stored as frontmatter so `relevant_memories` and hint injection
route it to those files).

---

## LSP queries

### `find_symbol`
Search symbols by name within a **single document** (case-insensitive
substring). **Inputs:** `query` (string, required), `uri` (string, optional but
needed for a real search). Omitting `uri` returns a friendly redirect to
`workspace_symbols` for workspace-wide name search. When the language server
errors or times out and `[topology]` is enabled, falls back to the topology
index, returning approximate results annotated `source=topology,
mode=indexed-approximate`.

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
Incoming and outgoing calls for a function. **Inputs:** `uri` (required), and
either `line` + `character` or `symbol_name`.

### `type_hierarchy`
Supertypes and subtypes of a class or interface. **Inputs:** `uri` (required)
plus a position (`line` + `character`).

### `diagnostics`
LSP errors, warnings, and hints. **Inputs:** `uris` (array of `file://` URIs —
omit or pass `[]` for all files with issues; one URI for a single file; many
URIs to batch). A single call replaces multiple per-file calls.

Mode-aware: on a connection negotiated for `pull`/`hybrid`
(`[lsp.<lang>] diagnostics = "pull"`, see
[Configuration](configuration.md#lsplanguage--language-servers)), the tool
pulls on demand via LSP 3.17 `textDocument/diagnostic` — reusing result IDs
and unchanged reports, folding in related documents, and pulling multiple
URIs at bounded concurrency; a `push`-mode connection keeps the existing
open-and-wait behaviour. A pull that fails never reports a false "No
issues" — it surfaces the error plus the last-known cached diagnostics,
explicitly marked stale or unverified. The no-URI (whole-workspace) query
runs a `workspace/diagnostic` sweep only when the server advertises that
capability; otherwise it returns the cached view plus an honest note that
only already-analysed or already-pulled files are covered — pass `uris` to
check specific files.

---

## LSP semantic edits

All default to `dry_run: true`. When applied, semantic edits use the same
write-tool bookkeeping as filesystem writes: path locks, dirty guards,
`workspace/didChangeWatchedFiles`, cache invalidation, undo capture, topology
refresh, quality hooks, and differential post-write diagnostics.

### `rename_symbol`
Workspace-wide rename via LSP — scope- and type-aware, updates every reference.
**Inputs:** `uri`, `new_name` (required), either `symbol_name` or `line` +
`character`, `dirty_ok`, `dry_run` (default true), `structural_fallback`
(default false). Prefer `symbol_name`; raw positions recover from narrow
"no identifier" misses by snapping once to the enclosing symbol's identifier.
When the language server cannot compute the rename (an error, or an empty edit
set — common with sourcekit-lsp before the build graph resolves), the tool
returns actionable guidance. Pass `structural_fallback=true`
to fall through to a best-effort, identifier-boundary text rename via
`find_replace` (word-boundary match across same-extension files, honouring
`dry_run`) — **not scope-aware**, so review the preview before applying.
The response carries a per-file unified diff of the change — a preview in
dry-run, the applied change otherwise — unless `show_write_diff` is disabled;
diffs are capped at 20 files with an "and N more file(s)" summary. The
structural-fallback path instead surfaces `find_replace`'s own match output.

### `replace_symbol_body`
Replace a symbol's entire declaration. **Inputs:** `uri`, `name_path`,
`content` (required), `include_doc_comment` (bool), `dry_run`, `dirty_ok`.

### `insert_before_symbol`
Insert text immediately before a symbol's declaration. **Inputs:** `uri`,
`name_path`, `content` (required), `include_doc_comment` (bool), `dry_run`,
`dirty_ok`.

### `insert_after_symbol`
Insert text immediately after a symbol's declaration. **Inputs:** `uri`,
`name_path`, `content` (required), `dry_run`, `dirty_ok`.

### `safe_delete_symbol`
Delete a symbol only if it has no external references (reports them and refuses
otherwise). **Inputs:** `uri`, `name_path` (required), `include_doc_comment`
(bool), `dry_run`, `dirty_ok`.

> `name_path` is a slash-separated symbol path within the file, e.g.
> `"ClassName/methodName"` or just `"funcName"` for a top-level symbol.
>
> All four append a unified diff of the change to their response — a preview in
> `dry_run`, the applied change otherwise — gated by `[edits].show_write_diff`
> (default on; same toggle as `edit_file`/`write_file`).

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

**Search-within-file mode.** Pass `pattern` to search the file instead of
windowing: each matching line is returned with its 1-based line number (and
optional context), so an over-cap file stays searchable in one tool. The whole
file is scanned line-by-line regardless of size; only the *output* is bounded.
**Search inputs:** `pattern` (literal text by default; a Go RE2 regex when
`use_regex`), `case_sensitive` (default smart-case — case-insensitive when the
pattern is all lowercase), `context_lines` (0–10, like `rg -C`; disjoint groups
get an `--` separator), `max_matches` (1–2000, default 200; output is truncated
and labelled beyond it). `pattern` may be combined with `start_line`/`end_line`
(or `offset`) to **restrict the search to that line window**, but not with
`limit` (rejected — use `max_matches`). When a `start_line`/`end_line` window is
set, `context_lines` is clipped to that window too — context never spills
match lines from outside the restricted range. A summary line (`# plumb-search:
N matches for …`) precedes the results; a no-match search returns an explicit
message, not an error; an invalid regex returns a clean error. Mirrors
`search_in_files`' literal/smart-case conventions.

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

### `file_status`
Lightweight, read-only "did this file change under me?" probe — no content
read. **Inputs:** `paths` (array, 1–50, required). Per path reports `git_dirty`
(uncommitted vs git HEAD/index — untracked counts as dirty),
`changed_since_plumb_wrote` (on-disk mtime advanced since plumb last wrote it
this session), `last_writer` (`plumb` | `external` | `unknown`), `mtime`, and
`size`. Missing files are reported, not an error. Does **not** satisfy strict
mode's read-before-edit requirement — it is a status probe, not a read.

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

**Anchor-bounded mode (alternative to `edits`).** Instead of an exact
`old_string`, supply `start_anchor` + `end_anchor` (two unique substrings) and a
`new_string` that replaces the span they bound. The two request shapes are
mutually exclusive — provide *either* `edits` *or* the anchor trio, never both.
Each anchor must match **exactly once** (ambiguous/absent → the same clear error
as `old_string`), `end_anchor` must occur after `start_anchor`, and the matcher
mirrors `str_replace`: CRLF-tolerant and forgiving of a pasted display-only
read_file gutter (`<n>\t`). `include_anchors=false` (default) replaces only the
text *between* the anchors, leaving them in place as stable boundaries;
`include_anchors=true` replaces the whole inclusive span (anchors included). An
empty `new_string` deletes the span. Everything downstream — the per-path lock,
`expected_mtime`/`expected_sha` guards, LSP notify, cache invalidation, diff
output, and write-rate budget — is the same write path as `edits`. Ideal for
rewriting a block whose interior changes but whose boundary lines are stable.
**Anchor inputs:** `start_anchor`, `end_anchor`, `new_string`,
`include_anchors` (bool, default false).

### `delete_file`
Delete a file (refuses directories unless `allow_dir` and the directory is empty).
The response reports the line and byte count removed (bytes only for a binary or
oversized file). **Inputs:** `file_path` (required), `dirty_ok`, `allow_dir`.

### `rename_file`
**Primary move tool.** Atomic move/rename. **Inputs:** `from`, `to` (required),
`overwrite` (bool — required to clobber an existing target), `dirty_ok`.

### `copy_file`
Duplicate a file, preserving permissions; cross-device safe. **Inputs:**
`from`, `to` (required), `overwrite`, `dirty_ok`.

### `transaction_apply`
Multi-file atomic edits with rollback (up to 50 ops). Validates everything in
memory, then writes under locks, rolling back on partial failure. The response
lists each file with a per-file unified diff, gated by `[edits].show_write_diff`.
**Inputs:** `operations` (array of `{file_path, edits, expected_mtime?}`), `dirty_ok`.

### `undo_edit`
Revert plumb's most recent write to a file — the safe alternative to a whole-file
`git checkout`/`git restore`, which discards every uncommitted change in the file.
Restores only what the last `edit_file`/`write_file` changed (deleting the file if
that write created it), and **refuses by default** when the file has changed since
plumb wrote it (an external or peer edit), so it never silently clobbers someone
else's work. Single-level per file (a fresh write re-arms it); undo history is
per session and cleared on a workspace switch. Pre-write content over 1 MiB is
not snapshotted, so undo is unavailable for very large files. **Inputs:**
`file_path`, `force` (override the changed-since-write guard).

---

## Memory

Per-workspace markdown notes at `<workspace>/.plumb/memories/`, also exposed as
MCP resources. Names are constrained to `[A-Za-z0-9_-]+`. Markdown files are the
source of truth; `<workspace>/.plumb/memory.db` is only a rebuildable search index.

Agents should call `write_memory` for durable project knowledge: conventions,
architecture decisions, gotchas, validation commands, or resolved bugs. Pass
workspace-relative `paths` globs when the note applies to specific files; those
frontmatter globs drive `relevant_memories` and automatic path hints.

| Tool | Purpose | Inputs |
|---|---|---|
| `list_memories` | List all memory names + descriptions. | optional workspace |
| `read_memory` | Read one memory. | memory name |
| `write_memory` | Create or overwrite a memory. | name, content, optional description, optional paths globs |
| `delete_memory` | Remove a memory. | memory name |
| `search_memories` | Ranked FTS search with grep fallback across memory bodies. | search pattern |
| `relevant_memories` | Memories whose `paths:` frontmatter matches a file path. | file path |

When `[memory] generated_summaries = true`, plumb also writes conservative,
redacted generated memories for idle sessions that touched workspace files. These
are named `episodic-*`, carry generated provenance, and are pruned by
`[memory] generated_memory_keep` (default 50; 0 disables pruning). They summarise
activity only; they do not infer architectural lessons.

---

## Topology

A persistent SQLite/FTS5 semantic index at `<workspace>/.plumb/topology.db`.
On by default (`[topology] enabled = false` opts out); all tools degrade
gracefully when it's off. See the [Topology guide](topology.md).

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
configured.

### `workspace_search`
Ranked discovery **broker** across the workspace's indexed corpora: **code**
and **docs** (Markdown/HTML sections) via the topology FTS5 index, and
**memory** via the memory FTS5 index. Results are ranked within each corpus and
interleaved round-robin by per-corpus rank (raw FTS5 scores are not comparable
across indexes); every hit is labelled `corpus`, `source`, `field`, `score`,
and `why`, and the header reports per-corpus index freshness
(`fresh|stale|building|missing|skipped`) plus `exact_match=false` — this is
discovery, never proof of absence. A stale memory index still serves (honestly
labelled) and kicks an async reindex. Decision rule: use `workspace_search`
for conceptual questions ("where is daemon locking handled?"); use
`search_in_files` for exact literal/regex matches over current file contents.
Ladder: `workspace_search` → topology/LSP → `search_in_files` → bounded
`read_file`. **Inputs:** `query` (required), `corpora` (optional subset of
`code`/`docs`/`memory`; default all), `limit` (default 20, max 100).

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
renaming identifiers — it understands scope and types. When `[edits].show_write_diff`
is on (default), the response appends a per-file unified diff in both preview and
applied modes, for up to the first 20 changed files, with a `+N more file(s)`
summary beyond that.

### `file_diff`
Unified diff between two files (system `diff -U`). **Inputs:** two file paths.

### `version`
Plumb version, Go runtime, OS/arch. **Inputs:** none.

### `run_task`
Run a stored per-language `[tasks.<lang>]` command — no shell, bounded output
(100 KiB/200 lines) and timeout. **Inputs:** `slot` (`build`/`lint`/`test`/`e2e`/`verify`;
`verify` runs build then test), `target` (optional, fills a `{target}` placeholder;
one shell-safe argument). A project-supplied command must be trusted first
(`plumb trust`); defaults and global-config commands always run. Pairs with
`topology_affected` (which says *which* tests to run).

### `agent_config`
Read and (when the user enabled `[agent_config_writes]`) write a small allowlist
of config keys on the user's behalf. **Inputs:** `op` (`describe`/`set`), `set`
(map of dotted key → value for `op=set`), `scope` (`project` only). Writable:
the `[tasks.<lang>]` slots + `log_level`, `ui.theme`, `ui.path_style`,
`topology.exclude_patterns`, `quality.analysers`. Guardrails (git tiers, roots,
strict mode, API keys, the enable knob itself) are never agent-writable. A batch
is validated and applied atomically, tagged `provenance=agent`, and revertible
with `plumb config unset`.
