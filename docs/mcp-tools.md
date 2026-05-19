# MCP Tools

Plumb exposes LSP and filesystem capabilities to LLMs as MCP tools. Each tool is registered with the MCP server at startup and appears in the `tools/list` response that Claude Desktop (or any other MCP client) uses to discover available actions.

Tools are implemented in `internal/tools/` and registered in `internal/cli/daemon.go`.

LSP tools cache their results in the session-scoped `cache.Cache`; entries are invalidated automatically when the language server reports that a file has changed (`textDocument/publishDiagnostics`).

Filesystem tools (read, write, edit, list, search) do not require a running language server. Write tools (`write_file`, `edit_file`, `delete_file`, `rename_file`, `transaction_apply`) notify the LSP via `workspace/didChangeWatchedFiles` after every successful write so symbol indexes and diagnostics stay current. This is the LSP-correct primitive for external file changes — distinct from the open-document lifecycle, which is for editor-managed buffers.

The full tool surface (including LSP-edit tools, memory tools, and VCS tools not detailed below) is enumerated in [`AGENTS.md`](../AGENTS.md). This document focuses on the tools whose schemas have non-trivial parameters.

---

## Tool catalogue

### `find_symbol`

Search for symbols (functions, types, variables, classes) by name **within a
single document**. For workspace-wide search, use `workspace_symbols`.

**Source**: `internal/tools/find_symbol.go`

> **Changed in 0.3.2:** `uri` is now required. Previously, calling without `uri`
> performed a workspace-wide search that was a byte-identical duplicate of
> `workspace_symbols` (same LSP call, same cache key, same output format). The
> two tools were split so each has a single clear purpose.

#### Input schema

```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Symbol name or substring to search for (case-insensitive)"
    },
    "uri": {
      "type": "string",
      "description": "Document to search within (file:// URI). Required."
    }
  },
  "required": ["query", "uri"]
}
```

| Field | Required | Description |
|---|---|---|
| `query` | yes | Case-insensitive substring match on symbol names |
| `uri` | yes | `file://` URI of the document to search within |

#### Behaviour

Calls `textDocument/documentSymbol` to fetch the full symbol tree for the
document, then filters client-side by case-insensitive substring match on
the symbol name (including child symbols). The full symbol list is cached by
URI; filtering is applied on each call without an extra round-trip.

#### Required LSP capabilities

| Method | Capability check |
|---|---|
| `textDocument/documentSymbol` | `ServerCapabilities.DocumentSymbolProvider.Enabled` |

#### Output format

```
Symbols matching "greet" in file:///project/main.py:

- Greeter (Class) at line 10
- greet (Method) at line 15
```

No results:
```
No symbols matching "Xyz" in file:///project/main.py.
```

#### Cache key

`<uri>:docSymbols` (the unfiltered list; filtering is client-side per call)

---

### `workspace_symbols`

Search for symbols by name across the entire workspace.

**Source**: `internal/tools/workspace_symbols.go`

#### Input schema

```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Symbol name or substring to search for across the entire workspace"
    }
  },
  "required": ["query"]
}
```

| Field | Required | Description |
|---|---|---|
| `query` | yes | Symbol name or substring. Fuzziness depends on the language server: gopls does subsequence matching; pyright does substring |

#### Behaviour

Calls `workspace/symbol` with `query`. The language server does the matching
server-side. Results are post-filtered through `isInWorkspace()` to drop
dependency-cache and stdlib hits (anything under `/pkg/mod/`, GOROOT, or
outside the acquired workspace root). Results are cached by query string.

#### Required LSP capabilities

| Method | Capability check |
|---|---|
| `workspace/symbol` | `ServerCapabilities.WorkspaceSymbolProvider.Enabled` |

#### Output format

```
Found 2 symbol(s) matching "Greeter":

- Greeter (Class) at file:///project/main.py:10
- greet (Method) at file:///project/main.py:15
```

