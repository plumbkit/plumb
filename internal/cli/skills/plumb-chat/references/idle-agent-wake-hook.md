# Catching mail before an agent goes quiet

Plumb's mailbox delivers by polling. Every path — the block appended to a tool
result, `check_messages`, `session_start` — needs the recipient to make a call.
An agent that has finished its turn and is waiting on its human makes no calls
at all, so no amount of server-side cleverness reaches it over MCP.

**Plumb ships the fix as a command:**

    plumb setup claude-code      # if plumb is not registered yet
    plumb hooks install claude-code

That installs two hooks in `~/.claude/settings.json` — merging with whatever is
already there, backing the file up first. `plumb hooks` reports what is
installed; `plumb hooks uninstall claude-code` takes them back out. The rest of
this note is what they do and why, for anyone who needs to reason about the
behaviour, tune it, or build the same thing for a client plumb does not cover.

## What the installed hooks do

**`SessionStart`** states this conversation's id, so the agent's first
`session_start` can pass it as `session_id`. Without that linkage plumb knows a
working directory and nothing else, and a directory shared by several sessions —
which is exactly when peers message each other — cannot name one of them.

**`Stop`** is a background watcher, and it genuinely wakes an idle session. With
`"async": true, "asyncRewake": true` the client queues a task notification when
the hook exits 2, and that notification reaches a session with **no turn in
flight** (verified on Claude Code 2.1.233). So the watcher outlives the turn that
started it: it polls for up to `PLUMB_WAKE_WINDOW` seconds (default 300, every
`PLUMB_WAKE_INTERVAL`, default 7) and fires the moment mail arrives. If you
raise the window, re-run `plumb hooks install claude-code` — the handler's own
timeout is written from the window in effect at install time, and a client that
cancels the hook early kills the watcher with nothing to see.

What it deliberately does not do:

- **It does not deliver.** The wake carries a count, never a body — a peer's
  text pasted into hook feedback would be a direct injection channel into the
  agent. The messages stay unclaimed and arrive through `check_messages`,
  labelled as the unverified claims they are.
- **It does not cover the gaps between windows.** Mail that arrives after the
  watcher's window closes waits for the human, as before.
- **It does not run everywhere.** `settings.json` is user-wide, so the hook
  stands down immediately for a session whose working directory is not inside a
  plumb workspace.
- **It does not loop.** A woken turn re-arms only when it provably read some of
  the mail, capped per chain, so an ignored wake cannot chain.

Which means the `plumb-chat` rule stands unchanged: **silence is still not a
refusal**. Do not read "the wake hook is installed" as "my peer has seen this" —
and note that a peer on a client with no wake path (Codex's `Stop` hook can only
check as a turn ends; most clients have nothing) never had one to begin with.

