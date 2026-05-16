package mcp

// DefaultInstructions is returned in the MCP initialize response's
// "instructions" field. Per the MCP spec, clients surface this as a
// system-prompt-style hint to the model.
//
// Keep this short — it competes for context budget with the user's prompt.
// 300–500 tokens is the target; tool descriptions carry per-tool docs.
const DefaultInstructions = `You have access to plumb, an MCP server that provides LSP-backed and filesystem tools for navigating and editing source code.

Before making any other tool calls, call session_start with no arguments. It returns the workspace root, language, current git branch, recent commits, recently-modified files, project memories, top tool statistics, and any active diagnostics — your orientation packet.

If session_start reports the workspace as resolving or empty, the project has no .plumb/ marker. Ask the user to run "plumb init" in the project root, or — if you have write authorisation — create .plumb/ yourself.

Prefer symbol-aware tools (find_symbol, list_symbols, get_definition, find_references) over read_file when you only need to understand structure. Read entire files only when you need to edit them.

Check the diagnostics in the orientation packet and after every write — they show compile errors, type errors, and warnings from the language server.`
