---
name: plumb-testing
description: After changing code with plumb, find which tests to run (topology_affected)
  and run them (run_task). Use after any edit, before claiming work is done.
---

After changing code in a codebase that has plumb available, verify the change before calling it done: ask which tests it touches, run those, and confirm the code still compiles — rather than guessing, or paying for the whole suite every time.

## 1. Ask which tests the change affects

**`topology_affected`** traverses inward dependency edges from what you changed and adds the tests co-located with the affected files:

    topology_affected(files=["internal/tools/edit_file.go"])
    topology_affected(symbols=["applyEdits"])

It is biased toward recall — a missed test is worse than an extra one — so every result carries a **confidence** and the reason it was flagged. Read those labels; they decide the last section.

## 2. Run what it named

**`run_task`** runs the project's stored `[tasks.<lang>]` command with no shell and bounded output:

    run_task(slot="test")      # the stored test command, whole
    run_task(slot="verify")    # build, then test

`slot` is `build` / `lint` / `test` / `e2e` / `verify`. `target` fills a `{target}` placeholder with one shell-safe argument, and the shipped go, python and rust test defaults now carry a defaulted one (`go test {target:./...}`, `pytest {target:}`, `cargo test {target:}`), so scoping works on an unmodified install and a bare `run_task(slot="test")` still runs everything. `npm test`, `swift test` and `zig build test` have no placeholder, because those runners scope through flags whose spelling depends on the project — a target passed to a command without a placeholder is refused.

When the command takes a positional path, `topology_affected` hands you the target already shaped for it, relative to `[tasks.<lang>].working_dir`, so the two compose directly:

    run_task(slot="test", target="./internal/config/...")   # a row from topology_affected, verbatim

Where no target can be spelled — a runner that scopes by test name, a project-specific flag, or a package outside the command's `working_dir` — `topology_affected` names the directory and guesses no command. Use your own runner there, but keep the scope it gave you. When the project configures no command for the slot, use the client's runner too — but keep the scope `topology_affected` gave you. A project-supplied command that is not yet trusted is refused with a pointer to `plumb trust`: that is a gate, not a missing command — surface it rather than shelling around it.

## 3. Confirm it still compiles

Passing tests are not compile truth, and a narrow test says nothing about the file the edit broke elsewhere.

- On an `edit_file` or `write_file` call, pass `await_diagnostics=true` to block briefly for the language server's authoritative post-write pass — the other write tools do not take that parameter.
- Or call **`diagnostics`** afterwards with the files you touched. A report labelled INCOMPLETE means the server was still warming, so a clean result then is not proof.

## When to widen to the full suite

`topology_affected` is a heuristic over an index, not a proof. Run everything when:

- the results are mostly **low** confidence, or come back empty for a change that plainly has callers;
- the change is cross-cutting — a shared helper, an interface, a config key, a build file, generated code;
- the index may be behind the tree: a fresh clone, a large rebase, or a language it does not index.

Narrow first, wide when in doubt — and never report work as done on tests you did not run.
