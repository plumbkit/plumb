# Waking an idle agent that has mail

Plumb's mailbox delivers by polling only. Every path — the block appended to a
tool result, `check_messages`, `session_start` — needs the recipient to make a
call. An agent that has finished its turn and is waiting on its human makes no
calls at all, so no amount of server-side cleverness reaches it. The daemon
cannot push over MCP; that is a property of the transport, not a gap in plumb.

The client can close it. A Claude Code **Stop hook** fires when the agent
finishes responding, and a Stop hook may keep the turn going. So the shape is:
at end of turn, ask whether mail is waiting, and if it is, continue the turn
with that as the instruction instead of going quiet.

Everything about the Claude Code contract below was verified against
<https://code.claude.com/docs/en/hooks> on 2026-08-13. Everything about plumb
was verified against the source in this repository. Where a piece is missing,
this document says so rather than guessing.

## What plumb does not give you

**There is no plumb CLI subcommand that answers "is there mail for session X".**
`plumb sessions` lists live sessions (id, name, language, folder, adapter, pid,
start time) and nothing about their mailboxes; it has one flag, `--all`, and no
`--json`. No other subcommand touches `collab.db`. Do not write a hook that
shells out to a `plumb mail`-style command — it does not exist.

What would make this recipe a one-liner is a read-only, non-claiming probe:
something like `plumb mail --session <name> --json`, exiting 0 when the mailbox
is empty and 1 when it is not, reading the same rows `PendingNotes` reads
(`internal/collab/store.go`) without touching the `delivered_at` watermark. The
non-claiming part is the whole point — see *Never claim from the hook* below.

Until that exists, a hook has to do two things itself: work out which plumb
session it is, and query `collab.db` directly.

## The Stop hook contract

Verified facts, and the ones a working hook depends on:

- The event name is `Stop`. It runs when the main agent has finished
  responding. It does **not** run on a user interrupt, and an API error fires
  `StopFailure` instead.
- `Stop` has **no matcher support** — it always fires. Omit `matcher`.
- Input arrives on stdin as JSON. Beyond the common fields (`session_id`,
  `transcript_path`, `cwd`, `permission_mode`, `hook_event_name`), Stop hooks
  receive `stop_hook_active`, `last_assistant_message`, `background_tasks`, and
  `session_crons`.
- `stop_hook_active` is `true` when Claude Code is **already continuing as a
  result of a stop hook**. This is the recursion guard, and the docs are
  explicit that you must check it (or process the transcript) to avoid blocking
  on a condition that never resolves.
- Independently of your guard, **Claude Code overrides the hook and ends the
  turn after 8 consecutive blocks**. That is the backstop, not the design.
- To keep the turn going, return one of:
  - `{"hookSpecificOutput": {"hookEventName": "Stop", "additionalContext": "…"}}`
    — non-error feedback. The conversation continues so the agent can act on
    it; the transcript labels it `Stop hook feedback` and no hook-error
    notification is shown. This is the right shape here: unread mail is the
    hook working as designed.
  - `{"decision": "block", "reason": "…"}` — `reason` is required and tells the
    agent why it should continue. It carries the same loop protections, but
    presents as a hook error.
  - Exiting 2 with a message on stderr routes the same way as `reason`.
- Exit 0 with no output allows the stop.

### Settings

Hooks live in `~/.claude/settings.json` (all your projects),
`.claude/settings.json` (one project, committable), or
`.claude/settings.local.json` (one project, not committed).

```json
{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "$HOME/.claude/hooks/plumb-mail-wake.sh",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

`timeout` defaults to 600 seconds for command hooks, which is far too long for
something on the end-of-turn path; set it low. The handler runs in the current
directory with Claude Code's environment.

## Identifying the plumb session

This is the hard half, and the reason the recipe below is conservative.

Plumb writes one JSON file per session under `<data dir>/plumb/sessions/`:
`~/Library/Application Support/plumb/sessions/` on macOS,
`$XDG_DATA_HOME/plumb/sessions/` (default `~/.local/share/plumb/sessions/`)
elsewhere. The fields that matter are `name` (the mailbox address), `folder`
(the pinned workspace) and `ended_at`.

**`ended_at` is serialised even when the session is live**, as the zero time
`"0001-01-01T00:00:00Z"`. A naive truthiness test on that field reports every
session as ended. Compare against the zero value, or against a year prefix.

There is no field that maps a Claude Code conversation to a plumb session by
default. `external_id` would do it — it holds whatever the agent passed as
`session_start`'s `session_id` — but nothing sets it automatically, so in
practice it is empty.

That leaves two options:

1. **Match on `folder` == the hook's `cwd`, and require exactly one match.**
   Simple, no cooperation needed, and correct for the common case of one agent
   per repository. When several live sessions share a folder — which is normal
   when subagents are running — it is ambiguous, and the hook must allow the
   stop rather than guess. Waking the wrong agent is worse than not waking.
2. **Close the loop with a `SessionStart` hook.** `SessionStart` hooks can
   return `hookSpecificOutput.additionalContext`, which lands in the agent's
   context before the first prompt. Have it inject the Claude Code
   `session_id` and an instruction to call
   `session_start({session_id: "<that value>"})`. Plumb persists it as
   `external_id`, and the Stop hook can then match exactly. This is reliable
   but depends on the agent actually making that call.

## Never claim from the hook

Claiming is what marks a message delivered, and plumb guarantees exactly-once
delivery by routing every reader through the same claim. A hook that ran the
claiming query would consume the message outside the agent's context: the
watermark would say delivered while the agent had never seen the text.

So the hook **counts** and never updates, and its feedback must not paste the
message body either. Two reasons: the body stays unclaimed and arrives through
a real delivery path, where it is correctly labelled as another agent's
unverified claim; and hook feedback that carries a peer's free text is a
straight injection channel into your agent's instructions.

Tell the agent that mail is waiting and to call `check_messages`. Nothing more.

## The recipe

`~/.claude/hooks/plumb-mail-wake.sh`, `chmod +x`. Needs `jq` and `sqlite3`.

```bash
#!/usr/bin/env bash
# Stop hook: keep the turn alive when a plumb peer has written to this session.
# Counts unread mail; never claims it. Allows the stop on any ambiguity.
set -euo pipefail

