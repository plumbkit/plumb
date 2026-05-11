# MCP Tools

Plumb exposes LSP capabilities to LLMs as MCP tools.  Each tool is registered
with the MCP server at startup and appears in the `tools/list` response that
Claude Desktop (or any other MCP client) uses to discover available actions.

Tools are implemented in `internal/tools/` and registered in
`internal/cli/serve.go`.  All three tools cache their results in the
session-scoped `cache.Cache`; entries are invalidated automatically when the
language server reports that a file has changed (`textDocument/publishDiagnostics`).

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