No results:
```
No symbols found matching "Xyz".
```

#### Cache key

`wsSymbols:<query>`

---

### `list_symbols`

Return the complete symbol outline of a file: every function, type, method, field, and constant with its kind and line range.

**Source**: `internal/tools/list_symbols.go`

#### Input schema

```json
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "file:// URI of the document to outline"
    },
    "include_signatures": {
      "type": "boolean",
      "description": "When true, append the first non-blank, non-comment source line of each function, method, or constructor symbol below its entry."
    }
  },
  "required": ["uri"]
}
```

| Field | Required | Description |
|---|---|---|
| `uri` | yes | `file://` URI of the document to outline |
| `include_signatures` | no | Append the declaration line of function/method/constructor symbols (shows parameter types and receiver types). Non-callable symbols (fields, constants, types) are not annotated. |

#### Behaviour

Calls `textDocument/documentSymbol` for the full symbol tree and formats
every entry with its kind and line range. Children are indented one level per
nesting depth. Results are cached by URI.

When `include_signatures=true`, the tool reads the file on disk and appends
the first non-blank, non-comment source line at each callable symbol's start
line with a `→` prefix. This shows the signature without the full body and
is skipped for non-callable kinds (struct fields, constants, type aliases,
etc.) where the declaration line adds little value.

#### Required LSP capabilities

| Method | Capability check |
|---|---|
| `textDocument/documentSymbol` | `ServerCapabilities.DocumentSymbolProvider.Enabled` |

#### Output format

```
Symbols in file:///project/main.go (4 total)

Greeter (Struct) lines 5–9
  Prefix (Field) line 6
Greet (name string) string (Method) lines 11–13
  → func (g Greeter) Greet(name string) string {
```

#### Cache key

`<uri>:docSymbols`

---

### `get_definition`

Jump to the definition of the symbol at a given position in a document.

**Source**: `internal/tools/get_definition.go`

#### Input schema

```json
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Document URI (file:// scheme)"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number",
      "minimum": 0
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset",
      "minimum": 0
    }
  },
  "required": ["uri", "line", "character"]
}
```

| Field | Required | Description |
|---|---|---|
| `uri` | yes | `file://` URI of the document containing the symbol |
| `line` | yes | Zero-based line number of the cursor position |
| `character` | yes | Zero-based character offset of the cursor position |

#### Behaviour

Calls `textDocument/definition` at the given position and returns the
location(s) where the symbol is defined.  Line and character numbers in the
output are 1-based for human readability (LSP uses 0-based internally).

Results are cached by the exact position triple `<uri>:def:<line>:<character>`.

#### Required LSP capabilities

| Method | Capability check |
|---|---|
| `textDocument/definition` | `ServerCapabilities.DefinitionProvider.Enabled` |

#### Output format

Single definition:
```
Definition at file:///project/base.go:3:1
```

Multiple definitions:
```
2 definitions for symbol at file:///project/main.go:11:2:

1. file:///project/base.go:3:1
2. file:///project/impl.go:20:1
```

No result:
```
No definition found for symbol at file:///project/main.go:11:2.
```

#### Cache key

`<uri>:def:<line>:<character>`

---

### `explain_symbol`

Get documentation and type information for the symbol at a given position
(hover information from the language server).

**Source**: `internal/tools/explain_symbol.go`

#### Input schema

```json
{
  "type": "object",
  "properties": {
    "uri": {
      "type": "string",
      "description": "Document URI (file:// scheme)"
    },
    "line": {
      "type": "integer",
      "description": "Zero-based line number",
      "minimum": 0
    },
    "character": {
      "type": "integer",
      "description": "Zero-based character offset",
      "minimum": 0
    }
  },
  "required": ["uri", "line", "character"]
}
```

| Field | Required | Description |
|---|---|---|
| `uri` | yes | `file://` URI of the document |
| `line` | yes | Zero-based line number |
| `character` | yes | Zero-based character offset |

#### Behaviour

