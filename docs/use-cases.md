# Use cases: measured tool comparisons

This page records honest, reproducible measurements of Plumb's tools against the standard tools
a coding agent already has — `grep`/`ripgrep`, whole-file reads, naive find-replace, and "just
run the whole suite". The aim is not to claim Plumb wins everywhere. It doesn't, and the numbers
below say so plainly. The aim is to show *where* a structured, language-aware tool layer helps,
where it is neutral, and where it costs you something, so you can reason about when it earns its
place.

Four honest results up front:

- **Targeted reads are a real token win** — but the size of the win is a property of *your*
  question, not of the tool. Reading one function instead of its file ranges from **33.4×** to
  **2.9×** smaller here, depending on how much of the file that function is.
- **Raw text search is a wash** — Plumb returns the same matches as `ripgrep` at a comparable
  size. Its value there is signal and safety, not fewer tokens.
- **Semantic navigation is a correctness win, not a size one** — asked "what actually uses this?"
  a text search returned **60% non-references**, and it is the *kind* of over-match that matters:
  prose, a doc comment, a string literal, and a different symbol that merely contains the name.
- **Batching reads is a turns win, not a token one** — `read_multiple_files` still costs more
  bytes than reading the same files one at a time (**76 bytes** over, down from a **1.32×** loss
  before PLAN-357), but the gap is now small and honest, not padded by a decorative separator or
  a wrong byte count. It buys one round trip instead of three and, as of that same fix,
  edit-safety parity with `read_file` under strict mode — it no longer trades a batch read for a
  broken next edit.

Writing this page changed the software it measures. Two scenarios turned up real defects — a
test-selection bug that dropped the one test covering the change, and 17% of a response spent on
a decorative line — and the tools were fixed before these numbers were published. Both are
described where they were found, in Scenario 7 and Scenario 8.

## How this was measured

| | |
|---|---|
| Repository | this repo at commit `b46e233f`, plus this page and its script |
| Date | 2026-08-21 |
| Plumb | 0.17.0 (go1.26.7) |
| Client | a direct MCP session over stdio (`plumb serve`) |
| Platform | macOS (arm64) |

Every number on this page is produced by [`scripts/measure-use-cases.py`](../scripts/measure-use-cases.py).
Run it and you get this table back; there are no hand-copied figures.

- **Native numbers are real subprocesses.** `rg`, `/usr/bin/grep`, `wc -c`. The byte count is
  what the command actually wrote to stdout.
- **Plumb numbers are a real MCP session.** The script speaks JSON-RPC to `plumb serve` and
  measures the exact response payload a client would put in its context window.
- Token figures use ~4 characters per token, the same rough average Plumb uses internally. They
  are a conversion of the measurement, not a second measurement.
- These are **measured payload bytes** — distinct from the *estimated* token-efficiency figure
  shown inside Plumb's own stats, which is a heuristic.
- **Read scenarios carry two baselines, because "raw bytes" is a baseline nobody gets.** `wc -c`
  is the obvious native number, but no agent read tool returns a bare file — Claude Code's own
  `Read` prefixes every line with a line number, exactly as Plumb does. Charging Plumb for that
  framing while giving the native side raw bytes understates Plumb in every read scenario at
  once, so both are measured: **raw**, and **with a line-number gutter**. The gutter modelled is
  the cheapest honest one — the line number right aligned to the width of the largest, then a tab,
  which is what Plumb emits. Padding to `cat -n`'s fixed six columns would overcharge the native
  side and flatter Plumb, which is the same error pointing the other way.

> **If you reproduce this, invoke `/usr/bin/grep` by absolute path.** Several agent harnesses
> install a `grep` shim on `PATH` that quietly respects `.gitignore`. Measuring through one turns
> the grep-vs-ripgrep comparison into ripgrep-vs-ripgrep and erases the entire difference this
> page is about. The first draft of these numbers was wrong for exactly that reason: a shimmed
> `grep` reports the same 29 matches `ripgrep` does, where the real one finds 202.

