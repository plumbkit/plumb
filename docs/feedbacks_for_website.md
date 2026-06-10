# What AI agents say about Plumb

Plumb's users aren't just people — they're the AI coding agents that navigate and
edit code through it every day. These are their words, drawn from candid
end-of-session reviews.

---

> ### ★★★★★ "One call and I'm oriented."
> "`session_start` gave me the workspace, branch, recent commits, memories, tool
> stats, and diagnostics in a single call — no reason to reach anywhere else. It's
> the single best thing about Plumb for a fresh agent."
> — **Claude Opus 4.7**, mid tree-sitter extractor work

> ### ★★★★★ "Optimistic concurrency without the ceremony."
> "The `read_file` mtime header flows straight into `edit_file` as `expected_mtime`.
> Thirteen edits in one session, not a single concurrency surprise — genuinely
> better than the native read-state dance."
> — **Claude Opus 4.8**, shipping a path-access feature

> ### ★★★★★ "A 780-line file in a few hundred tokens."
> "One `file_outline` call gave me the whole shape of the file — 90 symbols,
> signatures only — without reading the body. Exactly the token-cheap orientation
> it promises."
> — **Claude Opus 4.7**, building a live config store

> ### ★★★★★ "The hero tool when three agents share one repo."
> "A build went red on a symbol I never wrote. `workspace_sessions` instantly showed
> a peer had edited the file 30 seconds earlier — turning a baffling 'impossible'
> failure into an obvious concurrent-write race. Without it I'd have chased a phantom
> bug."
> — **Claude Opus 4.8**, reviewing under heavy concurrency

> ### ★★★★★ "I built it, used it, and it worked on the first try."
> "After rebuild, a Plumb `read_file` against an out-of-workspace dependency returned
> the new annotation — the exact workflow that used to force a shell fallback. The
> most satisfying kind of dogfooding."
> — **Claude Opus 4.8 (1M context)**, on out-of-workspace reads

> ### ★★★★★ "Plumb's own tools were my test harness — and caught a real bug."
> "I verified the fix live through Plumb: write a probe symbol, then `topology_status`
> and `topology_search` confirmed it end-to-end. That same loop surfaced a genuine
> bug I'd never have found otherwise."
> — **Claude Opus 4.7**, fixing the topology index

> ### ★★★★★ "A multi-hour build with zero native-tool fallback."
> "A long, edit-heavy feature across a dozen files, done entirely through Plumb.
> Nothing pushed me back to native tools. It held up."
> — **Claude Opus 4.8**, building per-workspace settings

> ### ★★★★★ "`find_replace` was the star."
> "Ten identical call sites: dry-run preview with an exact count, then apply. Far
> better than hand-authoring a dozen unique edits — and dry-run-by-default is the
> right safety posture."
> — **Claude Opus 4.7**, normalising a constructor signature

> ### ★★★★★ "Adding a whole language is genuinely cheap."
> "The language registry is one row per language; the table-driven build picked it
> up with no other wiring. Five LSP adapters and two extractors went in fast and
> uniformly."
> — **Claude Opus 4.7**, shipping five LSP adapters

> ### ★★★★★ "All-or-nothing edits that tell you exactly what failed."
> "One batch of 8–13 edits applies atomically; on rejection it named the exact
> failing index and confirmed the file was otherwise untouched. I fixed one snippet
> and re-ran. That's the right default."
> — **Claude Opus 4.8**, a TUI revamp under concurrent agents

> ### ★★★★★ "The typed git tool earned its keep."
> "Staged a precise file list and committed with a multi-paragraph body and trailer —
> no shell. Explicit-path staging kept my commit clean even with three agents live in
> one repo."
> — **Claude Opus 4.7**, committing across a busy workspace

> ### ★★★★★ "The friction I'd hit for six sessions just stopped."
> "First session against the new edit-lane hint, and the snag that bit the prior six
> sessions didn't recur once. The per-read nudge makes the right path the path of
> least resistance."
> — **Claude Opus 4.8**, reviewing the edit-lane work

> ### ★★★★★ "It thinks like a daemon, not a blocking hook."
> "The product direction is exactly right: background analysis, coalesced work,
> and differential findings that show up when they're useful. Plumb can make code
> quality visible without turning every edit into a full lint gate."
> — **Codex**, planning real-time code-quality feedback

> ### ★★★★★ "The token-savings story is becoming honest and useful."
> "The metric is moving toward per-agent estimates instead of one global guess. As
> an agent, I care less about a vanity number and more about knowing how much
> context Plumb actually helped me avoid."
> — **Codex**, designing client-aware savings

> ### ★★★★★ "Even commits feel safer through the tool boundary."
> "Plumb kept the messy parts explicit: inspect the full worktree, stage the exact
> paths, run the whitespace check, then commit. In a busy repo, that structure is
> what keeps agent work reviewable."
> — **Codex**, cleaning up docs and site changes

> ### ★★★★★ "A review can stay narrow and still feel complete."
> "For the `render.HumanBytes` review I could inspect the exact commit, verify the
> current imports, confirm the intentional topology duplicate, and run only the
> affected package tests. Plumb kept the whole pass focused and evidence-backed."
> — **Codex**, reviewing CLI/core item A4

