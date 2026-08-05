---
name: plumb-memory
description: Search plumb's per-workspace memories before re-deriving a decision or constraint, and write one after learning something a later session would otherwise rediscover. Use when knowledge outlives the current task.
---

Plumb keeps per-workspace memories as markdown under `<workspace>/.plumb/memories/`, indexed for ranked search. They are for knowledge that outlives one session and has no better home in the code — not a scratchpad, and not a substitute for a comment or a doc.

Under a lean tool profile only `search_memories` is advertised; set `[tools] profile = "full"` in `.plumb/config.toml` to get the rest of this lane.

## 1. Look before you derive

Re-deriving a constraint someone already recorded is the failure this lane exists to stop.

    search_memories(pattern="rate limiter")
    relevant_memories(path="internal/tools/edit_file.go")

`search_memories` is ranked FTS5 while the index is fresh and falls back to substring grep otherwise. `relevant_memories` takes the other route in — the memories whose stored `paths` globs match a file you are about to touch, which is the one you want immediately before an edit.

`session_start` already surfaces the top memories and any last-session summary, so re-listing them is usually wasted; reach for these when you need something it did not show.

## 2. Pick the search mode deliberately

    search_memories(pattern="essio", mode="grep")

`mode` is `auto` (the default), `fts`, or `grep`. FTS5 tokenises whole words, so a substring inside an identifier — `essio` within `UserSession` — never matches it. Auto notices an empty FTS result and greps; a literal `fts` keeps the empty answer. Setting `case_sensitive` to true also forces grep, because FTS5 is case-insensitive; leaving it unset keeps the default smart-case behaviour. `use_regex` forces grep too.

## 3. Read the one you want, whole

    list_memories()
    read_memory(name="topology-resync-pacing")

`read_memory` prints a provenance footer for a memory that is not user-authored. Weigh it: `confidence` is one of `user`, `generated`, `imported`, or `inferred`, and only `user` means a person wrote it deliberately. `generated` covers both the daemon's rule-based idle summaries (named `episodic-*`) and anything an agent handed over with `share_findings` (named `finding-*`) — a name prefix, not a tier of its own. Ranking and hint injection already prefer `user`; your reading should too.

## 4. Write when the next session would have to rediscover it

    write_memory(name="topology-resync-pacing", content="…", paths=["internal/topology/**"])

Worth recording: a decision and the option it ruled out, a non-obvious constraint, a footgun with the symptom that betrays it, a convention the tree does not state. Not worth recording: anything a code comment, a doc, or a commit message says better; task state; anything stale by next week.

`paths` is the routing key, not decoration — it drives `relevant_memories` and the hint block plumb appends to path-bearing responses, so a memory written without it is reachable only by search. Write `description` as a claim, not a topic label: it is all a reader sees in a listing.

Generated memories are secret-scrubbed before storage, but one you write is stored as given — never paste credentials into it.

## 5. Hand off mid-session instead of waiting to go idle

    share_findings(summary="…", paths=["internal/topology/**"])

This writes what you have learned as a generated memory now, so a peer session finds it through the ordinary channels instead of waiting for this session to go idle. Gated on `[collab] knowledge_handoff` (off by default), and stored at the lower, agent-authored confidence tier.

## 6. Delete what went false

    delete_memory(name="stale-note")

A memory that has gone false is worse than none: it will be retrieved and believed. Delete it rather than leaving it to be out-argued by a later one.
