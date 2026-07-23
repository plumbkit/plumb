---
name: plumb-minimal-change
description: Prove reuse and minimality with plumb evidence before writing non-trivial code
---

Before writing non-trivial code in a codebase that has plumb available, work through this ladder. Each step is evidence, not vibes — cite what the tools actually returned.

## 1. Trace the flow first

`workspace_search` / `topology_search` / `file_outline` / `read_symbol` the relevant area before editing anything. Understand what's already there.

## 2. Ask if this needs code at all

A doc update, a config change, or an existing flag may satisfy the request. Check before reaching for an editor.

## 3. Search for existing helpers before writing new ones

`workspace_search` and `workspace_symbols` for a function/type that already does this; check memory hints (`relevant_memories`) for prior art or a declined approach.

## 4. For bug fixes, find every caller

`find_references` / `topology_impact` on the broken symbol. Fix the shared root cause once, not each call site separately.

## 5. Prefer what's already there

Stdlib, platform, or an already-installed dependency beats a new dependency or a custom framework.

## 6. Use the smallest edit surface that preserves behaviour

Symbol edits (`replace_symbol_body`, `insert_before_symbol`/`insert_after_symbol`, `move_symbol`) over full-file rewrites. Deletion over new abstraction when it's safe to delete.

## 7. Verify proportionally

`topology_affected` to pick the focused tests; `run_task` for the smallest relevant check. Run the broader `verify` before claiming done only when the change's scope warrants it.

## 8. Name the ceiling of a simplified approach

If you chose a simpler implementation with a known limit, leave a short `plumb:` comment naming the ceiling and the upgrade path — don't let it pass as the general solution.

## Guardrails

Minimum sufficient behaviour is the goal, not code golf. Never cut validation, security checks, accessibility, or tests to shrink a diff. Explicit user intent and correctness always win over minimality.
