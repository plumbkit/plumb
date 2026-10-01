---
name: plumb-git
description: Run git through plumb's policy-gated git tool — what each tier allows, why a subcommand was refused, and which narrower plumb tool to reach for instead of a destructive git command. Use for any git work in a plumb workspace.
---

Plumb's `git` tool is a policy-gated wrapper, not a shell. The subcommand leads the argv, nothing is interpolated by a shell, and eight global flags that could re-target the repository or inject config are refused wherever they appear in `args`: `-c`, `-C`, `--exec-path`, `--git-dir`, `--work-tree`, `--namespace`, `--upload-pack`, `--receive-pack`. The one exception is `switch`, where `-c` / `-C` mean create-branch rather than anything global: they are rewritten to `--create` / `--force-create` before the denylist runs, and the response says so. Prefer this tool over shelling out — the shell path bypasses the policy and plumb's per-repository git lock.

Under a lean tool profile `file_status` and `minimal_diff_review` are not advertised; set `[tools] profile = "full"` in `.plumb/config.toml` to see them.

## The four tiers

| Tier | Subcommands | Gate |
|---|---|---|
| read | `status`, `log`, `diff`, `show`, `blame`, `shortlog`, `check-ignore` | always allowed |
| write | `add`, `commit`, `mv` | `[git] allow_writes` |
| destructive | `reset`, `clean`, `rebase`, `revert`, `cherry-pick` | `[git] allow_destructive` **and** a confirmation |
| network | `push`, `fetch`, `pull` | `[git] allow_push` **and** a confirmation |

`rm` is refused at every tier: delete the file with `delete_file`, then stage the deletion with `add`.

**Seven subcommands are classified by their arguments**, biased towards the safer-to-deny higher tier — so the same subcommand can land in different tiers on different calls. Options are read as git reads them: an abbreviation (`--disc`), a bundle (`-dr`) or a value (`tag -m -d`) counts exactly as git counts it:

- `checkout -b` (branch creation) is **write**; every other `checkout` is **destructive**, since it can discard the working tree or detach HEAD — including `-B` on a branch that already exists (on a new, plain-named branch `-B` given once is a write). Prefer `switch` for a safe branch change.
- `switch` is **write**, but `switch -f` / `--force` / `--discard-changes` and `-C` / `--force-create` on a branch that already exists are **destructive** (on a new, plain-named branch `-C` given once is a write; a name git expands, such as `@{-1}`, is not plain).
- `restore`: with `--staged` (the index only) it is **write**; with `--worktree`, or with no flag at all, it is **destructive**.
- `branch`: `--list` / `-l` / `-a` / `-r` / `--show-current` / `--contains` / `--merged` / `--points-at`, and a bare `branch` (`-v` included), are **read** — a later `--no-list` cancels list mode; creating, `-m` / `--move` / `--copy` and the upstream/description options are **write**; `-d` / `--delete` and every forced form (`-f` / `--force`, `-M`, `-C` — also inside a bundle such as `-qC` — and `-D`) are **destructive** whether or not the branch exists. Unlike `checkout -B`, `switch -C` and `tag -f`, a forced `branch` is never lowered to a write for a new name; to start a branch, use `branch <name>`, `checkout -b` or `switch -c`.
- `tag`: `-l` / `--list` / `-n` / `--contains` / `--merged`, and a bare `tag`, are **read**; creating is **write**; `-d` / `--delete`, and `-f` / `--force` on an existing tag, are **destructive** (on a new, plain-named tag `-f` given once is a write).
- `stash`: `list` / `show` are **read**; a bare `stash` plus `push` / `save` / `pop` / `apply` / `create` / `store` are **write**; `drop` / `clear` are **destructive**; any other sub-subcommand is refused with the permitted list.
- `merge` (`--no-ff`, `--ff-only`, `--no-edit`, `-m <message>`, `<ref>`) is **write**, like `commit`, and runs the merge hooks; `--abort` / `--quit` are **destructive**. `--continue`, `--no-verify`, `-e` / `--edit` and `-F` / `--file` are refused: conclude a merge with `commit` and a message instead. A merge that stops on conflicts names the conflicted files and leaves git's merging state: resolve, `add`, then `commit`.