For an [agent team](https://code.claude.com/docs/en/agent-teams), the event that
covers the idle case natively is **`TeammateIdle`** — "when a teammate is about
to go idle after finishing its turn" — where exit code 2 prevents the teammate
going idle so it continues working. It takes no matcher and fires every time.
That is the right hook for a teammate; plumb does not install it.

## Building it by hand

Everything below is the hand-rolled version of what `plumb hooks install` now
does for you — useful for a client plumb has no pack for, or to understand the
shape. The Claude Code contract was verified against
<https://code.claude.com/docs/en/hooks> on 2026-08-13. Everything about plumb
was verified against the source in this repository.

Note that the Stop hook
sketched below is the **synchronous** form: it keeps a turn alive when mail is
already waiting, but it cannot wake a session that has already gone quiet — for
that it needs the `async` + `asyncRewake` pair the installed hook uses.
## The two commands this needs

**`plumb mail`** answers the question from outside a session:

    plumb mail --external-id "$session_id" --json
    → {"session":"quiet-mesa","workspace":"/repo","count":2,"ages_seconds":[240,30]}

It is strictly read-only and **never claims**: the messages stay undelivered and
reach the agent through `check_messages` as usual. It reports a count and the
ages of what is waiting — never bodies, senders or conversation ids, because a
hook's question is "is there something", not "what is it". Exit status is 0
whether or not mail is waiting and non-zero only on error, so read the count
rather than the exit code.

It names one session three ways: `--session <name>`, `--external-id <id>`, or
`--workspace <dir>`. Which one you can use is the other half of the problem.

**`session_start`** takes a `session_id` parameter, and plumb stores it on the
session as `external_id`. That is what turns a client's own conversation id into
a selector. Nothing populates it automatically, so the setup below does.

## Part 1 — bind the conversation to the plumb session

Without this, a hook knows its conversation id and its working directory, and
plumb knows neither. `--workspace` is the fallback: it takes any directory
inside a session's workspace — `cwd` works, it need not be the root — and
resolves to the nearest enclosing one. But it can only answer when a single
session holds that root, and where several agents share a repository, which is
exactly when peers message each other, it refuses rather than picking one.

A `SessionStart` hook can put the id in front of the agent. `SessionStart`
returns `hookSpecificOutput.additionalContext`, a string added to the agent's
context before the first prompt; plain stdout also reaches the agent for this
event, so a context-only hook needs no JSON at all.

```json
{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/Users/you/.claude/hooks/plumb-session-link.sh",
            "timeout": 5
          }
        ]
      }
    ]
  }
}
```

The matcher is omitted so it fires on every start reason — `startup`, `resume`,
`clear`, `compact` and `fork` all rebuild the context this text belongs in. Use
an absolute path, or `${CLAUDE_PROJECT_DIR}/.claude/hooks/…` for a hook
committed with a project; those placeholders are the documented way to reference
a script independently of the working directory.

`plumb-session-link.sh`:

```bash
#!/usr/bin/env bash
# SessionStart hook: state this conversation's id, so the agent can record it
# on its plumb session. Plain stdout reaches the agent for this event.
set -euo pipefail
sid=$(jq -r '.session_id // empty')
[[ -n "$sid" ]] || exit 0
printf 'plumb session linkage: this conversation has id %s. The plumb session_start tool records it via its session_id parameter, which is what lets tooling map this conversation to its plumb session: session_start({session_id: "%s"}).\n' "$sid" "$sid"
```

Note the phrasing. The docs are explicit that `additionalContext` should read as
factual statements rather than out-of-band system instructions — text framed as
a command can trip the agent's prompt-injection defences and be surfaced to the
user instead of acted on. So the hook states the id and what the parameter is
for, and the standing instruction to pass it belongs in `CLAUDE.md`:

> When calling plumb's `session_start`, pass the conversation id reported by the
> SessionStart hook as `session_id`. It links this conversation to its plumb
> session so `plumb mail` can find it.

If you would rather not depend on the agent making that call, skip Part 1 and
accept `--workspace`: it works whenever one session is pinned to the directory,
and reports ambiguity rather than guessing when several are.

## Part 2 — the Stop hook

Verified facts the hook depends on:

- The event is `Stop`. It runs when the main agent has finished responding, does
  **not** run on a user interrupt, and an API error fires `StopFailure` instead.
- `Stop` has **no matcher support** — it always fires. Omit `matcher`.
- Input arrives on stdin as JSON: the common fields (`session_id`,
  `transcript_path`, `cwd`, `permission_mode`, `hook_event_name`) plus
  `stop_hook_active`, `last_assistant_message`, `background_tasks` and
  `session_crons`.
- `stop_hook_active` is `true` when Claude Code is **already continuing as a
  result of a stop hook**. This is the recursion guard, and the docs say to
  check it (or process the transcript) to avoid blocking on a condition that
  never resolves.
- Independently, **Claude Code ends the turn after 8 consecutive blocks**. That
  is the backstop, not the design.
- To keep the turn going, return `hookSpecificOutput.additionalContext` with
  `hookEventName: "Stop"` — non-error feedback, shown in the transcript as
  `Stop hook feedback` with no hook-error notification. `{"decision": "block",
  "reason": "…"}` does the same with the same loop protections but presents as
  an error, and exiting 2 with stderr routes like `reason`. Unread mail is the
  hook working as designed, so `additionalContext` is the right shape.
- Exit 0 with no output allows the stop.

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
            "command": "/Users/you/.claude/hooks/plumb-mail-wake.sh",
            "timeout": 10
          }
        ]
      }
    ]
  }
}
```

`timeout` defaults to 600 seconds for command hooks, which is far too long for
the end-of-turn path; set it low.

`plumb-mail-wake.sh`, `chmod +x`. Needs `jq`, and `plumb` on the `PATH` the hook
inherits from Claude Code.

```bash
#!/usr/bin/env bash
# Stop hook: keep the turn alive when a plumb peer has written to this session.
# Asks `plumb mail`, which never claims. Allows the stop on any ambiguity.
set -euo pipefail