The sample symbol is `FormatSavings` (`internal/stats/savings.go`) and the sample file is
`internal/cli/stats.go` (316 lines, 9,856 bytes).

## Scenario 1 — Searching the project for a symbol

Question: *find every occurrence of `FormatSavings`.*

| Tool | Matching lines | Bytes | ~Tokens | Respects `.gitignore`? | Names the enclosing symbol? |
|---|---|---|---|---|---|
| `/usr/bin/grep -rn FormatSavings .` | 202 | 25,699 | ~6,425 | no | no |
| `rg -n FormatSavings` | 30 | 3,100 | ~775 | yes | no |
| Plumb `search_in_files` | 30 | 2,754 | ~688 | yes | no — **off by default** |
| … with `include_enclosing_symbol: true` | 30 | 3,288 | ~822 | yes | yes, on 14 hits |

**Takeaway — a wash against `ripgrep`, and that's fine.** All three return the same 30 matches.
Plain `search_in_files` is 11% *smaller* than `rg`; turn the annotation on and it is 6.1%
*larger*. Either way it is a wash, and hiding Plumb's search to "save tokens" would save nothing.

The annotation deserves its own row rather than a tick in the plain one, because
`include_enclosing_symbol` **defaults to false** — the enclosing symbol costs an LSP query per
matched file, so you opt in. Quoting the cheap call's byte count beside the expensive call's
feature would describe a call nobody made. Asked for, it labels 14 of the 30 hits with the
function containing them (the other 16 are prose, config and comments that sit inside no
function) — something a text search cannot produce at any price.

Against naive `grep`, none of this is a wash: **6.7× the matching lines and 8.3× the bytes of
`ripgrep`**, all of it noise. Scenario 2 is about where that noise comes from.

## Scenario 2 — The working checkout is not the repository

The gap between `grep` and `ripgrep` above isn't about the tools' cleverness. It is that one of
them searches the repository and the other searches the directory the repository lives in.

| | Bytes |
|---|---|
| Tracked files (`git ls-files`) | 22.6 MB |
| The actual working directory | 1,238.0 MB |

**54.9× more bytes on disk than in the repository.** None of it is exotic: a dependency
directory (`node_modules`), a build output directory, a test cache, release artifacts, the
compiled binary itself, and — the one that really hurts — sibling `git worktree` checkouts
parked inside the repo, which are *full copies of the source*, so every symbol appears in each
of them again.

That last category is why the noise is not merely bulky but actively misleading. A `grep` hit in
a stale worktree copy looks exactly like a hit in your code.

**Takeaway — scoping is a correctness property, not a speed optimisation.** `ripgrep` earns its
place here on its own; Plumb's contribution is that `search_in_files` gives you the same
guarantee without you having to remember to ask for it. The multiplier is machine-local — yours
depends on what you happen to have built lately — so measure your own:

```sh
git ls-files -z | xargs -0 du -ch | tail -1   # the repository
du -sh .                                      # what grep will actually walk
git status --ignored --porcelain -s | grep '^!!'   # and what the difference is made of
```

## Scenario 3 — Reading one function

Question: *I need one function out of `internal/cli/stats.go` (9,856 bytes, ~2,464 tokens).*

| Approach | Function size | Bytes | vs raw file | vs a real read tool |
|---|---|---|---|---|
| Whole file, raw (`wc -c`) | — | 9,856 | 1× | — |
| Whole file, with a line gutter | — | 11,120 | 1.13× | 1× |
| `read_symbol axisCell` | 6 lines | 333 | 29.6× | **33.4× smaller** |
| `read_symbol parseAge` | 16 lines | 737 | 13.4× | **15.1× smaller** |
| `read_symbol runStats` | 117 lines | 3,874 | 2.5× | **2.9× smaller** |

The line counts come from `read_symbol`'s own `# symbol: … lines A–B` header, not from where the
next declaration starts — that shortcut overcounts, because it swallows the trailing blank line
and the following function's doc comment.

