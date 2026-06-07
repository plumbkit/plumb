// Throwaway eval harness for the topology semantic-search spike (tree-sitter
// plan, Phase 7). Its own module so it is excluded from plumb's `go build ./...`,
// golangci-lint, and the file-size guard. Stdlib only — it shells out to the
// sqlite3 CLI for the corpus + FTS5 baseline and to the OpenAI embeddings API
// over net/http, so it needs no external Go dependencies.
module semspike

go 1.23
