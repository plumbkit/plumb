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

**Six subcommands are classified by their arguments**, biased towards the safer-to-deny higher tier — so the same subcommand can land in different tiers on different calls:

- `checkout -b` / `-B` (branch creation) is **write**; every other `checkout` is **destructive**, since it can discard the working tree or detach HEAD. Prefer `switch` for a safe branch change.
- `switch` is **write**, but `switch -f` / `--force` / `--discard-changes` is **destructive**.
- `restore`: with `--staged` (the index only) it is **write**; with `--worktree`, or with no flag at all, it is **destructive**.
- `branch`: `--list` / `-l` / `-a` / `-r` / `-v` / `--show-current` / `--contains` / `--merged`, and a bare `branch`, are **read**; creating, `-m` / `-M` / `--move` / `--copy` are **write**; `-d` / `-D` / `--delete` is **destructive**.
- `tag`: `-l` / `--list` / `-n` / `--contains` / `--merged`, and a bare `tag`, are **read**; creating is **write**; `-d` / `--delete` is **destructive**.
- `stash`: `list` / `show` are **read**; a bare `stash` plus `push` / `save` / `pop` / `apply` / `create` / `store` are **write**; `drop` / `clear` are **destructive**; any other sub-subcommand is refused with the permitted list.

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

## Destructive and network subcommands

    git(subcommand="restore", args=["internal/tools/git.go"], confirm=true)
    git(subcommand="push", confirm=true)

The confirmation is required on **every** call in those two tiers, not once per session, and `[git] protected_branches` are never force-pushable whatever the rest of the policy says.

Check the tier before adding a confirmation. `restore --staged` is a **write**, so it needs `[git] allow_writes` and no confirmation — passing one there is inert, and reaching for it is a sign you have the tier wrong.

## Prefer the narrower tool

Three jobs that reflexively reach for git have a safer plumb answer:

    undo_edit(file_path="/abs/path/internal/tools/git.go")
    file_status(paths=["internal/tools/git.go"])
    minimal_diff_review(mode="changed")

- **`undo_edit`** reverts plumb's own most recent write to one file and refuses if the file changed since — the surgical alternative to a `checkout` of that file, which is destructive-tier and discards everything else too.
- **`file_status`** answers "is this dirty, and who wrote it last?" without reading the file or running a diff.
- **`minimal_diff_review`** reviews the working diff for signs of over-building before you commit it. Advisory only: it never blocks a write, and silence is not proof the change is minimal.

## Working in a nested repository

    git(subcommand="status", repo="/abs/path/to/submodule")

`repo` defaults to the pinned workspace, so a call that omits it in a superproject stages the submodule *pointer*, not the work inside. Pass `repo` pointing into the submodule, with `files` relative to it, to stage and commit there.
