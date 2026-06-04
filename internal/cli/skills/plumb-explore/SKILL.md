---
name: plumb-explore
description: Navigate and understand unfamiliar code using plumb MCP tools
---

When asked to understand, explore, or navigate a codebase that has plumb available, follow this sequence rather than reading whole files.

## 1. Map (topology) — start here

Topology answers instantly and works even while the language server is warming up.

- **`topology_search`** — ranked symbol/file discovery. Use this before `grep` or `find`.
- **`topology_explore`** — neighbourhood around a named symbol (callers, callees, depth=2).
- **`topology_impact`** — blast radius: what would break if this symbol changes.
- **`file_outline`** — file shape (signatures, bodies collapsed) in ~200 tokens. Use before `read_file`.
- **`read_symbol`** — source of one named symbol without reading the whole file.

## 2. GPS (LSP) — once you know where to look

LSP gives exact, type-aware answers:

- **`workspace_symbols`** — workspace-wide name search.
- **`get_definition`** — exact definition location (scope-aware, not text search).
- **`find_references`** — all call sites with source lines.
- **`call_hierarchy`** — callers and callees.
- **`type_hierarchy`** — supertypes and subtypes.

## 3. Read whole files only when about to edit

Use `file_outline` and `read_symbol` to understand; save `read_file` for when you need to make changes. When you do read, copy the `mtime=` value from the response header and pass it as `expected_mtime` to `edit_file`.