Calls `textDocument/hover` at the given position and returns the hover content
verbatim.  Both `gopls` and `pyright` return Markdown — the content is passed
through to the LLM without modification so it can render code fences and
inline formatting.

Results are cached by position.

#### Required LSP capabilities

| Method | Capability check |
|---|---|
| `textDocument/hover` | `ServerCapabilities.HoverProvider.Enabled` |

#### Output format

Content present (typically Markdown from the language server):
```
```go
func (g *Greeter) Greet(name string) string
```
Greet returns a personalised greeting string.
```

No content:
```
No documentation found for symbol at file:///project/main.go:11:2.
```

#### Cache key

`<uri>:hover:<line>:<character>`

---

### Symbol edit tools

`insert_before_symbol`, `insert_after_symbol`, `replace_symbol_body`, and
`safe_delete_symbol` form a family of LSP-backed structural edits. They all
share the same target-resolution pipeline: fetch the document's symbol tree,
walk it by `name_path` (slash-separated for nested symbols, e.g.
`Greeter/greet`), and apply a single `TextEdit` at one of the symbol's
positions.

**Source**: `internal/tools/symbol_edits.go`

#### Common arguments

| Field | Required | Default | Description |
|---|---|---|---|
| `uri` | yes | — | Document URI (`file://` scheme) |
| `name_path` | yes | — | Slash-separated symbol path within the file (e.g. `"ClassName/methodName"`, or just `"funcName"` for top-level) |
| `content` | yes (insert/replace) | — | Text to insert or replace with |
| `dry_run` | no | `true` | Preview only when true; write to disk when false |
| `include_doc_comment` | no | `false` | Since 0.3.2; see below. Not accepted by `insert_after_symbol` |

#### `include_doc_comment` (since 0.3.2)

Most LSP servers (including gopls) report a symbol's `Range` starting at the
declaration keyword (`func`, `class`, `type`, etc.), **excluding** any doc
comment immediately above it. Without compensating, this creates three
broken scenarios:

1. `replace_symbol_body` replaces only the declaration, leaving the old doc
   comment as a stale comment above whatever you wrote.
2. `safe_delete_symbol` deletes only the declaration, leaving an orphaned doc
   comment pointing at the next symbol in the file.
3. `insert_before_symbol` inserts between the doc comment and its symbol —
   wrong if you intended to add a new (commented) declaration above the
   existing one.

When `include_doc_comment=true`, the operation's range is extended upward to
cover any contiguous comment lines flush against the symbol. A "comment line"
is any line whose first non-whitespace characters match `//`, `#`, `/*`, or
`*` — covering Go/Rust/C/Java/JS line comments, Python and shell hash
comments, and the lines of JSDoc/JavaDoc `/** ... */` blocks. A blank line
or non-comment line terminates the scan.

| Tool | What `include_doc_comment=true` does |
|---|---|
| `insert_before_symbol` | Inserts before the first comment line instead of between the comment and the declaration. Useful when adding a new commented declaration above an existing one |
| `replace_symbol_body` | Replaces the comment block *and* the declaration together. Your `content` should include the new doc comment |
| `safe_delete_symbol` | Deletes the comment block *and* the declaration together |
| `insert_after_symbol` | N/A — insertion-after has no leading-comment ambiguity |

#### Required LSP capabilities

| Method | Used by |
|---|---|
| `textDocument/documentSymbol` | all four tools (target resolution) |
| `textDocument/references` | `safe_delete_symbol` (reference check) |

#### Atomic writes

Edits are applied via `applyTextEditsToFile`: read, splice the new text in at
the computed byte offsets, write to `<path>.tmp`, then `rename(2)` over the
original. The file is replaced atomically; on any failure the original is
left untouched.

---

## Cache invalidation

All tool results are stored with the document URI as a prefix in the cache key.
When the language server sends `textDocument/publishDiagnostics` for a URI
(which happens after any document change), `cache.Invalidator.Handle` evicts
every entry whose key contains that URI.