allow() { exit 0; }   # exit 0 with no output = let the turn end

input=$(cat)
[[ "$(jq -r '.stop_hook_active // false' <<<"$input")" == "true" ]] && allow
cwd=$(jq -r '.cwd // empty' <<<"$input")
[[ -n "$cwd" ]] || allow

sessions="${XDG_DATA_HOME:-}"
if [[ -z "$sessions" ]]; then
  case "$(uname -s)" in
    Darwin) sessions="$HOME/Library/Application Support" ;;
    *)      sessions="$HOME/.local/share" ;;
  esac
fi
sessions="$sessions/plumb/sessions"
[[ -d "$sessions" ]] || allow

# Live sessions pinned to this directory. ended_at is present-but-zero while
# live, so test the zero value, not emptiness.
mapfile -t names < <(
  jq -r --arg cwd "$cwd" '
    select((.ended_at // "0001-01-01T00:00:00Z") | startswith("0001-01-01"))
    | select(.folder == $cwd) | .name // empty
  ' "$sessions"/*.json 2>/dev/null | sort -u
)
[[ ${#names[@]} -eq 1 ]] || allow   # none, or ambiguous: do not guess

db="$cwd/.plumb/collab.db"
[[ -f "$db" ]] || allow             # created lazily; absent means never used

now_ns=$(( $(date +%s) * 1000000000 ))
unread=$(sqlite3 -readonly "$db" "
  SELECT COUNT(*) FROM collab_rows
   WHERE kind = 'note'
     AND addressee = '${names[0]}'
     AND delivered_at = 0
     AND expires_at > $now_ns
     AND (target_workspace = '' OR target_workspace = '$cwd');
" 2>/dev/null) || allow
[[ "${unread:-0}" -gt 0 ]] || allow

jq -n --argjson n "$unread" '{
  hookSpecificOutput: {
    hookEventName: "Stop",
    additionalContext: ("You have \($n) unread plumb message(s) from a peer agent. " +
      "Call check_messages to read them before finishing.")
  }
}'
```

### Why it is shaped this way

- **Every failure allows the stop.** A missing database, an unparseable
  session file, an ambiguous folder, a sqlite error — all fall through to
  `allow`. A wake hook that fails closed would strand turns on an unrelated
  fault.
- **`addressee = <name>` only**, never `'next'`. Plumb's own listing path
  (`PendingNotes`) excludes `next` notes deliberately: they are claimed by
  whoever asks first, so advertising one to every candidate would promise a
  message most of them will lose the race for.
- **`expires_at > now`** in nanoseconds, matching the column's unit. Rows are
  filtered by expiry on every read regardless of whether the reaper has run.
- **`target_workspace` scoping** mirrors `ClaimNotes`: a cross-project row is
  claimable only by a session pinned to its target, and a same-project row
  carries none.
- **`stop_hook_active` short-circuits first**, so one wake per continuation
  chain. Blocking again on genuinely new mail would be defensible — the
  condition self-resolves once the agent calls `check_messages`, which claims
  the rows — but this is a hook that runs on every turn of every session, and
  the conservative reading is the one to ship. Claude Code's 8-block cap is a
  backstop for the other choice, not a licence to skip the guard.

### What it does not cover

- **Cross-project mail.** Those rows live in the daemon-level store
  (`collab-xproject.db` in plumb's data directory), readable only by a project
  that set `[collab] cross_project`. The hook above reads the workspace store
  only. Extending it means reading the second database and re-checking that
  opt-in, which is `[collab]` config the hook would have to resolve itself.
- **`sqlite3 -readonly` against a WAL database.** It needs the `-shm` file to
  be present and readable. While the daemon holds the database open this
  holds; a stale or unreadable sidecar surfaces as a sqlite error, which the
  script treats as "no mail" and allows the stop.
- **Subagents.** A subagent's Stop is converted to `SubagentStop`, which takes
  the same decision-control format. The recipe above is not wired for it, and
  the folder-matching heuristic is exactly what breaks when several sessions
  share a workspace.
