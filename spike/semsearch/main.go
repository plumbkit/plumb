// Command semsearch evaluates whether a semantic re-rank beats the FTS5-only
// baseline for topology_search, against a fixed labelled query set over plumb's
// own topology index. It is the Phase-7 "spike first" gate: build the real
// feature only if semantic clearly wins here.
//
// It compares four ranking arms on the same corpus and queries:
//   - fts        : the current FTS5 bm25 baseline (what topology_search does today)
//   - semantic   : pure cosine similarity over OpenAI embeddings
//   - rerank     : FTS5 spine — take FTS5's top-N candidates, re-rank by cosine
//                  (the plan's "FTS5 authoritative, semantic re-ranks" design)
//   - rrf        : reciprocal-rank fusion of the fts and semantic rankings
//
// Metrics: recall@10 and MRR over a hand-labelled relevance set.
//
// Usage:  OPENAI_API_KEY=... go run . [-db <path>] [-k 10]
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// ─── labelled query set ──────────────────────────────────────────────────────
// Each query pairs a natural-language question with the symbol NAMES that a good
// search should surface. The set deliberately mixes queries with token overlap
// (FTS5 should do fine) and vocabulary gaps (throttle≠rate-limit, warm-up≠
// acquire, dangerous≠classify) where only meaning connects query to symbol.

type labelled struct {
	q        string
	relevant []string
}

var queries = []labelled{
	{"abbreviate a filesystem path for display", []string{"ContractPath"}},
	{"throttle how many writes a session can make", []string{"RateLimiter", "NewRateLimiter", "rateLimitError"}},
	{"prevent two agents from corrupting the same file", []string{"lockPath", "safeWrite", "concurrentWriteDetected"}},
	{"how long before an idle connection is closed", []string{"evictIdle", "runIdleReaper"}},
	{"decide if a git command is dangerous", []string{"classifyGit", "gateGit", "gitTier"}},
	{"which tests should i run after editing code", []string{"TopologyAffected", "TestsInDirs"}},
	{"warm up the language server in the background", []string{"acquireLang", "startOrReuse", "awaitReady", "poolOnStart"}},
	{"remove the home directory prefix from a path", []string{"ContractPath"}},
	{"find functions that take a context but never use it", []string{"queryUnusedContext", "StructuralQuery", "ctxParamName"}},
	{"notify the editor that a file changed on disk", []string{"notifyLSP"}},
}

// ─── corpus ──────────────────────────────────────────────────────────────────

type symbol struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Qualified string `json:"qualified"`
	Signature string `json:"signature"`
	Docstring string `json:"docstring"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
}

// doc is the text embedded for a symbol: name, signature, and docstring carry
// the meaning a semantic model can use.
func (s symbol) doc() string {
	parts := []string{s.Name}
	if s.Signature != "" {
		parts = append(parts, s.Signature)
	}
	if s.Docstring != "" {
		parts = append(parts, s.Docstring)
	}
	d := strings.Join(parts, " | ")
	if len(d) > 2000 { // keep well under the per-input token cap
		d = d[:2000]
	}
	return d
}

func main() {
	dbPath := flag.String("db", "../../.plumb/topology.db", "path to plumb's topology.db")
	k := flag.Int("k", 10, "rank cutoff for recall@k / MRR")
	out := flag.String("out", "", "optional path to write the markdown report")
	embedderFlag := flag.String("embedder", "openai", "openai | local")
	pyBin := flag.String("py", "/tmp/semvenv/bin/python", "python interpreter for the local embedder")
	pyScript := flag.String("pyscript", "embed_local.py", "local embedder script")
	flag.Parse()

	local := *embedderFlag == "local"
	cache := "embeddings-cache.json"
	var pyCmd []string
	key := os.Getenv("OPENAI_API_KEY")
	if local {
		embedModel = "bge-small-en-v1.5 (local)"
		cache = "embeddings-cache-local.json"
		pyCmd = []string{*pyBin, *pyScript}
	} else if key == "" {
		fmt.Fprintln(os.Stderr, "OPENAI_API_KEY is required (or pass -embedder local)")
		os.Exit(1)
	}

	corpus, err := loadCorpus(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load corpus: %v\n", err)
		os.Exit(1)
	}
	byID := make(map[int64]symbol, len(corpus))
	for _, s := range corpus {
		byID[s.ID] = s
	}
	fmt.Printf("corpus: %d code symbols (function/method/type) from %s\n", len(corpus), *dbPath)

	// Embed the corpus + queries (cached to disk so re-runs are free).
	emb := newEmbedder(key, cache, local, pyCmd)
	docs := make([]string, len(corpus))
	for i, s := range corpus {
		docs[i] = s.doc()
	}
	t0 := time.Now()
	corpusVecs, err := emb.embedAll(docs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "embed corpus: %v\n", err)
		os.Exit(1)
	}
	qTexts := make([]string, len(queries))
	for i, q := range queries {
		qTexts[i] = q.q
	}
	qVecs, err := emb.embedAll(qTexts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "embed queries: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("embeddings: %d new this run, %d cached (%.1fs)\n\n", emb.newCount, emb.hitCount, time.Since(t0).Seconds())

	report := evaluate(*dbPath, *k, corpus, byID, corpusVecs, qVecs)
	fmt.Print(report)
	if *out != "" {
		if werr := os.WriteFile(*out, []byte(report), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "write report: %v\n", werr)
		} else {
			fmt.Fprintf(os.Stderr, "report written to %s\n", *out)
		}
	}
}

// loadCorpus reads the code symbols (function/method/type) from the topology DB
// via the sqlite3 CLI in JSON mode (robust to tabs/newlines in docstrings).
func loadCorpus(dbPath string) ([]symbol, error) {
	const q = `SELECT n.id, n.name, n.qualified, n.signature, n.docstring, n.kind, f.path
	           FROM topology_nodes n JOIN topology_files f ON f.id = n.file_id
	           WHERE n.kind IN ('function','method','type')`
	out, err := exec.Command("sqlite3", "-json", dbPath, q).Output()
	if err != nil {
		return nil, fmt.Errorf("sqlite3: %w", err)
	}
	var rows []symbol
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return rows, nil
}

// ftsSearch runs plumb's FTS5 baseline (terms OR-matched, bm25 rank) and returns
// node ids best-first.
func ftsSearch(dbPath, query string, limit int) ([]int64, error) {
	terms := strings.Fields(query)
	quoted := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.ReplaceAll(t, `"`, "")
		if t != "" {
			quoted = append(quoted, `"`+t+`"`)
		}
	}
	match := strings.Join(quoted, " OR ")
	sqlStr := fmt.Sprintf(`SELECT fts.rowid AS id FROM topology_fts fts
	         JOIN topology_nodes n ON n.id = fts.rowid
	         WHERE topology_fts MATCH %s AND n.kind IN ('function','method','type')
	         ORDER BY fts.rank LIMIT %d`, sqlLiteral(match), limit)
	out, err := exec.Command("sqlite3", "-json", dbPath, sqlStr).Output()
	if err != nil {
		return nil, fmt.Errorf("fts sqlite3: %w", err)
	}
	var rows []struct {
		ID int64 `json:"id"`
	}
	if len(bytes.TrimSpace(out)) > 0 {
		if err := json.Unmarshal(out, &rows); err != nil {
			return nil, fmt.Errorf("fts decode: %w", err)
		}
	}
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids, nil
}

func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
