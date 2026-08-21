# Use cases: measured tool comparisons

This page records honest, reproducible measurements of Plumb's tools against the standard tools
a coding agent already has — `grep`/`ripgrep`, whole-file reads, naive find-replace, and "just
run the whole suite". The aim is not to claim Plumb wins everywhere. It doesn't, and the numbers
below say so plainly. The aim is to show *where* a structured, language-aware tool layer helps,
where it is neutral, and where it costs you something, so you can reason about when it earns its
place.

Four honest results up front:

- **Targeted reads are a real token win** — but the size of the win is a property of *your*
  question, not of the tool. Reading one function instead of its file ranges from **29.6×** to
  **2.5×** smaller here, depending on how much of the file that function is.
- **Raw text search is a wash** — Plumb returns the same matches as `ripgrep` at a comparable
  size. Its value there is signal and safety, not fewer tokens.
- **Semantic navigation is a correctness win, not a size one** — asked "what actually uses this?"
  a text search returned **57% non-references**, and it is the *kind* of over-match that matters:
  prose, a doc comment, a string literal, and a different symbol that merely contains the name.
- **Batching reads is not a token win at all** — `read_multiple_files` returned **1.82× more**
  bytes than reading the same three files natively. It buys turns and atomicity, and it costs
  you payload. That one is published because it is a loss — and because decomposing it showed
  most of the overhead is Plumb's read framing, not the batching.

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
- **The comparison is deliberately unkind to Plumb.** "Native read" is the raw file, counted with
  `wc -c`. Plumb's number is its full response, which carries a line-number gutter and a
  provenance header — about 1.42× the raw bytes (measured in Scenario 8). Real agent read tools
  add line numbers too, so charging that framing to Plumb and not to the native side understates
  Plumb's margin rather than inflating it.

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
| `/usr/bin/grep -rn FormatSavings .` | 201 | 25,561 | ~6,390 | no | no |
| `rg -n FormatSavings` | 28 | 2,921 | ~730 | yes | no |
| Plumb `search_in_files` | 28 | 2,579 | ~645 | yes | no — **off by default** |
| … with `include_enclosing_symbol: true` | 28 | 3,113 | ~778 | yes | yes, on 14 hits |

**Takeaway — a wash against `ripgrep`, and that's fine.** All three return the same 28 matches.
Plain `search_in_files` is 12% *smaller* than `rg`; turn the annotation on and it is 6.6%
*larger*. Either way it is a wash, and hiding Plumb's search to "save tokens" would save nothing.

The annotation deserves its own row rather than a tick in the plain one, because
`include_enclosing_symbol` **defaults to false** — the enclosing symbol costs an LSP query per
matched file, so you opt in. Quoting the cheap call's byte count beside the expensive call's
feature would describe a call nobody made. Asked for, it labels 14 of the 28 hits with the
function containing them (the other 14 are prose, config and comments that sit inside no
function) — something a text search cannot produce at any price.

Against naive `grep`, none of this is a wash: **7× the matching lines and 8.75× the bytes of
`ripgrep`**, all of it noise. Scenario 2 is about where that noise comes from.

## Scenario 2 — The working checkout is not the repository

The gap between `grep` and `ripgrep` above isn't about the tools' cleverness. It is that one of
them searches the repository and the other searches the directory the repository lives in.

| | Bytes |
|---|---|
| Tracked files (`git ls-files`) | 22.5 MB |
| The actual working directory | 1,234.0 MB |

**54.8× more bytes on disk than in the repository.** None of it is exotic: a dependency
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

| Approach | Function size | Bytes | ~Tokens | vs whole file |
|---|---|---|---|---|
| Native read of the whole file | — | 9,856 | ~2,464 | 1× |
| `read_symbol axisCell` | 6 lines | 333 | ~83 | **29.6× smaller** |
| `read_symbol parseAge` | 16 lines | 737 | ~184 | **13.4× smaller** |
| `read_symbol runStats` | 117 lines | 3,874 | ~968 | **2.5× smaller** |

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
| Native read of the whole file | 9,856 | ~2,464 |
| Plumb `file_outline` | 1,558 | ~390 |

**Takeaway — 6.3× smaller.** `file_outline` returns every declaration — signatures with line
ranges, bodies collapsed — for ~390 tokens instead of ~2,464. Enough to navigate the file and
decide what to read in full, without reading it all. Unlike Scenario 3 this ratio is fairly
stable, because it scales with the file's declaration density rather than with your question.

## Scenario 5 — "What actually uses this?"

Question: *what are the real call sites of `FormatSavings`?* This is a different question from
Scenario 1 — not "where does this text appear" but "where is this symbol genuinely referenced".

