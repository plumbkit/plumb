// Package semantics provides the embedding client for opt-in semantic re-rank
// of topology_search. It talks to hosted or user-run HTTP endpoints only — plumb
// never bundles or supervises a model.
//
// Most providers speak the OpenAI wire format, so one client covers OpenAI,
// Voyage, Jina, Mistral, and any self-run OpenAI-compatible server (Ollama,
// llama.cpp, LM Studio, TEI, vLLM). Cohere uses a distinct shape handled by an
// adapter (cohere.go). The package imports nothing from plumb — it takes plain
// parameters resolved by config.SemanticsConfig.Resolve.
package semantics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Embedder turns text into vectors. Concurrency: implementations are safe for
// concurrent use.
type Embedder interface {
	// Embed returns one vector per input text, in order. An empty input yields
	// an empty result with no call.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model returns the embedding model id; it is part of the cache key so
	// switching models never serves stale vectors.
	Model() string
}

// NewEmbedder builds the embedder for a resolved provider. baseURL and model
// must be non-empty (config.Resolve fills provider presets). apiKey may be empty
// for keyless local endpoints.
func NewEmbedder(provider, baseURL, model, apiKey string, timeout time.Duration) (Embedder, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("semantics: base_url is empty (set [semantics].base_url or pick a non-custom provider)")
	}
	if model == "" {
		return nil, fmt.Errorf("semantics: model is empty")
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	hc := &http.Client{Timeout: timeout}
	if provider == "cohere" {
		return &cohere{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), model: model, apiKey: apiKey}, nil
	}
	return &openAICompatible{hc: hc, baseURL: strings.TrimRight(baseURL, "/"), model: model, apiKey: apiKey}, nil
}

// openAICompatible implements the OpenAI POST /embeddings contract.
type openAICompatible struct {
	hc      *http.Client
	baseURL string
	model   string
	apiKey  string
}

func (c *openAICompatible) Model() string { return c.model }

func (c *openAICompatible) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]any{"model": c.model, "input": texts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("semantics: embeddings request: %w", err)
	}
	defer resp.Body.Close()
	raw, over, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("semantics: reading response: %w", err)
	}
	if over {
		return nil, fmt.Errorf("semantics: %s response exceeded %d bytes", c.baseURL, maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("semantics: %s returned %d: %s", c.baseURL, resp.StatusCode, snippet(raw))
	}
	var er struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("semantics: decode embeddings: %w", err)
	}
	out := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index >= 0 && d.Index < len(out) {
			out[d.Index] = d.Embedding
		}
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("semantics: provider returned no vector for input %d", i)
		}
	}
	return out, nil
}

// maxResponseBytes caps an embeddings response body. The http.Client carries a
// timeout but no byte bound, so a misbehaving or (with a project-controlled
// base_url) hostile endpoint could stream an arbitrarily large body within the
// timeout and force the daemon to buffer it all. Real embedding responses are
// far smaller; an over-limit read is an error, so topology_search falls back to
// the FTS5 baseline.
const maxResponseBytes = 16 << 20 // 16 MiB

// readCapped reads up to maxResponseBytes from r, reporting overflow when the
// body would exceed the cap so the caller fails rather than trusting a
// truncated body.
func readCapped(r io.Reader) (data []byte, overflow bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > maxResponseBytes {
		return data[:maxResponseBytes], true, nil
	}
	return data, false, nil
}

func snippet(b []byte) string {
	const n = 300
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// Cosine returns the cosine similarity of two equal-length vectors.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Rerank returns the indices [0,len(candidates)) ordered by descending cosine
// similarity to query. Candidates with a nil/zero vector sort last (score 0).
// The sort is stable, so equal scores preserve the input (FTS5) order — keeping
// FTS5 as the tie-breaking spine.
func Rerank(query []float32, candidates [][]float32) []int {
	idx := make([]int, len(candidates))
	score := make([]float64, len(candidates))
	for i := range candidates {
		idx[i] = i
		score[i] = Cosine(query, candidates[i])
	}
	sort.SliceStable(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
	return idx
}
