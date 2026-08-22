# Tools — MCP API Reference

Plumb exposes **59** structured tools to AI assistants. Every write tool is
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
- **Crash durability (fsync-before-ack).** Before a write is acknowledged, the
  staged temp file is fsynced and so is the parent directory after the rename,
  so a successful call survives a hard crash or power cut — the data and the
  directory entry are both on stable storage. Directory-fsync failures are
  logged but never fail an already-landed write (some filesystems refuse
  them). Disable with `[edits] fsync = false` (or `PLUMB_FSYNC=0`) for
  benchmarks or exotic filesystems.
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
Current session name and ID, daemon version, the source commit the daemon binary
was built from (`(dirty)` when the source tree had uncommitted changes, or an
explicit `unknown` for a binary built without a revision stamp — see
[`docs/cli-reference.md`](cli-reference.md#plumb-version)), Go runtime, OS/arch,
start time, and uptime; the session's `purpose` tag when set; the workspace pin's provenance when known
(how, when, and from where the pin was last set — pin-drift observability,
issue #182); live config-store state (generation, last reload time, whether a
restart is needed); and this session's tool-call count plus its slowest calls
(per-call durations from recorded stats).
**Inputs:** none.

`version` was merged into this tool: the old name still works as an
unadvertised alias (it took no arguments, and the answer is a superset — the Go
runtime and OS/arch rows `version` reported are here) and the result carries a
notice pointing at `daemon_info`.

### `rename_session`
Rename the current MCP session. **Inputs:** `name` (string — letters, digits,
and `-` only; user-provided case is preserved; max 25 chars).

The name must be free. A session name is the address the mailbox delivers to, so
a name another **live** session already answers to is refused (compared
case-insensitively), as is `next` — the next-arrival address. Renaming to the
name you already hold is allowed, and an ended session does not reserve its
name. Generated names are checked the same way at registration and re-drawn, so
no two registered sessions share one. (A session whose registration failed keeps
a display name but has no mailbox address at all, so it cannot shadow one.)

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
  and age. Only operations that could modify the workspace are listed: read-only
  git subcommands (`status`, `log`, `diff`, …) and dry-run previews never
  appear, and a call that failed or was refused is kept but marked
  `[failed — no change applied]` — evidence of peer activity, not a change to
  re-read. A successful git commit is attributed in full: its line carries the
  session name, the commit's short SHA and subject, and the repository; other
  git writes are labelled with their subcommand (`git add`, `git push`). If a
  file you are about to edit appears here, re-read it first. When
  `[collab] peer_awareness` is on and the topology index has the file, each entry
  is annotated with its enclosing package/symbol (best-effort, `source=topology`).

Read-only. Workspace-boundary-guarded (no other workspace's session data is
ever exposed). Backed by `internal/session` (session files) and
`stats.RecentWritesByWorkspace` (the `tool_calls` stats table). Both data
sources are read under a 500 ms hard timeout so the tool never blocks the MCP
response.

When `[collab] intents` is on, the output also lists each active session's live
intent (an unverified claim, distinct from the observed `recent_writes`); when
`[collab] mailbox` is on it lists the messages addressed to the caller that are
still unread (listed, not claimed here — reading them is `check_messages`' job).
Neither ever creates `collab.db`.

---

## Cross-agent sharing (`[collab]`)

Four advisory tools for concurrent agents. Each is gated by its own `[collab]`
flag and refuses with a clear enable hint when the flag is off. Everything is
**advisory** — nothing here ever blocks a write — **secret-scrubbed**
(`internal/redact`) before storage and **byte-budgeted** when injected. What an
agent *says* is a **claim**, rendered distinctly from what the daemon *observed*
(phase-1 peer awareness). `share_intent` and same-project messages live in
`<workspace>/.plumb/collab.db` (WAL, auto-gitignored like `topology.db`), created
lazily on first use, expiring per `[collab] intent_ttl_minutes` and pruned on the
daemon session-reaper tick; `share_findings` instead writes a durable generated
memory.

**Delivery is by polling only — plumb does not push to another agent.** That single
constraint shapes the mailbox. It is a property of plumb, not of MCP: some clients
do expose a server→client path that reaches a session between turns, and plumb
wires none of them, so "polling only" holds for every client plumb supports today
rather than being a limit of the transport. Messages are appended to the result of *any*
successful tool call, so an agent that is working receives them without asking;
`check_messages` blocks server-side so an agent that is *waiting* can hand its
turn over instead of spin-polling. An agent idling on its human makes no tool
calls at all and will not see a message until it does something — silence is not
a refusal.

### `share_intent`
Broadcast what you are working on so peers can steer around it (e.g. "refactoring
the rate limiter — avoid `internal/tools/ratelimit*`"). You have at most one live
intent — a new call replaces it — and it is cleared automatically when your
session ends. Peers see it in `workspace_sessions`, and a peer whose write touches
a path matching your `path_globs` gets a bounded advisory hint labelled
`[Peer intent (claim, unverified): …]`; a peer about to run a repo-state git op
(`switch`/`rebase`/`reset`/`commit`/…) on a repository your intent covers gets a
similar warning. Requires `[collab] intents = true`.

**Inputs:** `body` (string, required — free text); `path_globs` (array of
strings, optional — workspace-relative globs for the area you are working on);
`ttl_minutes` (integer, optional — expiry override; defaults to `[collab]
intent_ttl_minutes`).

### `leave_note`
Send a message to a named peer session, or to `next` (whoever attaches to this
workspace next). The send half of the mailbox.

Every message belongs to a **conversation**. Omit `conversation_id` to start one
(the reply reports the new id); quote the id you were given to answer in thread.
A **thread** is capped at `[collab] max_exchanges` messages — once spent,
further replies are refused with an instruction to summarise for the human.

Be clear about what that cap is worth. plumb cannot observe a human turn, so it
counts *total* messages in a thread, not consecutive agent replies; and starting
a new `conversation_id` starts a fresh budget, which nothing prevents. It is a
speed bump that makes continuing a deliberate act, not an enforced ceiling on
how long two agents may talk.

Each message is delivered **exactly once**, to whichever path reads it first:
the block appended to an ordinary tool result, `check_messages`, or the
recipient's next `session_start`. A delivered message stays in the store until
its TTL, which is what gives a conversation its transcript and its exchange
count.

A message is addressed to a **session**, not to a name. When the peer you name
is connected, the message is bound to that exact session and only it can ever
read it. Session names are reusable — an ended session does not reserve its
name, and any session may `rename_session` to a free one — so without that
binding a message its recipient never read would be handed to whoever next
answers to the name, bodies included, while the sender was told it reached the
peer it meant. The trade is deliberate and stated in the reply: a bound message
**expires unread if its recipient never comes back**, which is the right failure
for a private message. Addressing a peer that is *not connected* stores no
binding and is delivered by name, as is `next` — that one is a first-claimer
race by design. Messages written before this existed are likewise unbound and
keep delivering by name. A **daemon restart does not orphan bound mail**: the
reconnecting session inherits its predecessor's identity, but only on the
strength of the proxy session ID `plumb serve` replays in its handshake — never
for merely answering to the name. One predecessor is carried, so a message
unread across two restarts expires instead.

Addressing a session pinned to a **different workspace** is refused up front
unless *that* project has already set `[collab] cross_project = true` — the
send fails with a clear reason rather than being accepted only to sit
unclaimed until it expires. Once confirmed, cross-project messages are stored
in a daemon-level database, never in the recipient's directory — no session
ever writes into another project's workspace — and are labelled with the
sending project when read. `next` is always same-project: "whoever attaches
next" has no meaning across projects. Requires `[collab] mailbox = true`.

**Inputs:** `body` (string, required — the message); `to` (string, optional — a
peer session name, or `next` (default)); `conversation_id` (string, optional —
reply into an existing thread).

### `check_messages`
Read messages other agents have sent you, optionally waiting for one. The receive
half of the mailbox, and what makes an actual back-and-forth possible.

With `wait_seconds` omitted or `0` it returns immediately with whatever is
waiting. With a positive value it **blocks** until a message arrives or the wait
expires, capped by `[collab] max_wait_seconds` (default 55 s, kept below the
client's own MCP call timeout). That is how a session hands its turn to a peer
after asking something, at a cost of one tool call per turn rather than one per
poll. Requires `[collab] mailbox = true`.

It also reports **your own unread mail**: every message you sent that nobody has
read yet, with its age, appended to whatever else it returns. That is the one
fact a sender cannot otherwise observe — plumb does not push, so "no reply"
means either the peer read it and has not answered or never read it at all, and
only this separates the two. It matters more since messages became bound to a
session, because a bound message expires unread rather than passing to a
successor. It reads both stores regardless of your own `cross_project` setting
(that flag gates what this project will *read from* another, and these rows are
your own), which is also where it earns most: a cross-project message to a
recipient who never opted in expires unread by default. Listing is a **read** —
it never consumes the message on the recipient's behalf. Silence means
everything you sent has been picked up.

**Inputs:** `wait_seconds` (integer, optional — block up to this long; `0`
default).

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

### `workspace_symbols`
Search symbols by name across the **entire workspace** via the LSP index;
stdlib/dependency hits are filtered out. Prefer over text search for name
lookups. Pass `uri` to restrict the same search to a **single document** — a
case-insensitive substring match over that file's symbol tree, children
included. **Inputs:** `query` (string, required), `uri` (string, optional —
absolute path, `file://` URI, or workspace-relative path; omit it for the
workspace-wide search). Falls back to the topology index (annotated
`source=topology, mode=indexed-approximate`) when the LSP errors or times out
and `[topology]` is enabled. `find_symbol` was merged into this tool: the old
name still works as an unadvertised alias — `query` and `uri` pass through
unchanged, a uri-less call now runs the workspace-wide search it used to
redirect to, and the result carries a notice pointing at `workspace_symbols`.

### `get_definition`
Source location where a symbol is defined. **Inputs:** `uri` (required), and
either `line` + `character` or `symbol_name`.

### `explain_symbol`
Hover documentation and type information for a symbol. **Inputs:** `uri`
(required), and either `line` + `character` or `symbol_name`.

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
cache with the other symbol queries, so a warm outline reuses an existing
query. `list_symbols` was merged into this tool: the old name still works as an
unadvertised alias (its `include_signatures` flag is dropped — the outline
always renders signature lines) and the result carries a notice pointing at
`file_outline`.

### `find_references`
All usages of a symbol across the workspace, each with its source line.
**Inputs:** `uri` (required), either `line` + `character` or `symbol_name`,
`include_declaration` (bool, default true).

### `call_hierarchy`
Incoming and outgoing calls for a function. **Inputs:** `uri` (required), and
either `line` + `character` or `symbol_name`.

### `type_hierarchy`
Supertypes and subtypes of a class or interface. **Inputs:** `uri` (required),
and either `line` + `character` or `symbol_name`.

### `diagnostics`
LSP errors, warnings, and hints. **Inputs:** `uris` (array of `file://` URIs —
omit or pass `[]` for all files with issues; one URI for a single file; many
URIs to batch). A single call replaces multiple per-file calls. The
`plumb-diagnose` skill puts this in the wider triage loop: reading a refusal,
getting compile truth, and telling a broken tool from a server that has not
warmed up.

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

The same honesty applies to a **cold** server. While a language server is
still completing its handshake it does not fail — it simply has not published
anything yet, so a report taken then would read as a clean bill of health. Any
report assembled while the server is warming is therefore labelled
`INCOMPLETE`, with the elapsed warm-up time and a `daemon_info` pointer: a
clean result there is **not** proof the code compiles. The label is applied to
every outcome, not just an empty one (a partial set can be missing whatever the
server has not reached yet), and a multi-URI batch is labelled when the server
behind *any* of its URIs is warming — batches can span languages, and so
servers.

---

## LSP semantic edits

All default to `dry_run: true`. When applied, semantic edits use the same
write-tool bookkeeping as filesystem writes: path locks, dirty guards,
`workspace/didChangeWatchedFiles`, cache invalidation, undo capture, topology
refresh, quality hooks, and differential post-write diagnostics.

**Prefer these over `edit_file` for whole-declaration changes.**
`replace_symbol_body` / `insert_before_symbol` / `insert_after_symbol` /
`safe_delete_symbol` address a declaration by `name_path` (e.g.
`"ClassName/methodName"`) — no line/character coordinates, no hand-computed
ranges, no read-modify-write of the surrounding text. Reach for `edit_file`
when you're changing a fragment *within* a declaration (a few lines, a
string, a single statement); reach for the symbol-edit family when you're
replacing, inserting around, or deleting an entire function, method, type,
or other named declaration. Neither `edit_file` nor `transaction_apply`
requires raw coordinates by default — both default to `old_string` /
`new_string` string matching, and `transaction_apply` has no coordinate mode
at all; `edit_file`'s line/character `range` mode is an optional fallback,
not the primary path.

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

### `move_symbol`
Relocate a whole top-level declaration (function, method, type, const, or var)
from one file to another **within the same directory/package**, atomically — the
declaration is removed from `source_uri` and appended to `destination_uri` in a
single all-or-nothing write (destination-write failure rolls the source back, so
the symbol is never duplicated or lost). **Inputs:** `source_uri`, `name_path`,
`destination_uri` (required), `include_doc_comment` (bool, **default true** — a
relocated declaration keeps its doc comment), `create_destination` (bool,
default false; a created Go file is seeded with the source's `package` clause),
`dry_run` (default true), `dirty_ok`. Locates the symbol via the LSP
document-symbol tree, falling back to tree-sitter when the server is cold.
**Conservative v1:** it does **not** rewrite references or imports, so a move
that would change the symbol's package or import path — a different directory, or
a different Go `package` clause — is **refused** rather than applied
half-correctly. Also refuses an ambiguous bare name, a missing destination
without `create_destination`, out-of-workspace paths, and — for Go — a
destination whose build constraints differ from the source's: **explicit**
(`//go:build`/`// +build` comments) or **implicit** (the `_GOOS`/`_GOARCH`/
`_GOOS_GOARCH` and `_test` filename-suffix conventions, e.g.
`handlers_linux.go` or `foo_test.go`), compared independently — not for
cross-axis equivalence, so a plain file with `//go:build linux` moved into a
`_linux.go` file is still refused. Moving a declaration between differently
constrained files would silently change what compiles per platform/tag, or
drop it from the production build entirely (the `_test.go` case). The
response carries a unified diff of both files (preview in dry-run, applied
change otherwise) unless `show_write_diff` is disabled. Undo is per-file:
reverting a move takes **two** `undo_edit` calls (source, then destination),
with a transient intermediate state where the declaration is duplicated in
both files.

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
pattern is all lowercase), `context_lines` (0–50, like `rg -C`; disjoint groups
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
Read up to 20 files in parallel; per-file errors reported inline. Reads are
recorded per file exactly like `read_file`, so a batch-read file is editable
under `[edits] strict` mode with no re-read. **Inputs:** `paths` (array, 1–20,
required), plus the same slicing/search parameters as `read_file` — applied
**uniformly to every path** in the call, with no per-path override:
`start_line`, `end_line` (1-based, inclusive; window every file the same way),
`pattern` (search every file instead of windowing it — literal by default, a
Go RE2 regex when `use_regex`), `use_regex`, `context_lines` (0–50, like
`rg -C`), `max_matches` (1–2000, default 200). A windowed batch read still
records each file's FULL mtime/sha in the read tracker (identical to
`read_file`'s own ranged-read behaviour — strict mode is mtime-based, not
range-based) and its per-file header still carries `baseline` (the whole-file
byte size), so a `lines=2` slice is distinguishable from a 2,000-line file.
The 20-path cap is unchanged by slicing. When every successfully-read file
agrees on one indent convention (3 or more files), it is stated once in a
`# plumb-read-batch indent=…` preamble instead of being repeated per file.

### `find_files`
Glob/regex file or directory finder, and plumb's directory lister. **Inputs:**
`pattern` (optional — omit to match everything), `path`, `type` (`file` | `dir`
| `any`, default `file`), `extension`, `max_depth` (`1` lists one level, like
`ls`), `max_results` (default 500), `include_hidden`, `include_details`,
`sort_by` (`name` | `size` | `modified`, default `name`), `use_regex`. Honours
`.gitignore`. Glob patterns support brace alternation (`*.{ts,tsx}`), including
nested and repeated groups; an expansion past 256 alternatives or 10 levels of
nesting is refused rather than truncated. `include_details` renders each entry
with a `[FILE]`/`[DIR]`/`[LINK]` marker, its size and modified time (symlinks
as `name -> target`) instead of a bare path list.

`list_files` and `list_directory` were merged into this tool: the old names
still work as unadvertised aliases (`list_files`' `root` is mapped to `path`
and its depth default of 8 is pinned; `list_directory` is served as
`max_depth:1, type:"any", include_details:true`), each with a notice naming
`find_files`. Both now inherit `find_files`' `.gitignore` confinement in place
of `list_files`' hardcoded exclude list.

### `search_in_files`
ripgrep-style content search; smart-case; honours `.gitignore`. **Inputs:**
`pattern` (required — **literal text by default**, a Go RE2 regex only when
`use_regex` is true), `use_regex`, `path`, `glob` (supports brace alternation,
e.g. `**/*.{go,md}`), `exclude` (array of globs), `case_sensitive`,
`context_lines` (0–50), `max_results` (default 200), `include_hidden`,
`max_file_bytes`, `include_enclosing_symbol` (bool — annotates each hit with the
deepest enclosing LSP symbol; requires LSP). Total output is capped at 200 KiB;
truncation is labelled.

A literal-mode search whose pattern contains regex syntax appends a one-line
note saying so, since the alternative reading of a clean "No matches" is "these
don't exist". Unambiguous syntax (a single `|` — `||` is excluded, since
boolean-or is ubiquitous — `.*`, `.+`, a leading `^`, and regex escapes such as
`\.`/`\d`/`\w`/`\s`) is flagged whether or not the search matched; shapes that
are also ordinary code (`[...]`, `(...)`, `{n,m}`, a trailing `$`) are flagged
only when nothing matched. `find_replace` and `read_file`'s pattern mode carry
the same note.

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
Replacing an entire declaration? Prefer `replace_symbol_body` (see *LSP
semantic edits* above) — addressed by `name_path`, no coordinates needed.

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
Delete files and empty directories (refuses directories unless `allow_dir`, and
even then only when empty — there is no recursive delete). The response reports
the line and byte count removed (bytes only for a binary or oversized file).
**Inputs:** `file_path` **or** `paths` (array, max 100 — not both), `dirty_ok`,
`allow_dir`.

`paths` batches round-trips, not semantics: every path obeys the same per-path
rules. All paths are validated — boundary, existence, `allow_dir`, dirty —
**before any is removed**, so a batch that will be refused is refused whole.
Removal order is files first, then directories deepest-first, so listing a tree's
files together with its directories works in one call. Each path consumes a
rate-limit token. To clear a tree: `find_files` its contents, then one
`delete_file` batch with `allow_dir: true`.

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
frontmatter globs drive `relevant_memories` and automatic path hints. The
`plumb-memory` skill walks the whole lane — search before re-deriving, the
search modes and when each is wrong, what belongs in a memory, and deleting one
that has gone false.

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
`read_file` — the `plumb-explore` skill teaches the same ladder, with the
signal for leaving each rung. **Inputs:** `query` (required), `corpora`
(optional subset of `code`/`docs`/`memory`; default all), `limit` (default 20,
max 100).

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
**Inputs:** `name` (required unless `mode="reachability"`), `depth` (default 3, max 4),
`max_nodes` (default 100, max 200), `max_bytes` (default 30000), `edge_kinds` (default
`["imports","calls"]`).

`mode: "reachability"` switches to a different, package-level question: what does an
entry point (a `package main` directory, or a `topology_routes` candidate) actually pull
in, and what is unreachable from every entry point — see
[Topology → Package-level reachability](topology.md#package-level-reachability) for the
full shape, its production-imports-only scoping, its Go-only limitation, and its
correctness note. **Inputs (reachability mode; `roots`/`path_to`/`layers` are rejected
outside this mode, not silently ignored):** `roots` (array of directories, or `"main"`;
default every `package main` directory plus `topology_routes` candidates), `path_to` (a
directory — returns one root→target chain instead of the summary), `layers` (boolean —
returns a package-SCC condensation instead of the summary). Every response opens with
`package-level (import edges, production imports only — Go _test.go importers
excluded); function-level unavailable` and is capped at ~5 KB. Go-only for now: a
workspace whose extractor doesn't emit the per-file package/import edge shape refuses
with a clear message rather than reporting every package unreachable.

### `topology_affected`
Given changed files/symbols, return likely affected files and tests. **Inputs:**
`files` (array), `symbols` (array), `max_results` (default 50).

### `topology_routes`
Pattern-matches entry-point-shaped symbol names/signatures (Go HTTP handlers,
Cobra commands, Python `@app.route`). Does **not** parse route registrations or
call sites — it cannot recover a path-to-handler binding, only candidate
functions whose name/signature look like a known entry-point idiom. Results
annotated with confidence — heuristic. **Inputs:** `framework` (optional: `go`
| `python` | `cobra`), `path_prefix` (optional), `limit` (default 20).

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
`checkout`, `restore`, `rebase`, `revert`, `cherry-pick`, …) needs
`allow_destructive` + `confirm:true`.
**Network** (`push`, `fetch`, `pull`) needs `allow_push` + `confirm:true`.
Force-push to a protected branch and ad-hoc URL pushes are always refused. See
[Configuration → `[git]`](configuration.md#git--tiered-git-tool-gating). The
`plumb-git` skill covers the tiers from the caller's side, including the
narrower plumb tool to prefer over a destructive git command (`undo_edit`,
`file_status`, `minimal_diff_review`).

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
`commit -m <message>`, plus `-- <files>` when `files` is passed to limit the
commit to those paths (so `--amend`, `--no-verify`, `-F`, and the editor are
unreachable) and `add` only runs `add -A -- <files>` (no globs, no free-form
paths; `-A` stages deletions of the named paths, which is why removing a tracked
file needs no `git rm`). Pre-commit hooks always run. Every non-read call consumes one
write-rate-limit slot. Output is capped (200 lines for `log`/`blame`, 100 KiB
overall); `add` and `commit` return a concise summary (staged file count, or
`<short-hash> <subject>`) rather than raw git output.

**Attribution:** with `[git] commit_trailer = true` (default off) every
plumb-mediated commit is stamped with a `Plumb-Session: <session-name>`
trailer; regardless of that knob, `workspace_sessions` always lists recent
commits per session (short SHA, subject, repository) from its recent-writes
feed. See [Configuration → `[git]`](configuration.md#git--tiered-git-tool-gating).

With `[collab] intents = true`, a **repo-state op** — every destructive-tier op,
plus the write-tier HEAD movers `commit`/`switch`/`checkout` — also surfaces any
live peer `share_intent` claims covering the repository as an advisory
`# plumb-warning:` block naming the peer and the claim.

Coverage is **tier-aware**. A destructive op can clobber anything in the
repository, so any live claim whose paths lie inside it counts. A write-tier op
(`commit`/`switch`) warns only for a claim that is genuinely repo-wide — an
unscoped broadcast, or globs such as `**` that cover the repository root — so a
narrowly scoped claim like `site/**` no longer fires on every commit.

Informational only: it never blocks the op, never requires `confirm`, and
index-only writes (`add`, `restore --staged`) stay silent.

**Inputs:** `subcommand` (required), `args` (array), `files` (array, for `add`),
`message` (string, for `commit`), `confirm` (bool).

### `git_init`
Initialise a git repository at a path. **Inputs:** path, `init_plumb` (bool —
also creates `.plumb/context.md`).

### `find_replace`
Text/regex find-and-replace across files; **dry-run by default.** **Inputs:**
`pattern`, `replacement` (required), `path`, `glob`, `use_regex`, `dry_run`
(default true), `dirty_ok`, `format_after` (run the workspace formatter),
`case_sensitive`, `max_files`, `max_file_bytes`. For identifier refactors use
`rename_symbol` (scope- and type-aware); `find_replace` is the plain-text lane.
When `[edits].show_write_diff`
is on (default), the response appends a per-file unified diff in both preview and
applied modes, for up to the first 20 changed files, with a `+N more file(s)`
summary beyond that.

### `file_diff`
Unified diff between two files (system `diff -U`). **Inputs:** two file paths.

### `minimal_diff_review`
**Advisory** review of a git diff for signs of over-building — findings **never
block a write**, they are hints. Deterministic (no LLM): it flags a **single-use
abstraction**, a **thin forwarding wrapper**, a **new dependency with a
well-known stdlib equivalent**, a **possible duplicate helper**, and a **logic
change with no accompanying test change**. Evidence is *asymmetric* — a check
stays silent unless it can point at concrete evidence and (where defensible) a
smaller alternative, so **silence is not proof a change is minimal**. Every
finding is confidence-labelled: `high` = proven from the diff text; `low` =
leans on the topology index, which is *approximate* (its call graph is
intra-file — unlike `find_references`' exact cross-file lookup — and may be a few
edits stale). Reviews the working-tree diff vs `base_ref`; scope it with `files`
in a shared worktree so unrelated peer-agent edits are excluded. Degrades cleanly
outside a git repository, and each response carries a *not analysed / limits*
section listing the review's blind spots. **Inputs:** `base_ref` (default
`HEAD`), `files` (array, optional scope), `mode` (`changed` (default) = working
tree vs `base_ref` | `staged` = index vs `base_ref`), `max_findings` (default
20, max 100), `include_suggestions` (default true).

### `run_task`
Run a stored per-language `[tasks.<lang>]` command — no shell, bounded output
(100 KiB/200 lines) and timeout. **Inputs:** `slot` (`build`/`lint`/`test`/`e2e`/`verify`;
`verify` runs build then test), `target` (optional, fills a `{target}` placeholder;
one shell-safe argument). A project-supplied command must be trusted first
(`plumb trust`); defaults and global-config commands always run. Pairs with
`topology_affected` (which says *which* tests to run) — the `plumb-testing`
skill walks the whole post-edit loop.

### `mutation_test`
Verify that tests actually assert what they appear to: apply an explicit mutant,
prove it still **compiles**, run a scoped test set, classify the result, and
restore the file. **Inputs:** `mutants` (array, 1–20, each `{file_path,
old_string, new_string, label?}` — an exact-once `str_replace` in the style of
`edit_file`; it does **not** generate mutants), `test_task` (slot, default
`test`), `test_target` (fills the stored test command's `{target}` — the way to
scope the run; ask `topology_affected` what to name), `compile_task` (slot,
default `build`), `timeout_seconds` (per step, default 600).

**Three outcomes.** `killed` — the mutant compiled and a test failed, so the
assertion is real. `survived` — the mutant compiled and every test still passed,
so the assertions covering that line are **vacuous**; this is the finding that
matters. `invalid` — the mutant did not apply, did not compile, or the run timed
out, or a command could not be started; it proves nothing and is **never**
reported as a kill. That last distinction is the tool's reason to exist: a
mutant that does not compile, a `sed` that silently matched nothing, or a test
runner that is not installed all make the test command fail, and a hand-rolled
harness reads any of those failures as a kill.

The compile gate cannot be disabled — a workspace with no `build` command
configured is refused rather than served unverifiable verdicts. The gate always
runs unscoped, because a whole-module compile catches breakage a package-scoped
test never reaches.

**The workspace must be green before the run starts.** A kill means "passed
before this change, failed after it", so both halves have to be checked: the
compile and test commands are run once on the **unmutated** tree, and the whole
run is refused if either fails. Otherwise a suite that was already red — a
peer's edit elsewhere in the tree, a pre-existing failure, a missing test
dependency — reports every mutant `killed` for a reason that has nothing to do
with any mutant. The dirty-file refusal below does not cover this: it guards the
file being mutated, not the rest of the workspace. The cost is one extra
compile+test cycle per run, not per mutant; scope it with `test_target`.

**Restoration is guaranteed** on every exit path (pass, fail, compile error,
timeout, panic, cancellation): the pre-mutation bytes are snapshotted in memory,
rewritten under the same per-path lock the write tools use, and verified by
SHA-256 before the run reports clean. A file with **uncommitted changes is
refused with no override** — a clean file is what makes `git checkout` a
guaranteed recovery if the daemon dies mid-run. One run at a time per daemon; a
second call is refused rather than queued, since concurrent runs would read each
other's breakage as their own result.

### `agent_config`
Read and (when the user enabled `[agent_config_writes]`) write a small allowlist
of config keys on the user's behalf. **Inputs:** `op` (`describe`/`set`), `set`
(map of dotted key → value for `op=set`), `scope` (`project` only). Writable:
the `[tasks.<lang>]` slots + `log_level`, `ui.theme`, `ui.path_style`,
`topology.exclude_patterns`, `quality.analysers`. Guardrails (git tiers, roots,
strict mode, API keys, the enable knob itself) are never agent-writable. A batch
is validated and applied atomically, tagged `provenance=agent`, and revertible
with `plumb config unset`.