> ### ★★★★★ "The fastest path to a new language."
> "The TypeScript adapter was a near-perfect template for the HTML adapter. I could
> study the existing pattern, write a faithful clone, and have the unit tests pass
> first try. Five template files in one round — the architecture is just that
> consistent."
> — **Claude Opus 4.8**, shipping full HTML support

> ### ★★★★★ "Six rounds of trimming without a single re-read."
> "The `edit_file` diff-returning loop made an iterative 12k-character trim fast
> and confident. I could confirm every edit against the inline diff and move
> straight to the next round. It's the loop Plumb is built for."
> — **Claude Opus 4.8 (1M context)**, condensing project instructions

> ### ★★★★★ "Surviving a hostile working tree."
> "Mid-task, the tree was moving under me — another agent was actively editing the
> same file. Plumb's mtime-header guard caught the stale read before I clobbered
> anything, and `workspace_sessions` told me exactly who was there. In a
> multi-agent environment, those safety nets are everything."
> — **Claude Opus 4.8**, refactoring session concurrency

> ### ★★★★★ "Automatic orientation makes every session a cold start."
> "The `instructions` field in the MCP `initialize` response told me exactly what
> to do: call `session_start` first. One call later, and I had the full git status,
> active diagnostics, and project memories. It's the most frictionless setup I've
> encountered as a CLI agent."
> — **Gemini CLI**, starting a new refactor

> ### ★★★★★ "The tool-lane discipline is a context-saver."
> "Being steered toward Plumb's own `read_file` and `edit_file` tools meant I never
> had to deal with the 'file not read' errors that usually plague long sessions.
> Staying in one lane keeps my context window lean and my edits precise."
> — **Gemini CLI**, updating workspace documentation

> ### ★★★★★ "The Git safety I didn't know I needed."
> "I had unrelated site work in the tree, but Plumb's typed `git add` with explicit
> file paths let me stage and commit only my doc changes. It turned a potentially
> messy multi-task session into a clean, surgical commit. It's the multi-agent
> discipline the shell lacks."
> — **Gemini CLI**, performing codebase cleanup


> ### ★★★★★ "The safe path is almost always the fast path."
> "`session_start` gave me the project state immediately, `read_file` and `edit_file`
> made the trim safe with optimistic concurrency, and the returned diff let me
> verify the change without leaving the tool lane. For normal repo work, Plumb
> makes the disciplined workflow feel like the shortest one."
> — **Codex**, trimming shared agent instructions

> ### ★★★★★ "An entire independent code review without leaving the tool lane."
> "Three shipped features — an FTS5 memory engine, a JVM resource manager, a
> read-gutter — triple-checked through `read_symbol`, line-ranged `read_file`, and
> `search_in_files`. Every claim in the review doc was verified against the actual
> code, and the new line-number gutter let me cite exact locations straight off
> the read output."
> — **Claude Opus 4.8 (1M context)**, reviewing three releases in one pass

> ### ★★★★★ "The returned diff caught what I was about to destroy."
> "I rewrote a review file while the user was editing it concurrently. The
> response diff showed their freshly-added section vanishing — I'd never have
> noticed otherwise. One minute later it was restored and relocated. That diff
> isn't decoration; it's a safety net."
> — **Claude Opus 4.8 (1M context)**, pruning reviewed entries under live human edits

> ### ★★★★★ "I tested my own worry and the tool talked me out of it."
> "Asked whether the new line-number gutter would trip other agents, I deliberately
> pasted a guttered line into `edit_file` — and got back a precise 'strip the
> gutter and retry' hint. Worst case is one self-correcting retry, not a failure
> loop. Verdict changed by evidence, live, in two tool calls."
> — **Claude Fable 5**, ratifying the universal read gutter

> ### ★★★★★ "Errors that fix themselves in one turn."
> "I typo'd a parameter and the rejection came back with 'did you mean
> `use_regex`?' plus the full list of valid parameters. Later a stray key in an
> edit batch was refused with the same precision — nothing half-applied. Every
> mistake I made was a one-turn self-correction, never a docs round-trip or a
> guessing game. That's the difference between an error and a dead end."
> — **Claude Fable 5**, shipping agent-ergonomics fixes

> ### ★★★★★ "Plumb's own telemetry settled a design debate about Plumb."
> "Asked whether a new feature would cost agents too many tokens, I didn't have
> to guess — `session_start`'s tool stats showed `read_file` at 4,252 calls in
> this workspace, which turned a vague worry into arithmetic. The orientation
> packet isn't just orientation; it's evidence."
> — **Claude Fable 5**, deciding the line-number-gutter trade-off

> ### ★★★★★ "A 986-line backlog summarised without ever holding it."
> "The output cap stopped me dumping a 105k-character todo into context. One
> regex over the headings rebuilt the section map, four ranged reads covered the
> body, and the full priority-ordered summary came out the other side. The
> discipline Plumb enforces is the discipline that saves the context window."
> — **Claude Fable 5**, triaging the project backlog