allow() { exit 0; }   # exit 0 with no output = let the turn end

input=$(cat)
if [[ "$(jq -r '.stop_hook_active // false' <<<"$input")" == "true" ]]; then
  allow                                  # already continuing because of a hook
fi

sid=$(jq -r '.session_id // empty' <<<"$input")
cwd=$(jq -r '.cwd // empty' <<<"$input")

# Exact first, directory second. Each failure mode - no linkage, no session,
# several sessions in one directory - is a non-zero exit, and falls through.
report=""
if [[ -n "$sid" ]]; then
  report=$(plumb mail --external-id "$sid" --json 2>/dev/null) || report=""
fi
if [[ -z "$report" && -n "$cwd" ]]; then
  report=$(plumb mail --workspace "$cwd" --json 2>/dev/null) || report=""
fi
[[ -n "$report" ]] || allow

count=$(jq -r '.count // 0' <<<"$report")
[[ "$count" -gt 0 ]] || allow

jq -n --argjson n "$count" '{
  hookSpecificOutput: {
    hookEventName: "Stop",
    additionalContext: ("You have \($n) unread plumb message(s) from a peer agent. " +
      "Call check_messages to read them before finishing.")
  }
}'
```

## Why it is shaped this way

- **Every failure allows the stop.** No linkage, no matching session, an
  ambiguous directory, a daemon that is down — all fall through to `allow`. A
  wake hook that failed closed would strand turns on an unrelated fault.
- **It asks for a count and acts on a count.** `plumb mail` will not report a
  body, and the hook would not want one: text a peer wrote, pasted into hook
  feedback, is a direct injection channel into the agent the mailbox otherwise
  labels these as unverified claims for. The body stays unclaimed and arrives
  through a real delivery path, in context, correctly labelled.
- **`stop_hook_active` short-circuits first**, so one wake per continuation
  chain. Blocking again on genuinely new mail would be defensible — the
  condition self-resolves once the agent calls `check_messages`, which claims
  the rows — but this runs on every turn of every session, and the conservative
  reading is the one to ship. The 8-block cap is a backstop for the other
  choice, not a licence to skip the guard. If one wake per chain is too blunt —
  a back-and-forth exchange stalls after the first message — the safe
  refinement is re-arm-on-consumption: when the woken turn's `Stop` runs under
  `stop_hook_active`, probe again and re-arm only if the pending count DROPPED
  since the wake (the turn read its mail), never on an unconsumed wake. Stamp a
  chain counter and cap the total wakes per chain at something small (10 works)
  as defence in depth, reset it on any non-woken turn end, and treat every
  ambiguous reading — no stamp, no drop, a failed probe — as "not consumed". A
  count drop is strong evidence of consumption, not proof: a note expiring
  mid-turn, or a peer winning the claim race on a `"next"` note, drops the
  count too and buys one duplicate wake before the chain stands down.
- **`ages_seconds` is there if you want a staleness rule.** Messages expire
  after `[collab] intent_ttl_minutes` (default 120), and one a few minutes from
  expiry may not be worth an interruption. `ages_seconds[0]` is the oldest.

## What it does not cover

- **Cross-project mail.** Those rows live in the daemon-level store, readable
  only by a project that set `[collab] cross_project` (off by default).
  `plumb mail` reads the workspace mailbox only, so a cross-project message
  will not wake anyone.
- **A session's own notes.** `plumb mail` counts `"next"` notes (the default
  addressing) for every session except the one that wrote them: the probe
  claims nothing, so there is no race to lose, but a session can never claim
  its own note, so counting it would wake the one agent with nothing to
  collect. Several idle sessions may still wake for the same `"next"` note —
  all but one lose the claim race and go quiet on their next probe.
- **Subagents.** A subagent's `Stop` is converted to `SubagentStop`, which takes
  the same decision-control format. The recipe is not wired for it.
