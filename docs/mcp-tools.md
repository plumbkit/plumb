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

Search for symbols (functions, types, variables, classes) by name across the
workspace or within a single document.

**Source**: `internal/tools/find_symbol.go`

#### Input schema

```json
{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Symbol name or substring to search for"
    },
    "uri": {
      "type": "string",
      "description": "Limit search to this document (file:// URI). Omit for workspace-wide search."
    }
  },
  "required": ["query"]
}
```

| Field | Required | Description |
|---|---|---|
| `query` | yes | Case-insensitive substring match on symbol names |
| `uri` | no | `file://` URI of a specific document to search within |

#### Behaviour

- **Without `uri`**: calls `workspace/symbol` with `query`. The language server
  does the matching server-side. Results are cached by query string.
- **With `uri`**: calls `textDocument/documentSymbol` to fetch all symbols in
  the document, then filters client-side by case-insensitive substring match on
  the name (including child symbols). The full symbol list is cached by URI;
  filtering is applied on each call without an extra round-trip.

#### Required LSP capabilities

| Method | Capability check |
|---|---|
| `workspace/symbol` | `ServerCapabilities.WorkspaceSymbolProvider.Enabled` |
| `textDocument/documentSymbol` | `ServerCapabilities.DocumentSymbolProvider.Enabled` |

#### Output format

Workspace search (no `uri`):
```
Found 2 symbol(s) matching "Greeter":

- Greeter (Class) at file:///project/main.py:10
- greet (Method) at file:///project/main.py:15
```

Document search (with `uri`):
```
Symbols matching "greet" in file:///project/main.py:

- Greeter (Class) at line 10
- greet (Method) at line 15
```

No results:
```
No symbols found matching "Xyz".
```

#### Cache key

- Workspace: `wsSymbols:<query>`
- Document (symbol list): `<uri>:docSymbols`

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