| Tool | Result | Noise |
|---|---|---|
| `rg -n FormatSavings` | 28 matching lines across 8 files | **16 non-references (57%)** |
| Plumb `find_references` | 14 reference positions on 12 lines across 6 files | **0** |

Note the units differ, and the difference is instructive: a *reference* is a position, not a
line. Two calls on one line are two references but a single `grep` hit. Compared like with like
— distinct file:line pairs — it is 12 real lines against 28 reported ones.

The 16 lines that are **not** references break down as:

- **11** — prose in this very page, which names the symbol constantly.
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
| Plain find-replace (no word boundary) | 28 lines across 8 files |
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

| Approach | Scope |
|---|---|
| `go test ./...` | 55 packages, 652 test files, 4,307 test functions |
| Plumb `topology_affected` | 1,037 tests in **3 packages** |

**Takeaway — a real narrowing, but read the confidence labels.** Two honest caveats matter more
than the ratio:

**It is recall-biased on purpose.** Nearly all 1,037 hits are labelled `co-located, confidence
0.5` — "in the same directory as something affected", not "provably reaches your change". The
tool prefers an extra test to a missed one and says so in its own output. The 4× reduction in
test functions is the weak number; **3 packages out of 55** is the one you act on, because you
narrow a `go test` run by package path.

**Ask for the whole answer.** `max_results` defaults to **50** against an answer of 1,037. The
response does say `[truncated: max_results reached]` on its last line — but it is one line under
a wall of results that reads as complete, and because every co-located hit carries the same 0.5
confidence the cut falls in path order rather than by relevance. At the default it dropped
`TestFormatSavings`, the one test that directly exercises the changed function. Raise
`max_results` past the expected answer, and check that marker rather than the list's plausibility.

Note also that the full 1,037-entry response is 119 KB (~30k tokens). Use it to choose packages,
not to read.

## Scenario 8 — Reading several files at once

Question: *I need these three files.* Published because it is a **loss**.

| Approach | Bytes | vs native | Turns |
|---|---|---|---|
| Three native reads (raw file bytes) | 1,739 | 1× | 3 |
| Three Plumb `read_file` calls | 2,469 | 1.42× | 3 |
| One Plumb `read_multiple_files` | 3,160 | 1.82× | 1 |

**Takeaway — 1.82× *more* payload, for one round trip instead of three.** The middle row is the
honest part, and it is worth splitting out: most of the overhead is not batching at all. Plumb's
reads carry a line-number gutter and a provenance header (mtime, sha256, line and byte counts),
which costs **1.42×** the raw bytes whether you batch or not. Batching adds a further 691 bytes
across three files — a separator rule and a `### path (N bytes)` title each — for **1.28×** the
three individual calls.

So there are two separate costs here, and only the smaller one is about this tool. There is no
token argument for `read_multiple_files` either way.

What it does buy: one agent turn instead of three (latency, and fewer chances to be interrupted
mid-sequence), and inline per-file errors, so one unreadable path doesn't abort the batch. Those
are real, and they are not measured in bytes. If your agent budget is tokens rather than turns,
read the files one at a time — and this is exactly why the tool is a candidate for hiding in a
leaner tool profile.

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

| Language | File | Whole file | Symbol read | `file_outline` |
|---|---|---|---|---|
| Go | `internal/cli/stats.go` (9,856 B) | ~2,464 tok | `parseAge`, 16 lines — 737 B, **13.4×** | 1,558 B — **6.3×** |
| Python | `scripts/build-blog.py` (16,930 B) | ~4,232 tok | `load_posts`, 19 lines — 1,255 B, **13.5×** | 2,426 B — **7.0×** |
| JavaScript | `internal/web/ui/src/lib/charts.js` (11,371 B) | ~2,843 tok | `activityCalendar`, 28 lines — 1,258 B, **9.0×** | 1,721 B — **6.6×** |

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
| Read one function | `read_symbol` | 2.5×–29.6× fewer tokens, depending on the symbol |
| Understand a file | `file_outline` | ~6.3× fewer tokens (7.0× Python, 6.6× JS) |
| Find text | `search_in_files` | a wash vs `ripgrep`; 9.9× smaller than naive `grep` |
| Find references | `find_references` | exact vs 57% noise — a correctness win |
| Rename a symbol | `rename_symbol` | 15 scoped edits vs 25–28 blind ones — a safety win |
| Pick tests to run | `topology_affected` | 3 packages instead of 55 — recall-biased, check truncation |
| Read several files | `read_multiple_files` | **1.82× more** tokens; buys turns and atomicity |
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