This means:
- Stale results are never served after a file is saved.
- The first tool call after a change pays the LSP round-trip cost; subsequent
  calls for the same position within the same TTL window are served from cache.

The TTL is configured in `~/.config/plumb/config.toml` under `[cache]`:

```toml
[cache]
ttl = "5m"
max_size = 1000
```

---

## Adding a new tool

See `AGENTS.md` → "How to add an MCP tool" for the full checklist.  Brief summary:

1. Create `internal/tools/<name>.go` implementing `mcp.Tool`.
2. Add unit tests in `internal/tools/<name>_test.go` using `mockLSP`.
3. Register with `srv.Register(tools.New<Name>(proxy, c, ttl))` in
   `internal/cli/serve.go`.
4. Document inputs, outputs, required capabilities, and cache key in this file.

Keep tools focused: each tool should call one or two LSP methods and return
a clear, human-readable string.  Composite results (e.g. definition + hover +
references in one call) belong in a separate tool, not as flags on an existing one.

---

## Tool error handling

When a tool's `Execute` method returns a non-nil error, the MCP server wraps
it in an `isError: true` result payload (per the MCP spec) rather than a
JSON-RPC error object.  The error message is prefixed with `"error: "` so the
LLM can distinguish tool failures from empty results.

LSP errors (server not ready, method not supported, timeout) propagate as tool
errors and are not retried automatically.  The client can retry the same call;
the language server supervisor will restart the process if it has crashed.

---

## Filesystem tools

### `read_file`

Read the text contents of a file.

**Source**: `internal/tools/read_file.go`

| Field | Required | Description |
|---|---|---|
| `path` | yes | Absolute path or `file://` URI |
| `start_line` | no | 1-based line to start reading from (inclusive) |
| `end_line` | no | 1-based line to stop reading at (inclusive) |

**Output format:**

```
# plumb-read mtime=2026-05-11T13:46:38.895137000+10:00 indent=tabs

<file contents or selected line range>
```

The mtime header is the file's modification time at read time, in RFC3339Nano. Copy it verbatim into `edit_file`'s `expected_mtime` parameter to assert the file hasn't changed between read and edit.

The `indent=` field reports the leading whitespace style of the returned body: `tabs`, `spaces`, `mixed`, or `none`. Many clients render tab characters as visual spaces in code blocks; when `indent=tabs`, ensure leading whitespace in `old_str` uses real tab characters (`\t`) rather than the spaces you see on screen, or the `edit_file` match will silently fail.

**Notes:**

- Binary files (null byte in first 8 KiB) are rejected.
- When a line range is supplied, the read is streamed via `bufio.Scanner` and stops at `end_line` — large files are not loaded entirely.
- Output is capped at 200 KiB. Use line ranges on larger files.
- Records the mtime in the session's `ReadTracker` so `edit_file` strict mode can verify the agent read the file before editing it.
- The `indent=` classification is over the returned body, not the full file — a line-ranged read reflects only the slice you received.

---

### `read_multiple_files`

Read up to 20 files in a single call.

**Source**: `internal/tools/read_multiple_files.go`

| Field | Required | Description |
|---|---|---|
| `paths` | yes | Array of up to 20 absolute paths or `file://` URIs |

Reads run in parallel (capped at 8 concurrent). Errors for individual files are reported inline — one unreadable file does not abort the others. Each file's output includes the same `# plumb-read mtime=...` header as single `read_file`.

---

### `write_file`

Create or overwrite a file atomically.

**Source**: `internal/tools/write_file.go`

| Field | Required | Description |
|---|---|---|
| `path` | yes | Absolute path or `file://` URI |
| `content` | yes | Full content to write |
| `create_dirs` | no | Create parent directories if absent. Default `true` |
| `dirty_ok` | no | Allow writing a file that has uncommitted changes. Default `false` — refused if the target file is dirty. Pass `true` to overwrite anyway. |

