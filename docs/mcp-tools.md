# MCP Tools

> Full catalogue of tool inputs, outputs, and required LSP capabilities to be written in Step 9.

## Planned tools (v0)

| Tool | LSP methods | Notes |
|---|---|---|
| `find_symbol` | `workspace/symbol` | cached by query |
| `get_definition` | `textDocument/definition` | returns def + surrounding context |
| `explain_symbol` | `definition` + `hover` + `references` | composite |
| `rename` | `textDocument/prepareRename`, `textDocument/rename` | patch-based |
