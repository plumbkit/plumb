# semsearch — topology semantic-search spike

Throwaway eval harness for the tree-sitter plan's **Phase 7** (hybrid semantic
search on `topology_search`). It answers the "spike first" gate: *does a semantic
re-rank beat the FTS5-only baseline enough to justify building the feature?*

Its own Go module (stdlib only) so it stays out of plumb's `go build ./...`,
`golangci-lint`, and the file-size guard. It shells out to the `sqlite3` CLI for
the corpus + FTS5 baseline and calls the OpenAI embeddings API over `net/http`.

## What it does

1. Loads plumb's own code symbols (function/method/type) from `.plumb/topology.db`.
2. Embeds each symbol (name + signature + docstring) and a fixed set of
   hand-labelled natural-language queries with `text-embedding-3-small`
   (cached to `embeddings-cache.json`, so re-runs are free).
3. Scores four ranking arms on recall@10 / MRR@10:
   - `fts` — the current FTS5 bm25 baseline;
   - `semantic` — pure cosine over embeddings;
   - `rerank` — FTS5 spine, re-ranked by cosine (the plan's design);
   - `rrf` — reciprocal-rank fusion of fts + semantic.
4. Prints a markdown report and a build / don't-build verdict.

## Run

```sh
export OPENAI_API_KEY=sk-...
go run . -db ../../.plumb/topology.db -out report.md
```

Cost: embedding ~3k short symbol docs with `text-embedding-3-small` is well under
US$0.01; the cache makes subsequent runs free.

## Result

See [`docs/internal/semantic-search-spike.md`](../../docs/internal/semantic-search-spike.md)
for the recorded findings and recommendation.
