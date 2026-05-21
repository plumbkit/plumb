# Topology — the semantic index

Topology is plumb's optional, persistent semantic index of your codebase. It
complements the language server: where LSP gives compiler-grade precision after
a startup cost, topology gives instant, broad answers from a local SQLite/FTS5
database — no language-server boot, no per-conversation indexing wait.

Topology is **disabled by default**. Enable it per project or globally:

```toml
[topology]
enabled = true
```

The index lives at `<workspace>/.plumb/topology.db` and is maintained by a
background indexer.

## The dual-engine model

Plumb pairs two engines that handle different phases of an agent's work:

```mermaid
flowchart LR
    Q["Agent question"] --> TOPO["Topology (the Map)<br/>FTS5 search · BFS explore<br/>instant, syntactic"]
    TOPO -->|found where to work| LSP["LSP (the GPS)<br/>rename · diagnostics · references<br/>precise, type-aware"]
    LSP --> EDIT["Safe edit + verify"]
```

- **Topology is the Map.** Use it for discovery: "where is the routing logic?",
  "what's around this symbol?", "what does changing this touch?". It answers
  immediately, tolerates broken code, and has a tiny memory footprint — but it
  is syntactic (Go AST, Python and TypeScript/JS regex extractors), so it offers
  *broad recall*, not compiler-level precision or type resolution.
- **LSP is the GPS.** Once you know *where* to work, the language-server tools
  (`get_definition`, `find_references`, `rename_symbol`, `diagnostics`) make and
  verify changes with full type awareness.

See [Architecture → dual-engine](architecture.md#plumb-topology-vs-lsp-the-dual-engine-architecture)
for how the two fit into the layered design.

## When to use topology vs LSP

| You want to… | Use |
|---|---|
| Find where a concept/feature lives | `topology_search` |
| Understand a symbol's neighbourhood | `topology_explore` |
| Assess the blast radius of a change | `topology_impact` |
| Know which tests a change might affect | `topology_affected` |
| List framework entry points (routes, commands) | `topology_routes` |
| Jump to a definition with certainty | `get_definition` (LSP) |
| Find every real call site | `find_references` (LSP) |
| Rename safely across the workspace | `rename_symbol` (LSP) |
| See compile errors | `diagnostics` (LSP) |

A common flow: `topology_search` to locate → `topology_explore`/`topology_impact`
to scope → LSP tools to read and edit precisely → `diagnostics` to verify.

## The six tools

See [Tools → Topology](tools.md#topology) for full inputs. In brief:

- **`topology_status`** — index health: file/entity counts, DB size, indexed
  languages, last sync, last error.
- **`topology_search`** — FTS5 ranked symbol/file search (`query`, optional
  `kinds`/`language` filters).
- **`topology_explore`** — BFS neighbourhood around a named symbol, with depth,
  node, and byte budgets.
- **`topology_impact`** — bidirectional blast radius: what a symbol depends on,
  and what depends on it.
- **`topology_affected`** — given changed files/symbols, the files and tests
  most likely affected. Use after writing to decide what to run.
- **`topology_routes`** — heuristic, framework-aware entry-point scan (Go HTTP
  handlers, Cobra commands, Python `@app.route`). Results carry a confidence
  annotation.

## Configuration

All `[topology]` fields (see the
[Configuration reference](configuration.md#topology--semantic-index)):

| Field | Default | Effect |
|---|---|---|
| `enabled` | `false` | Turn the index on. |
| `resync_on_attach` | `false` | Full resync each time the workspace attaches. |
| `exclude_patterns` | `[]` | Path globs to skip during indexing. |
| `max_file_size_bytes` | `524288` | Largest file considered (512 KiB). |
| `resync_interval_minutes` | `0` | Periodic full-resync interval; `0` disables. |

## Trade-offs and limitations

- **Syntactic, not semantic.** Topology does not resolve types or follow
  dynamic dispatch. Treat its graph as a strong hint, then confirm with LSP.
- **`topology_routes` is heuristic.** It pattern-matches known frameworks;
  always read the confidence annotation.
- **Per-workspace cost.** The index is a small SQLite file under `.plumb/`. The
  background indexer keeps it fresh as files change.