`session_start` prints the live policy — read it there rather than discovering a tier by being refused.

## Reading history

    git(subcommand="status")
    git(subcommand="log", args=["-10", "--oneline"])

Output is capped — 200 lines for `log` and `blame`, 100 KiB overall — so ask a narrow question instead of paging a whole history.

## Staging and committing

`add` and `commit` are typed rather than pass-through, so the argv cannot be widened into something else. `add` runs `add -A -- <files>` — the `-A` matters: it stages **deletions** of the named paths as well as modifications, which is why removing a tracked file needs no `git rm` (which is refused anyway). `commit` runs `commit -m <message>`, optionally `-- <files>` when you pass `files`, which commits only those paths and ignores unrelated staged changes.

    git(subcommand="add", files=["internal/tools/git.go"])
    git(subcommand="commit", message="fix: bound the diagnostics wait")

Pre-commit hooks always run, so a commit can fail on a hook. Read that output before retrying — re-running unchanged will fail identically.

A hook slower than `[git] detach_after` (45 s by default) makes the call return **still running in the background** instead of a result. That is not a failure: do not retry. Further writes to the repository are refused until it finishes; follow it with `status` or `log -1`, and the next `git` call reports whether it landed (with the SHA) or failed (with the hook's output).

## Destructive and network subcommands

    git(subcommand="restore", args=["internal/tools/git.go"], confirm=true)
    git(subcommand="push", confirm=true)

The confirmation is required on **every** call in those two tiers, not once per session, and `[git] protected_branches` are never force-pushable whatever the rest of the policy says.

No git child ever opens an editor or a credential prompt: plumb runs each with `GIT_EDITOR=true`, `GIT_SEQUENCE_EDITOR=true` and `GIT_TERMINAL_PROMPT=0`, so after resolving a conflict `git(subcommand="rebase", args=["--continue"], confirm=true)` keeps the message git prepared, and `rebase -i` runs its todo list unchanged. A `GIT_EDITOR` under `[git] env` replaces that default.

Check the tier before adding a confirmation. `restore --staged` is a **write**, so it needs `[git] allow_writes` and no confirmation — passing one there is inert, and reaching for it is a sign you have the tier wrong.

## Prefer the narrower tool

Three jobs that reflexively reach for git have a safer plumb answer:

    undo_edit(file_path="/abs/path/internal/tools/git.go")
    file_status(paths=["internal/tools/git.go"])
    minimal_diff_review(mode="changed")

- **`undo_edit`** reverts plumb's own most recent write to one file and refuses if the file changed since — the surgical alternative to a `checkout` of that file, which is destructive-tier and discards everything else too.
- **`file_status`** answers "is this dirty, and who wrote it last?" without reading the file or running a diff.
- **`minimal_diff_review`** reviews the working diff for signs of over-building before you commit it. Advisory only: it never blocks a write, and silence is not proof the change is minimal.

## Cross-session guard and attribution

Before a write/destructive/network op, if a **different** plumb session moved this repo's HEAD/branch since this session last observed it, the op is refused unless re-run with `confirm:true` — the response names the peer session and the old→new refs (movement by this session, an external tool, or an unknown mover adds no friction). `expected_head` pins the exact HEAD commit an op must be at, refusing outright on a mismatch.

With `[git] commit_trailer = true` (default off) every plumb commit is stamped with a `Plumb-Session: <session-name>` trailer; either way, `workspace_sessions` lists recent commits per session (short SHA, subject, repository). With `[collab] intents = true`, a repo-state op (any destructive-tier op, plus `commit`/`switch`/`checkout`/`merge`) also surfaces live peer `share_intent` claims covering this repository — advisory only, never blocks the op, never requires confirmation.

## Working in a nested repository

    git(subcommand="status", repo="/abs/path/to/submodule")

`repo` defaults to the pinned workspace, so a call that omits it in a superproject stages the submodule *pointer*, not the work inside. Pass `repo` pointing into the submodule, with `files` relative to it, to stage and commit there.