**Takeaway — the win is real, and it is a ratio you control.** This is the one place the token
story genuinely lives: addressing code by symbol instead of by file. But quoting a single
headline multiple would be dishonest, because the multiple is just *how much of the file you
didn't need*. Fetch a 6-line helper out of a 316-line file and you save 29.6×. Fetch the 117-line
function that is a third of the file and you save 2.5× — still a win, but a modest one, and if
you then need its neighbours you have spent more than one whole-file read.

Rule of thumb: `read_symbol` pays when you know the symbol you want. When you only know the
rough area, `read_file` with a line range is the same idea; when you need most of the file,
just read the file.

## Scenario 4 — Understanding a file's shape

Question: *what's in `internal/cli/stats.go`?*

| Approach | Bytes | ~Tokens |
|---|---|---|
| Whole file, raw | 9,856 | ~2,464 |
| Whole file, with a line gutter | 11,120 | ~2,780 |
| Plumb `file_outline` | 1,589 | ~397 |

**Takeaway — 6.2× smaller than the raw file, 7.0× smaller than a real read of it.** `file_outline` returns every declaration — signatures with line
ranges, bodies collapsed — for ~397 tokens instead of ~2,464. Enough to navigate the file and
decide what to read in full, without reading it all. Unlike Scenario 3 this ratio is fairly
stable, because it scales with the file's declaration density rather than with your question.

## Scenario 5 — "What actually uses this?"

Question: *what are the real call sites of `FormatSavings`?* This is a different question from
Scenario 1 — not "where does this text appear" but "where is this symbol genuinely referenced".

| Tool | Result | Noise |
|---|---|---|
| `rg -n FormatSavings` | 30 matching lines across 9 files | **18 non-references (60%)** |
| Plumb `find_references` | 14 reference positions on 12 lines across 6 files | **0** |

Note the units differ, and the difference is instructive: a *reference* is a position, not a
line. Two calls on one line are two references but a single `grep` hit. Compared like with like
— distinct file:line pairs — it is 12 real lines against 30 reported ones.

The 18 lines that are **not** references break down as:

- **11** — prose in this very page, which names the symbol constantly.
- **2** — the changelog entry describing the work this page prompted.
- **2** — the measurement script's configuration, which names it as a string.
- **1** — the function's own doc comment.
- **1** — `TestFormatSavings`, a **different symbol** that merely contains the name.
- **1** — a `%q` format string inside a test's failure message.

**Takeaway — precision, not size.** Those five categories are the whole argument. A text search
over-matches on comments, strings, prose and same-named symbols, and can also *miss* a real
reference its pattern didn't anticipate. `find_references` asks the language server for the
references to that specific declaration, so it returns those and nothing else — 13 uses plus the
declaration itself, which it includes by default (`include_declaration`). This is the rebuttal to
Scenario 1: text search is a wash, but *semantic* search is a correctness win.

*Caveat:* `find_references` needs a warm language server. On a cold start, or for a language with
no server configured, Plumb falls back to its tree-sitter index and labels the result
approximate.

## Scenario 6 — Renaming a symbol

Question: *rename `FormatSavings`.* The same over-matching that made Scenario 5 a precision story
becomes a *destruction* story the moment you write instead of read.

| Approach | Would change |
|---|---|
| Word-boundary find-replace over the files `rg` lists | 25 lines across 8 files |
| Plain find-replace (no word boundary) | 30 lines across 9 files |
| Plumb `rename_symbol` (dry run) | 15 edits across 6 files |

The two files a find-replace touches and `rename_symbol` does not are this page and the
measurement script — a documentation corpus and a config constant, silently rewritten. Within
the code files, the naive pass also rewrites the failure-message string literal.

The second row is the one worth pausing on. Anchoring on word boundaries is the careful version,
and it still rewrites 13 lines that are not references; drop the anchors — which is what
`s/FormatSavings/…/g` actually does — and you additionally rename `TestFormatSavings`, a
different function whose name merely contains the symbol's. (On the macOS this page was measured
on, BSD `sed` does not support `\b` at all, so the careful version is not even available without
reaching for `perl` or GNU `sed`.)

