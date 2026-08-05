---
name: plumb-refactor
description: Rename symbols, move files, and make cross-file edits safely using plumb MCP tools
---

When asked to rename, move, or edit code across files in a codebase that has plumb available, use these tools rather than grep/sed or your client's own file-editing tool.

## Semantic rename

Use **`rename_symbol`** for any identifier rename — workspace-wide, type-aware, updates all definitions and references including imports:

    rename_symbol(uri="/path/to/file.go", symbol_name="OldName", new_name="NewName")   # preview
    rename_symbol(uri="/path/to/file.go", symbol_name="OldName", new_name="NewName", dry_run=false)

Only `uri` and `new_name` are required. Identify the symbol by `symbol_name` — plumb resolves the identifier position for you; raw `line`/`character` is the fallback. `dry_run` defaults to **true**, so the first call returns a per-file diff to check and a second call with `dry_run=false` applies it.

Never use `find_replace` or grep+sed for identifier renames — they miss references across files and break imports.

## Atomic cross-file edits

Use **`transaction_apply`** for changes spanning multiple files — validates all edits in memory first, then applies them all-or-nothing:

    transaction_apply(operations=[
      {file_path: "/a.go", edits: [{old_string: "…", new_string: "…"}], expected_mtime: "…"},
      {file_path: "/b.go", edits: [{old_string: "…", new_string: "…"}], expected_mtime: "…"},
    ])

## Moving files

Use **`rename_file`** for file moves — atomic, refuses to overwrite without `overwrite=true`:

    rename_file(from="/old/path.go", to="/new/path.go")

## Single-file edits: read → mtime → edit

1. Call `read_file` and copy the `mtime=` value from the response header.
2. Call `edit_file` with `expected_mtime` set to that value.

**Do not switch to your client's own edit tool after a plumb `read_file`.** Plumb tracks read-state per session and a client with its own read-before-edit tracking keeps a separate record; a read taken in one lane does not satisfy the other, so the edit is refused as unread or stale even though you just read the file. Some clients say so with an error naming a file that "has not been read yet" or "has been modified since read"; others simply bypass plumb's per-path locks, the language-server notification, and `undo_edit`. Stay in one lane: `read_file` → `edit_file`.

## Quick reference

| Task | Tool |
|---|---|
| Rename identifier everywhere | `rename_symbol` |
| Move / rename a file | `rename_file` |
| Atomic multi-file edit | `transaction_apply` |
| Single-file edit | `read_file` → `edit_file(expected_mtime=…)` |
| Text find-and-replace | `find_replace` (dry-run by default; not for identifiers) |
