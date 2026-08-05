---
name: plumb-explore
description: Navigate and understand unfamiliar code using plumb MCP tools
---

When asked to understand, explore, or navigate a codebase that has plumb available, work down this ladder rather than reading whole files:

`workspace_search` → topology / LSP → `search_in_files` → bounded `read_file`

Each rung is broader and cheaper than the one below it. Start at the top and drop down only when the note under the rung applies.

Under a lean tool profile several of the tools named below are not advertised. If one of them is not offered to you, use the nearest alternative on the same rung — for rung 0's opening move that is **`topology_search`**. Rung 3 is the thinnest: both its tools are hidden under lean, so a lean client falls back to its own native search — though lean `read_file` still takes `pattern`, `use_regex`, and `max_matches`, which covers scanning a file you have already located without leaving plumb.

## 0. Ranked discovery — start here

**Before reaching for a shell `grep`, `find`, or `ls`, start on this ladder.** Those three moves are what it replaces: a conceptual question belongs on this rung, `grep` belongs on rung 3's `search_in_files`, and `find`/`ls` belong on `find_files`. Prefer them whenever they are offered to you — they are workspace-bounded, `.gitignore`-aware, and bounded in output, none of which a shell scan is.

One query across every indexed corpus at once — code symbols, doc sections, and project memories — with each hit labelled by corpus, score, and why it matched.

- **`workspace_search`** — the opening move for a conceptual question ("where is daemon locking handled?").

Drop down once a hit gives you a symbol or file worth following — or straight away when you already know the name. This rung is approximate by design and never proof of absence.

## 1. Map (topology) — structure and impact

Topology answers instantly and works even while the language server is warming up.

- **`topology_search`** — ranked symbol/file discovery.
- **`topology_explore`** — neighbourhood around a named symbol (callers, callees, depth=2).
- **`topology_impact`** — blast radius: what would break if this symbol changes.
- **`topology_routes`** — framework entry points (HTTP handlers, CLI commands).
- **`file_outline`** — file shape (signatures, bodies collapsed) in ~200 tokens.
- **`read_symbol`** — source of one named symbol without reading the whole file.

Drop down when you need an exact, type-aware answer that an index can only approximate.

## 2. GPS (LSP) — once you know where to look

LSP gives exact, type-aware answers:

- **`workspace_symbols`** — workspace-wide name search.
- **`get_definition`** — exact definition location (scope-aware, not text search).
- **`find_references`** — all call sites with source lines.
- **`call_hierarchy`** — callers and callees.
- **`type_hierarchy`** — supertypes and subtypes.

Drop down when what you are after is not a symbol at all — a literal string, a config key, a log message — or when the answer has to be exhaustive.

## 3. Exact scan

- **`search_in_files`** — every occurrence in current file contents; the lane for audits, verification, and replacement prep. This is the `grep` replacement.
- **`find_files`** — locate files by name or path pattern, and the directory lister: `pattern` is optional, `max_depth: 1` lists one level like `ls`, and `include_details` adds a `[FILE]`/`[DIR]`/`[LINK]` marker with size and modified time. This is the `find` and `ls` replacement — the name lane, where `search_in_files` is the content lane.

Drop down when matching lines are not enough and you need the surrounding code.

## 4. Read whole files only when about to edit

Use `file_outline` and `read_symbol` to understand; save `read_file` for when you need to make changes. When you do read, copy the `mtime=` value from the response header and pass it as `expected_mtime` to `edit_file`.