**Safety model** (shared with all write tools):
- **Dirty check** — before writing, plumb runs `git status --porcelain -- <file>`. If the file has uncommitted changes (modified, staged, or untracked) and `dirty_ok` is `false` (the default), the write is refused with a clear error message. This protects uncommitted work from being silently overwritten by an agent. Pass `dirty_ok: true` to bypass. When git is not on `$PATH` or the file is not inside a git repository, the check is skipped and the write proceeds normally.
- **Per-path lock** serialises concurrent writes to the same file from any session.
- **Atomic rename** — content staged in `os.TempDir()` then `os.Rename`. EXDEV cross-device falls back to a `.plumb.tmp` sibling automatically.
- **Symlink-aware** — if `path` is a symlink, the link is resolved and the write goes through to the underlying target (the symlink is not replaced with a regular file).
- **Permissions preserved** — existing file mode is copied; new files get `0644`.
- **LSP notification** — `workspace/didChangeWatchedFiles` sent with `FileCreated` (new file) or `FileChanged` (overwrite).
- **Cache invalidation** — symbol cache entries for the URI are evicted immediately.
- **Post-write diagnostics** — the URI's diagnostics are polled for up to 300ms after the write; any change is appended to the response.
- **Rate-limited** — counts against the per-session write limit (default 120/min).

---

### `edit_file`

Apply one or more str_replace edits to an existing file.

**Source**: `internal/tools/edit_file.go`

| Field | Required | Description |
|---|---|---|
| `path` | yes | Absolute path or `file://` URI |
| `edits` | yes | Array of `{old_str, new_str}` objects, applied sequentially |
| `expected_mtime` | no | RFC3339Nano mtime previously emitted by `read_file`'s header. If present, the edit is rejected when the file's current mtime differs — opt-in optimistic concurrency. |
| `dirty_ok` | no | Allow editing a file that has uncommitted changes. Default `false`. |

Each `old_str` must appear **exactly once** in the file at the time the edit is evaluated. Absent or ambiguous strings are rejected with a clear error.

**Safety model** (in addition to the shared model above):
1. **Per-path lock** serialises against any concurrent `write_file` / `edit_file` / `delete_file` / `rename_file` / `transaction_apply` targeting the same path.
2. **Uniqueness lock** — the exact-once requirement detects concurrent modifications that changed the surrounding context.
3. **CRLF tolerance** — line endings in `old_str` are normalised against the file before matching. An LF `old_str` matches a CRLF file. **Limitation:** detection looks for the first CRLF in the file; files with mixed line endings (both `\r\n` and `\n`) have undefined matching behaviour. Normalise with `dos2unix` or `unix2dos` before editing.
4. **expected_mtime gate** — when supplied, the file's current mtime must equal the provided value, else the edit is rejected immediately.
5. **Strict mode** (opt-in via `[edits].strict = true` or `PLUMB_STRICT_EDITS=1`) — the file must have been read via `read_file` in this MCP session AND its current mtime must match what `read_file` observed. Per-session via `ReadTracker`.
6. **In-memory application** — all edits applied in memory before any write. File untouched on validation failure.
7. **Pre-rename mtime check** — between the read and the rename, plumb re-stats the file. A change surfaces as a retryable error.
8. **Atomic write + concurrent-write retry** — after the rename, plumb re-stats. A jump in mtime triggers re-read and retry (up to 3 attempts).
9. **Line-change summary in response** — output includes the new mtime and a compact `lines changed: L12-15, L45` summary.

---

### `delete_file`

Delete a single file. Refuses directories — use shell tools for recursive removal.

**Source**: `internal/tools/delete_file.go`

| Field | Required | Description |
|---|---|---|
| `path` | yes | Absolute path or `file://` URI |
| `dirty_ok` | no | Allow deleting a file that has uncommitted changes. Default `false`. |

