---
name: plumb-git
description: Run git through plumb's policy-gated git tool — what each tier allows, why a subcommand was refused, and which narrower plumb tool to reach for instead of a destructive git command. Use for any git work in a plumb workspace.
---

Plumb's `git` tool is a policy-gated wrapper, not a shell. The subcommand leads the argv, nothing is interpolated by a shell, and global flags that could re-target the repository — `-c`, `--git-dir`, `--work-tree` — are rejected outright. Prefer it over shelling out: the shell path bypasses the policy and plumb's per-path write locks.

Under a lean tool profile `file_status` and `minimal_diff_review` are not advertised; set `[tools] profile = "full"` in `.plumb/config.toml` to reach them.

## The four tiers

| Tier | Subcommands | Gate |
|---|---|---|
| read | `status`, `log`, `diff`, `show`, `blame` | always allowed |
| write | `add`, `commit`, `switch`, `branch`, `tag`, `stash` | `[git] allow_writes` |
| destructive | `reset`, `clean`, `checkout`, `restore`, `rebase` | `[git] allow_destructive` **and** a confirmation |
| network | `push`, `fetch`, `pull` | `[git] allow_push` **and** a confirmation |

Classification rounds **up** when a subcommand is ambiguous: `checkout -b` is a write, every other `checkout` is destructive. `session_start` prints the live policy — read it there rather than discovering a tier by being refused.

## Reading history

    git(subcommand="status")
    git(subcommand="log", args=["-10", "--oneline"])

Output is capped — 200 lines for `log` and `blame`, 100 KiB overall — so ask a narrow question instead of paging a whole history.

## Staging and committing

`add` and `commit` are typed rather than pass-through: `add` runs `add -- <files>`, `commit` runs `commit -m <message>`, so the argv cannot be widened into something else.

    git(subcommand="add", files=["internal/tools/git.go"])
    git(subcommand="commit", message="fix: bound the diagnostics wait")

Pre-commit hooks always run, so a commit can fail on a hook. Read that output before retrying — re-running unchanged will fail identically.

## Destructive and network subcommands

    git(subcommand="restore", args=["--staged", "internal/tools/git.go"], confirm=true)
    git(subcommand="push", confirm=true)

The confirmation is required on **every** call in those two tiers, not once per session, and `[git] protected_branches` are never force-pushable whatever the rest of the policy says.

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
