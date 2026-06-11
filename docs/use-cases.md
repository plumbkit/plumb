# Use cases: measured tool comparisons

This page records a few honest, reproducible measurements of Plumb's tools against the
standard tools a coding agent already has — `grep`/`ripgrep` and whole-file reads. The aim is
not to claim Plumb wins everywhere. It doesn't, and the numbers below say so plainly. The aim
is to show *where* a structured, language-aware tool layer helps and where it is simply
neutral, so you can reason about when it earns its place.

Two honest results up front:

- **Targeted reads are a real token-efficiency win** — reading one function instead of a whole
  file is ~8× less to put in the context window here.
- **Raw text search is a wash** — Plumb returns the same matches as `ripgrep` at a comparable
  size. Its value there is signal and safety, not fewer tokens.

## How this was measured

| | |
|---|---|
| Repository | this repo, commit `a0885ea` |
| Date | 2026-06-11 |
| Plumb daemon | 0.9.17 |
| Client | Claude Code (CLI, terminal) |
| Model | Claude Opus 4.8 (1M context) |
| Platform | macOS |

- Token figures use ~4 characters per token (the same rough average Plumb uses internally).
- Native numbers are exact and reproducible with the command shown (`wc -c`, `rg`, `grep`).
- Plumb numbers are the measured size of the tool's response (the text that lands in the
  context window).
- These are **measured payload bytes** — distinct from the *estimated* token-efficiency figure
  shown inside Plumb's own stats, which is a heuristic, not a measurement.

The sample symbol is `FormatSavings`, and the sample file is
`internal/stats/savings.go` (195 lines, 6,075 bytes).

## Scenario 1 — Searching the project for a symbol

Question: *find every occurrence of `FormatSavings`.*

| Tool | Matches | Bytes | ~Tokens | Walks the 60 MB binary + 8.9 MB `.git`? | Says which function each hit is in? |
|---|---|---|---|---|---|
| `grep -rn FormatSavings .` | 16 | 2,878 | ~720 | yes (~69 MB) | no |
| `rg -n FormatSavings` | 16 | 2,878 | ~720 | no (respects `.gitignore`) | no |
| Plumb `search_in_files` | 16 | comparable | ~720 | no | **yes** (annotates `[in: writeSessionStats]`, …) |

**Takeaway — a token wash, and that's fine.** `ripgrep` and `search_in_files` return the same
16 matches at the same order of size. Hiding Plumb's search to "save tokens" would save nothing.
What `search_in_files` adds instead is: the enclosing-symbol annotation that a text search can't
produce, a guarantee it never dumps the compiled binary or `.git` into your results, and not
having to walk ~69 MB to find out. Naive `grep -rn .` descends into both the 60 MB build
artefact and the 8.9 MB `.git` directory; `ripgrep` and Plumb both prune them.

## Scenario 2 — Reading one function

Question: *I need the `TokensSavedForClient` function.*

| Approach | Bytes | ~Tokens |
|---|---|---|
| Native read of the whole file | 6,075 | ~1,519 |
| Plumb `read_symbol` (just the function) | 709 | ~177 |

**Takeaway — ~8.5× smaller.** The function is 15 lines. Reading the whole 195-line file to get
it pulls ~1,519 tokens into the context window; `read_symbol` returns the function plus a small
header for ~177. This is where the token-efficiency story actually lives: addressing code by
symbol instead of by file. When you only know the rough area, `read_file` with a line range is
the same idea.

## Scenario 3 — Understanding a file's shape

Question: *what's in `savings.go`?*

| Approach | Bytes | ~Tokens |
|---|---|---|
| Native read of the whole file | 6,075 | ~1,519 |
| Plumb `file_outline` | 1,550 | ~387 |

**Takeaway — ~3.9× smaller.** `file_outline` returns all 14 declarations — every constant,
variable and function signature with its line range, bodies collapsed — for ~387 tokens instead
of ~1,519. Enough to navigate the file and decide what to read in full, without reading it all.

## Scenario 4 — "What actually uses this?"

Question: *what are the real call sites of `FormatSavings`?* This is a different question from
Scenario 1 — not "where does this text appear" but "where is this symbol genuinely referenced".

| Tool | Result | Noise |
|---|---|---|
| `rg FormatSavings` | 16 matches across 7 files | **7 non-references (~44%)** |
| Plumb `find_references` | 9 call sites across 4 files | **0** |

The 7 text matches that are **not** references: five lines of prose across two internal
markdown docs, plus the function's own doc-comment and its definition line. `find_references`
asks the language server for the actual references to that specific declaration, so it returns
the 9 true call sites and nothing else.

**Takeaway — precision, not size.** For the semantic question — references, "is it safe to
change this signature?", rename impact — a text search over-matches (comments, strings, prose,
same-named symbols) and can also miss a real reference its pattern didn't anticipate. The
language-server answer is exact. This is the rebuttal to Scenario 1: text search is a wash, but
*semantic* search is a correctness win.

*Caveat:* `find_references` needs a warm language server. On a cold start, or for a language
with no server configured, Plumb falls back to its tree-sitter index and labels the result
approximate.

## What the numbers say

| Question | Plumb tool | Result |
|---|---|---|
| Read one function | `read_symbol` | ~8.5× fewer tokens |
| Understand a file | `file_outline` | ~3.9× fewer tokens |
| Find text | `search_in_files` | token wash; adds symbol context + scoping |
| Find references | `find_references` | exact vs ~44% noise — a correctness win |

The token-efficiency win is concentrated in **targeted reads**. Raw text search is **neutral**
on tokens and better on signal. **Semantic** navigation is a **correctness** win rather than a
size one. That spread — wins, washes, and a precision story — is why Plumb keeps its read and
search tools first-class rather than treating them as redundant with an agent's native ones:
"the agent already has a search tool" is not the same as "the agent's search answers this
question as well".

## Reproduce it yourself

```sh
# Scenario 1 — native search (exact bytes)
rg -n FormatSavings | wc -lc
grep -rn FormatSavings . | wc -lc

# Scenario 2/3 — file and function size
wc -lc internal/stats/savings.go
sed -n '125,139p' internal/stats/savings.go | wc -lc

# Scenario 4 — grep matches vs real references
grep -rn FormatSavings . | sed 's|:.*||' | sort | uniq -c
```

The Plumb-side figures come from calling `search_in_files`, `read_symbol`, `file_outline` and
`find_references` through any MCP client and measuring the response. Re-run on a later commit
and the absolute numbers will drift; the shape of the result should not.