Sends `FileDeleted` via `workspace/didChangeWatchedFiles`. Symbol cache invalidated. Per-path lock acquired. Rate-limited.

---

### `rename_file`

Move or rename a single file. Distinct from `rename_symbol` (LSP-semantic identifier rename) — this is the filesystem-level operation.

**Source**: `internal/tools/rename_file.go`

| Field | Required | Description |
|---|---|---|
| `from` | yes | Source absolute path or `file://` URI |
| `to` | yes | Destination absolute path or `file://` URI; parent directories created automatically |
| `overwrite` | no | Allow overwriting an existing destination. Default `false` |
| `dirty_ok` | no | Allow moving a file that has uncommitted changes. Default `false`. |

Two-path locking: both source and destination paths are locked in lexical order, deadlock-safe even if two `rename_file` calls swap two files. Sends `FileDeleted` (source) + `FileCreated` (destination) via `workspace/didChangeWatchedFiles`. Symbol cache invalidated for both URIs.

---

### `transaction_apply`

Apply str_replace edits across multiple files atomically.

**Source**: `internal/tools/transaction.go`

| Field | Required | Description |
|---|---|---|
| `dirty_ok` | no | Allow editing files that have uncommitted changes. Default `false` — the transaction is refused if any target file is dirty. |
| `operations` | yes | Array of `{path, edits, expected_mtime?}` (max 50). Each operation's `edits` array uses the same schema as `edit_file`. |

**Three-phase commit:**

1. **Phase 1 — validation.** Acquire per-path locks for every target in lexical order (deadlock-safe). Dirty check: if `dirty_ok` is `false`, all target files are checked for uncommitted changes before validation begins — a single dirty file aborts the transaction with the full list of dirty paths. For each path: stat, read, validate every edit in memory, check `expected_mtime`. If any operation fails validation, NO writes happen.
2. **Phase 2 — write.** Write each prepared content via `safeWrite`. If any write fails partway through the list, already-written files are rolled back to their pre-transaction content via best-effort restoration writes.
3. **Phase 3 — notify.** Per file: fire `FileChanged` via `didChangeWatchedFiles`, invalidate symbol cache for the URI.

Each operation consumes one rate-limit slot — a 10-file transaction counts as 10 writes against the session's per-minute budget.

Use for refactors that must land as one unit: cross-file string rename of a public API, coordinated config + caller updates, etc.

---

---

### `list_directory`

List the immediate contents of a directory.

**Source**: `internal/tools/list_directory.go`

| Field | Required | Description |
|---|---|---|
| `path` | yes | Absolute path or `file://` URI of the directory |
| `pattern` | no | Glob filter applied to entry names (e.g. `*.go`) |
| `include_hidden` | no | Include names starting with `.`. Default `false` |
| `sort_by` | no | `name` (default), `size`, or `modified` |

Returns entries with `[FILE]` or `[DIR]` prefixes, file sizes, and modification times. Non-recursive — use `list_files` or `find_files` for tree traversal.

---

### `list_files`

Recursively walk a directory tree with glob filtering and depth control.

**Source**: `internal/tools/list_files.go`

See tool description for full parameter list.

---

### `search_in_files`

Ripgrep-style content search — regex, smart-case, context lines, glob filter. Respects `.gitignore`.

**Source**: `internal/tools/search_in_files.go`

**Performance guards**

- **Binary detection**: null-byte sniff on the first 8 KB; matched binaries are closed without reading the rest.
- **Size cap**: `max_file_bytes` (default 50 MiB) skips outsized files via `os.Stat` before opening, so logs/dumps/lockfiles can't stall the walk.
- **Glob directory pruning**: a glob with a literal path prefix (e.g. `src/**/*.go`) returns `fs.SkipDir` for sibling subtrees — the walker never descends into them.
- **Wall-clock budget**: a single call inherits the caller's context deadline, or falls back to 30 s, so a runaway walk can't outlive the MCP client's own timeout.

---

### `find_files`