`rename_symbol`'s 15 edits are the 14 reference positions **plus the doc comment** — the language
server knows the comment documents the symbol, and a reference search alone would have missed it.
It leaves the string literal, the prose and the config alone.

**Takeaway — this is the safety axis, and it is asymmetric.** A search that over-matches wastes
your attention. A find-replace that over-matches corrupts your repository, and the corruption
lands in files (docs, fixtures, strings) that your compiler will not complain about.

## Scenario 7 — Which tests to run after an edit

Question: *I changed `internal/stats/savings.go`. What do I run?*

| Approach | Scope | Response |
|---|---|---|
| `go test ./...` | 55 packages, 659 test files, 4,367 test functions | — |
| Plumb `topology_affected` | **5 packages**, 2,602 tests | 4,142 B |

The answer is a list of targets you can run, not a list of test names:

```
run these packages (5) — pass each target to run_task(slot:"test", target:…):
  ./internal/stats/...                          53 tests   changed package
  ./internal/tools/...                        1318 tests   imports the changed package
  ./internal/cli/...                           990 tests   imports the changed package
  ./internal/tui/...                           217 tests   imports the changed package
  ./internal/web/...                            24 tests   imports the changed package
```

**Takeaway — 5 packages out of 55, and it tells you why each one.** The actionable unit is the
package, because that is what a test runner takes. `internal/stats` is where the edit landed; the
other four are the packages that import it, which is exactly the set you would have had to work
out by hand.

The target is shaped for *this* workspace, not assumed to be Go: it is emitted only where the
primary language's runner takes a positional path, and it is expressed relative to
`[tasks.<lang>].working_dir`, so it can be handed straight to `run_task` — or to `go test` —
without editing. A workspace whose runner scopes by test name (`cargo test <filter>`) or by a
project-specific flag gets its directories named and no command guessed.

**The honest limit is granularity, not coverage.** Within a reached package, *every* test is
counted — all 1,318 in `internal/tools`, not the handful that touch savings code. Nothing in the
index says which tests exercise which function, because a Go test never lives in the file it
tests. So this narrows the run from 55 packages to 5; it does not narrow 4,367 tests to a
handful, and a tool claiming otherwise would be guessing.

> **This page found a bug here.** Measuring this scenario is what surfaced it. The tool used to
> seed its traversal from every node in the changed file — including one per `import`, named for
> the package it pulls in. `strings` and `stats` are not distinctive names (this index holds 636
> nodes called `strings`), so the lookup landed in unrelated packages and dragged their suites in:
> **1,037 tests across 3 packages, 984 of them false positives, in a 119 KB response**, while
> `cmd/clientsmoke` — which does not import `internal/stats` at all — was reported as affected.
>
> Worse, at the default `max_results` the answer contained **zero** `internal/stats` tests:
> `TestFormatSavings`, the one test covering the changed function, had been pushed out entirely.
> An agent following the advice would not have run the test for its own edit.
>
> Fixing it also exposed that the index had **no cross-file edges whatsoever** — 14,779
> `contains`, 8,002 `calls`, 6,759 `imports`, every one of them inside a single file — so the
> "dependency edge" arm could never fire for test selection. Resolving imports into real
> cross-package edges is what lets `internal/tools`, `internal/tui` and `internal/web` appear
> above; they were previously unreachable. Both are fixed, and the numbers in this table are
> from after the fix.

## Scenario 8 — Reading several files at once

Question: *I need these three files.* Published because it went through two real bugs before
landing at a wash, and the honest number matters more than a flattering one.

| Approach | Bytes | vs a real read tool | Turns |
|---|---|---|---|
| Three native reads, raw file bytes | 1,739 | 0.89× | 3 |
| Three native reads, with line gutters | 1,949 | 1× | 3 |
| Three Plumb `read_file` calls | 2,469 | 1.27× | 3 |
| One Plumb `read_multiple_files` | 2,545 | 1.31× | 1 |

**Takeaway — batching still costs more than reading the same files one at a time: 76 bytes over
three separate `read_file` calls, for one round trip instead of three.** That number moved three
times while this page was being kept honest — including once backwards, on a review round that
caught a real correctness bug in the byte-saving itself:

- **691 → 109 bytes** (PLAN-13-era fix). The batching-specific framing used to include three
  horizontal-rule separators — `strings.Repeat("─", 60)`, and U+2500 is *three* bytes in UTF-8, so
  each rule cost 180 — seventeen percent of the response spent on a decorative line the `###`
  heading already made redundant. Removing it, and a byte count that was simply wrong (it
  reported the length of the *rendered* response, header and gutters included, not the file — a
  677-byte file was announced as "933 bytes" one line above its own header reading
  `chars=675 baseline=677`), took the batching overhead from 691 bytes to 109.
- **109 → 7 bytes, then corrected to 76** (PLAN-357). While wiring `read_multiple_files` up to
  parity with `read_file`, its per-file header was first rebuilt to drop `chars`/`baseline`
  entirely, on the theory that they "describe the same whole-file read three ways" as `mtime`/
  `sha256`. True for an *unranged* read — false for a ranged one, which is exactly the case the
  same PR's slicing feature added: a windowed batch read still returns `lines=2`, and without
  `baseline` there is no signal left in the response that those 2 lines are a slice of a
  2,000-line file rather than the whole thing. Independent review caught it before merge; both
  fields are back on every per-file header, unconditionally, matching `read_file`. What *did*
  survive is the indent convention moving into a single preamble line, when at least 3
  successfully-read files agree on one (below that the line costs more than the per-file
  `indent=` it would remove) — a mixed-language batch, the case measured here, usually doesn't
  agree at all, so this batch's own response states each file's indent individually. A
  pinned-workspace-root preamble line was tried too and **measured out**: no per-file header has
  ever stated the workspace root, so hoisting it into a preamble removes nothing — it is pure
  added content regardless of path length, and would have made the batch response *bigger* for
  zero offsetting saving.

> **This scenario also found a correctness bug, not just a bytes one.** `read_multiple_files`
> built its inner reader with no `ReadTracker` wired in, so a batch read was never recorded —
> under `[edits] strict` mode, `edit_file` on a file you had just batch-read failed with "has not
> been read in this daemon session". Batching was strictly worse than reading the same files one
> at a time, on top of costing more bytes. Fixed alongside the bytes (PLAN-357); see the
> `Fixed` entry in `CHANGELOG.md`.

There is still no BYTES argument *for* `read_multiple_files` — three individual `read_file` calls
are smaller, by 76 bytes on this sample — but the gap is no longer 1.32× and no longer padded by a
decorative separator or a wrong byte count. What it buys is real and unmeasured in bytes: one agent
turn instead of three (latency, fewer chances to be interrupted mid-sequence), inline per-file
errors so one unreadable path doesn't abort the batch, and now edit-safety parity with `read_file`
under strict mode. If your agent budget is turns rather than raw bytes, batch.

## Scenario 9 — Latency, not just bytes

Bytes are only half of what a tool costs; the other half is how long the agent waits. Measured
against an already-warm daemon, 200 consecutive `read_file` calls over stdio:

| | Milliseconds |
|---|---|
| p50 | ~0.45–0.55 |
| p95 | ~0.5–0.9 |
| max | ~1–2 |

Deliberately quoted as ranges. Five repeat runs of the same 200 calls on the same machine put p95
anywhere between 0.53 and 0.87 ms, so publishing two decimal places would be false precision
about a figure that is really "well under a millisecond".

**Takeaway — sub-millisecond, so it isn't the thing to optimise.** A warm Plumb read is not
meaningfully slower than a native one; both are lost in the noise next to a model round trip.

The number that is *not* sub-millisecond is the first call against a cold language server, which
can take seconds while the server indexes — that is the language server booting, not the tool,
and it is why the semantic tools (Scenario 5, Scenario 6) label their results approximate until
the server is ready. Warm-path latency is the honest steady-state figure; cold start is a
one-off you pay per workspace.

## Scenario 10 — The same story in another language

Plumb's structural tools run off a tree-sitter index, so they do not need a configured language
server to answer "what's in this file" or "show me this function". The same two measurements,
outside Go:

Ratios below are against a real read tool (gutter included), with the raw-bytes ratio in
parentheses — the same two baselines as Scenario 3.

| Language | File (raw / gutter) | Symbol read | `file_outline` |
|---|---|---|---|
| Go | `internal/cli/stats.go` — 9,856 / 11,120 B | `parseAge`, 16 lines — 737 B, **15.1×** (13.4×) | 1,589 B — **7.0×** (6.2×) |
| Python | `scripts/build-blog.py` — 16,930 / 18,266 B | `load_posts`, 19 lines — 1,253 B, **14.6×** (13.5×) | 2,457 B — **7.4×** (6.9×) |
| JavaScript | `charts.js` — 11,371 / 12,599 B | `activityCalendar`, 28 lines — 1,258 B, **10.0×** (9.0×) | 1,752 B — **7.2×** (6.5×) |

The three symbols are named, with their sizes, because Scenario 3 showed the ratio is a property
of the symbol: a cross-language table pitting a 6-line helper against a 117-line function would
be measuring symbol size, not language. These three span 16–28 lines — close enough to compare,
but not identical, and the Go sample is the smallest of the three, which is part of why its
ratio sits slightly below Python's.

**Takeaway — the shape holds across languages.** The ratios land in the same band, which is what
you'd expect given the win comes from addressing code structurally rather than from anything
Go-specific. The caveat from Scenario 3 travels too: these are per-symbol ratios, and yours will
depend on the symbol.

## What the numbers say

| Question | Plumb tool | Result |
|---|---|---|
| Read one function | `read_symbol` | 2.9×–33.4× fewer tokens, depending on the symbol |
| Understand a file | `file_outline` | ~7.0× fewer tokens (7.4× Python, 7.2× JS) |
| Find text | `search_in_files` | a wash vs `ripgrep`; 9.3× smaller than naive `grep` |
| Find references | `find_references` | exact vs 60% noise — a correctness win |
| Rename a symbol | `rename_symbol` | 15 scoped edits vs 25–30 blind ones — a safety win |
| Pick tests to run | `topology_affected` | 5 packages instead of 55, in 4.1 KB — package-granular |
| Read several files | `read_multiple_files` | 1.31× (76 B over 3× `read_file`); buys turns, not tokens |
| Any warm call | — | p95 well under 1 ms — not the bottleneck |

The token-efficiency win is concentrated in **targeted reads**, and it scales with how much of
the file you didn't need. Raw text search is **neutral**. **Semantic** navigation is a
**correctness** win. Writing tools (`rename_symbol`) are a **safety** win, which is the axis that
actually matters, because a wrong edit costs more than a large one. And at least one tool costs
you tokens outright and is kept for other reasons.

That spread — wins, washes, and a loss — is why Plumb keeps its read and search tools
first-class rather than treating them as redundant with an agent's native ones: "the agent
already has a search tool" is not the same as "the agent's search answers this question as
well".

## Reproduce it yourself

```sh
make build                            # the script drives the binary it measures
python3 scripts/measure-use-cases.py
```

All of the *code* measured here is at `b46e233f`; the only things on top of it are this page and
the script itself. Don't check that commit out to reproduce — the script doesn't exist there yet.
Run it on the commit that added it, or on whatever you have, and compare shapes rather than
digits.

Add `--json` for machine-readable output, or `--dump DIR` to write out every measured tool
payload so a surprising number can be checked against the response it came from.

Two things will make your numbers differ, both expected:

- **Scenario 2 is machine-local.** Its multiplier depends on what you have built, installed and
  left lying around. The shape — "the checkout is much bigger than the repository" — is the
  claim; the number is an illustration.
- **This page is itself in the search results.** It names `FormatSavings` many times, so
  measuring on a later commit — or after editing this page — moves Scenarios 1, 5 and 6. That is
  Scenario 5's point happening live: text search counts every mention, `find_references` counts
  none of them.

Re-run on a later commit and the absolute numbers will drift. The shape should not; if it does,
that is worth knowing, which is what the script is for.
