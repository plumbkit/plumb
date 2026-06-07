package tools

import (
	"context"
	"log/slog"

	"github.com/golimpio/plumb/internal/semantics"
	"github.com/golimpio/plumb/internal/topology"
)

// SemanticRerankConfig is the per-call semantic re-rank setting, resolved by the
// daemon from [semantics] config. The zero value (and a nil Embedder) means
// disabled — topology_search returns the pure FTS5 ranking. It lives in the
// tools package (not config) so the package keeps its no-import-config boundary,
// mirroring GitPolicy.
type SemanticRerankConfig struct {
	Enabled    bool
	Candidates int                // FTS5 candidates to re-rank; 0 → 50
	Embedder   semantics.Embedder // nil when unavailable (disabled / no key / bad config)
}

func (c SemanticRerankConfig) active() bool {
	return c.Enabled && c.Embedder != nil
}

func (c SemanticRerankConfig) candidates() int {
	if c.Candidates <= 0 {
		return 50
	}
	return c.Candidates
}

// rerankSearchResults re-orders FTS5 results by embedding similarity to query.
// FTS5 stays the spine: this only re-orders the candidates it surfaced, never
// removes or adds. On ANY failure it returns the input unchanged and false, so
// the caller keeps the FTS5 order.
func rerankSearchResults(ctx context.Context, store *topology.Store, emb semantics.Embedder, query string, results []topology.SearchResult) ([]topology.SearchResult, bool) {
	if emb == nil || store == nil || len(results) == 0 {
		return results, false
	}
	qVec, candVecs, ok := embedCandidates(ctx, store, emb, query, results)
	if !ok {
		return results, false
	}
	order := semantics.Rerank(qVec, candVecs)
	out := make([]topology.SearchResult, len(results))
	for newPos, oldPos := range order {
		out[newPos] = results[oldPos]
	}
	return out, true
}

// embedCandidates returns the query vector and one vector per result. Cached
// vectors come from the topology DB (keyed by content hash); misses are embedded
// in the same call as the query and written back (best-effort). Returns
// ok=false on any embed/store error so the caller keeps the FTS5 order. Only the
// candidates of an actual search are ever embedded — embedding is fully lazy.
func embedCandidates(ctx context.Context, store *topology.Store, emb semantics.Embedder, query string, results []topology.SearchResult) (qVec []float32, candVecs [][]float32, ok bool) {
	model := emb.Model()
	docs := make([]string, len(results))
	hashes := make([]string, len(results))
	for i, r := range results {
		docs[i] = topology.EmbedDoc(r.Node)
		hashes[i] = topology.ContentHash(docs[i])
	}

	cached, err := store.GetEmbeddings(ctx, model, hashes)
	if err != nil {
		slog.Warn("semantics: embedding cache read failed; keeping FTS5 order", "err", err)
		return nil, nil, false
	}

	missDocs, missHash := uncachedCandidates(docs, hashes, cached)
	vecs, err := emb.Embed(ctx, append([]string{query}, missDocs...))
	if err != nil || len(vecs) != 1+len(missDocs) {
		slog.Warn("semantics: embed failed; keeping FTS5 order", "err", err)
		return nil, nil, false
	}
	qVec = vecs[0]
	fresh := make(map[string][]float32, len(missHash))
	for j, h := range missHash {
		v := vecs[1+j]
		cached[h] = v
		fresh[h] = v
	}
	if len(fresh) > 0 {
		if perr := store.PutEmbeddings(ctx, model, fresh); perr != nil {
			slog.Warn("semantics: embedding cache write failed (continuing)", "err", perr)
		}
	}

	candVecs = make([][]float32, len(results))
	for i, h := range hashes {
		candVecs[i] = cached[h]
	}
	return qVec, candVecs, true
}

// uncachedCandidates returns the distinct docs/hashes not present in cached,
// preserving first-seen order.
func uncachedCandidates(docs, hashes []string, cached map[string][]float32) (missDocs, missHash []string) {
	pending := map[string]bool{}
	for i, h := range hashes {
		if _, ok := cached[h]; ok || pending[h] {
			continue
		}
		pending[h] = true
		missDocs = append(missDocs, docs[i])
		missHash = append(missHash, h)
	}
	return missDocs, missHash
}