fd-style file finder — glob or regex, extension filter, type filter, depth limit. Respects `.gitignore`.

**Source**: `internal/tools/find_files.go`

**Performance guards**

- **Glob directory pruning**: when a glob pattern (not regex) has a literal path prefix, the walker prunes sibling subtrees before any name matching.
- **Wall-clock budget**: matches `search_in_files` — caller's deadline or a 30 s fallback.

---

### `find_replace`

Text/regex search-and-replace across files. Defaults to `dry_run=true`.

**Source**: `internal/tools/find_replace.go`

| Field | Required | Description |
|---|---|---|
| `path` | yes | Directory to walk, or a single file |
| `pattern` | yes | Search pattern. Plain text by default; regex when `use_regex=true` |
| `replacement` | yes | Replacement text. With regex, supports `$1`, `$2` backreferences |
| `use_regex` | no | Treat `pattern` as a regex. Default `false` |
| `glob` | no | File filter, e.g. `*.go`, `**/*.md`. A literal directory prefix (e.g. `src/**/*.go`) prunes sibling directories from the walk |
| `case_sensitive` | no | Default: smart-case (case-insensitive iff pattern is all lowercase) |
| `dry_run` | no | Default `true` — preview only; set `false` to write |
| `max_files` | no | Cap on files modified. Default `100`; enforced exactly even under parallel scheduling |
| `max_file_bytes` | no | Skip files larger than this. Default `52428800` (50 MiB). Guards against scanning huge logs/dumps that would blow past MCP timeouts |

**Behaviour**

- **Binary detection**: opens each candidate file and sniffs the first 8 KB for a null byte; if found, the file is closed without further reading. Huge binary blobs are never buffered.
- **Parallel**: per-file work runs across `runtime.NumCPU()` workers. Output is sorted by path for stable reporting.
- **gitignore**: honoured via the shared walker.
- **Atomic writes**: each replacement is written to `<path>.tmp` then `os.Rename`'d into place.

---

### `file_diff`

Unified diff between any two files. No git required.

**Source**: `internal/tools/file_diff.go`

---

## Bootstrap and session tools

### `session_start`

Bootstrap tool — call this first in every session.

**Source**: `internal/tools/session_start.go`

Returns in one round-trip:

- Workspace path + detected language
- Current git branch and 3 most recent commits
- First 200 lines of `.plumb/context.md`
- All memory names + descriptions
- 5 most recently-modified files (workspace-relative; skips `.git`/`node_modules`/`vendor`/etc.)
- Top-5 most-used tools from this workspace's stats history
- Active LSP errors and warnings

Designed for Claude Desktop where no filesystem access is available without tool calls. Idempotent.

**Cold-start fallback chain** when the daemon hasn't resolved a workspace yet:

1. Explicit `workspace` argument
2. Daemon's already-resolved workspace
3. `roots/list` query to the MCP client
4. Walk up from `os.Getwd()` looking for a project marker (`go.mod`, `package.json`, `Cargo.toml`, etc.)

| Field | Required | Description |
|---|---|---|
| `workspace` | no | Absolute workspace path. Defaults to the daemon-resolved workspace; falls back to roots/list, then cwd walk |

### `daemon_info`

Returns metadata about the current MCP session and daemon process.

**Source**: `internal/tools/daemon_info.go`

Includes the current session name, session ID, daemon version, daemon start timestamp, and uptime. The session name reflects any successful `rename_session` call in the same MCP session.

### `rename_session`

Renames the current MCP session using the existing session `Name` field.

**Source**: `internal/tools/rename_session.go`

Names are normalised to uppercase, may contain only ASCII letters and `-`, must not start or end with `-`, and are capped at 16 characters to match the generated session-name envelope. A successful rename updates the session JSON, future stats rows, and existing stats rows for the current session in the global stats database.

| Field | Required | Description |
|---|---|---|
| `name` | yes | New session name. Letters and `-` only; stored uppercase |
