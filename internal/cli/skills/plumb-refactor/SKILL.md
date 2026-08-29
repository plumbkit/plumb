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

Editing several files in the same task no longer needs one `read_file` call each: `read_multiple_files` reads up to 20 paths in a single call, and each file's own per-file header carries the `mtime`/`sha256` `edit_file` needs — the read is recorded per file exactly like `read_file`, so a later `edit_file` on any of them works under strict mode with no re-read.

**Do not switch to your client's own edit tool after a plumb `read_file`.** Plumb tracks read-state per session and a client with its own read-before-edit tracking keeps a separate record; a read taken in one lane does not satisfy the other, so the edit is refused as unread or stale even though you just read the file. Some clients say so with an error naming a file that "has not been read yet" or "has been modified since read"; others simply bypass plumb's per-path locks, the language-server notification, and `undo_edit`. Stay in one lane: `read_file` → `edit_file`.

## Choosing an `edit_file` mode

`edit_file` takes two mutually exclusive request shapes: an `edits` array, or `start_anchor` + `end_anchor` + `new_string`.

Within `edits`, prefer **range mode** (`start_line`/`end_line`, 1-based) over `str_replace` for a big multi-line replacement: `old_string` and anchors are matched character-for-character inside a JSON string, so every quote, backslash, and tab must be escaped, and a large enough edit can fail to serialise before it even reaches plumb. Range mode needs none of that — take the 1-based gutter line numbers from `read_file`/`read_symbol` output and send only `new_string`. `start_line: -1` appends at end of file; `end_line: -1` runs through the last line — the clean way to delete a block or append, no anchor needed.

**Anchor mode** replaces the span between two unique anchors (each must match exactly once, `end_anchor` after `start_anchor`); `include_anchors=true` replaces the whole inclusive span instead of just what's between them. It is character-precise: an anchor quoted *without* its trailing newline joins its line onto `new_string` — a common mistake, and the response flags a removed line break when it happens.

`str_replace` mode is best for a small, unambiguous, single-occurrence change; pass `expected_mtime` whenever a concurrent writer might touch the file (a sole agent editing in a burst may omit it, since the exactly-once match is itself a check).

## Letting the edit refuse to break the build

Pass `fail_on_new_errors=true` on `edit_file` or `write_file` when the edit should not land if it breaks compilation: it implies `await_diagnostics`, and if the language server CONFIRMS the write introduced a new error in the edited file, the write is rolled back — the file left byte-for-byte unchanged — and the diagnostics delta comes back as the error instead of a silent bad write. An unconfirmed check never rolls back, and neither do warnings, pre-existing errors, or breakage elsewhere in the tree, so a clean result is not a green light for the whole file. It is incompatible with `apply_partial` (the all-or-nothing guarantee it makes has no meaning per-edit) and is refused over a 1 MiB file (no snapshot to restore from). Reach for it as the default verify move on a refactor that must keep compiling, not as an occasional extra.

`transaction_apply` takes the same flag, scoped to the whole batch: since a coordinated multi-file change is only correct as a set, confirmed new errors in ANY written file roll back EVERY file in the transaction, not just the one that broke. That is the tool this skill's own table above routes a multi-file refactor to, so it — not just the single-file tools — is where `fail_on_new_errors` belongs on a cross-file rename or move.

## Moving a declaration between files (`move_symbol`)

`move_symbol` relocates a whole top-level declaration (function, method, type, const, or var) — including its leading doc comment by default — from one file to another, atomically: if the destination write fails, the source is rolled back, so the declaration is never duplicated or lost.

Scope is deliberately conservative (v1): source and destination must be in the **same directory/package**. plumb does not rewrite references or imports, so a move that would change the symbol's package or import path — a different directory, or (for Go) a different package clause — is **refused** rather than applied half-correctly; relocate across packages by hand. For Go, it also refuses when source and destination carry different build constraints (`//go:build`, legacy `+build`, or the implicit `_GOOS`/`_GOARCH`/`_test` filename suffixes), since moving a declaration across them would silently change what compiles per platform.

`dry_run` defaults to **true** — check the unified diff before setting `dry_run=false`. Undo is per-file: reverting a move takes two `undo_edit` calls, one per file.

## Quick reference

| Task | Tool |
|---|---|
| Rename identifier everywhere | `rename_symbol` |
| Move / rename a file | `rename_file` |
| Atomic multi-file edit | `transaction_apply` |
| Single-file edit | `read_file` → `edit_file(expected_mtime=…)` |
| Read several files before editing | `read_multiple_files(paths=[…])` (up to 20) |
| Refuse an edit that breaks compilation | `edit_file(fail_on_new_errors=true)` |
| Text find-and-replace | `find_replace` (dry-run by default; not for identifiers) |
