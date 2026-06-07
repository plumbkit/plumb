package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"
)

const embedModel = "text-embedding-3-small"

// embedder calls the OpenAI embeddings API, caching results to disk by content
// hash so re-runs cost nothing and only changed corpus entries are re-embedded.
type embedder struct {
	key       string
	cachePath string
	cache     map[string][]float32
	newCount  int
	hitCount  int
}

func newEmbedder(key, cachePath string) *embedder {
	e := &embedder{key: key, cachePath: cachePath, cache: map[string][]float32{}}
	if data, err := os.ReadFile(cachePath); err == nil {
		_ = json.Unmarshal(data, &e.cache)
	}
	return e
}

func cacheKey(text string) string {
	sum := sha256.Sum256([]byte(embedModel + "\x00" + text))
	return hex.EncodeToString(sum[:8])
}

// embedAll returns one vector per input text, in order. Cached texts are served
// from disk; the rest are embedded in batches and the cache is persisted.
func (e *embedder) embedAll(texts []string) ([][]float32, error) {
	var missing []string
	seen := map[string]bool{}
	for _, t := range texts {
		k := cacheKey(t)
		if _, ok := e.cache[k]; ok {
			continue
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		missing = append(missing, t)
	}
	const batch = 100
	for i := 0; i < len(missing); i += batch {
		end := min(i+batch, len(missing))
		vecs, err := e.call(missing[i:end])
		if err != nil {
			return nil, err
		}
		for j, t := range missing[i:end] {
			e.cache[cacheKey(t)] = vecs[j]
		}
		e.newCount += end - i
		e.persist()
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := e.cache[cacheKey(t)]
		out[i] = v
		if !seen[cacheKey(t)] {
			e.hitCount++
		}
	}
	return out, nil
}

func (e *embedder) persist() {
	if data, err := json.Marshal(e.cache); err == nil {
		_ = os.WriteFile(e.cachePath, data, 0o644)
	}
}

type embedReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResp struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (e *embedder) call(input []string) ([][]float32, error) {
	body, _ := json.Marshal(embedReq{Model: embedModel, Input: input})
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/embeddings", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+e.key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("openai status %d: %s", resp.StatusCode, raw)
			continue
		}
		var er embedResp
		if err := json.Unmarshal(raw, &er); err != nil {
			return nil, fmt.Errorf("decode embeddings: %w (%s)", err, raw)
		}
		if er.Error != nil {
			return nil, fmt.Errorf("openai: %s", er.Error.Message)
		}
		out := make([][]float32, len(input))
		for _, d := range er.Data {
			if d.Index >= 0 && d.Index < len(out) {
				out[d.Index] = d.Embedding
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("openai embeddings failed after retries: %w", lastErr)
}

// cosine returns the cosine similarity of two vectors (both are L2-normalised by
// OpenAI, but we normalise defensively so the metric is well-defined).
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(b) == 0 || len(a) != len(b) {
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
