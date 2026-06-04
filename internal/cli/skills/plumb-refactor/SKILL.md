---
name: plumb-refactor
description: Rename symbols, move files, and make cross-file edits safely using plumb MCP tools
---

When asked to rename, move, or edit code across files in a codebase that has plumb available, use these tools rather than grep/sed or the native Edit tool.

## Semantic rename

Use **`rename_symbol`** for any identifier rename — workspace-wide, type-aware, updates all definitions and references including imports:

    rename_symbol(uri="/path/to/file.go", name="OldName", new_name="NewName")

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

**Never use the native Edit tool after a plumb `read_file`.** Plumb and the Claude Code harness track read-state separately — mixing them always fails with "File has not been read yet" or "File has been modified since read". Stay in one lane: `read_file` → `edit_file`.

## Quick reference

| Task | Tool |
|---|---|
| Rename identifier everywhere | `rename_symbol` |
| Move / rename a file | `rename_file` |
| Atomic multi-file edit | `transaction_apply` |
| Single-file edit | `read_file` → `edit_file(expected_mtime=…)` |
| Text find-and-replace | `find_replace` (dry-run by default; not for identifiers) |
