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
	"os/exec"
	"time"
)

// embedModel labels the active embedder; it is part of the cache key so the
// OpenAI and local-model caches never collide. Set in main from the -embedder flag.
var embedModel = "text-embedding-3-small"

// embedder produces vectors, caching results to disk by content hash so re-runs
// cost nothing. Two backends: the OpenAI embeddings API, or a local model driven
// by a Python subprocess (embed_local.py).
type embedder struct {
	key       string
	cachePath string
	local     bool
	pyCmd     []string
	cache     map[string][]float32
	newCount  int
	hitCount  int
}

func newEmbedder(key, cachePath string, local bool, pyCmd []string) *embedder {
	e := &embedder{key: key, cachePath: cachePath, local: local, pyCmd: pyCmd, cache: map[string][]float32{}}
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
// from disk; the rest are embedded and the cache is persisted.
func (e *embedder) embedAll(texts []string) ([][]float32, error) {
	var missing []string
	seen := map[string]bool{}
	for _, t := range texts {
		k := cacheKey(t)
		if _, ok := e.cache[k]; ok || seen[k] {
			continue
		}
		seen[k] = true
		missing = append(missing, t)
	}
	// The local model loads once per subprocess call, so embed everything in one
	// shot; the OpenAI API has per-request limits, so batch it.
	batch := 100
	if e.local {
		batch = max(len(missing), 1)
	}
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
		out[i] = e.cache[cacheKey(t)]
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

func (e *embedder) call(input []string) ([][]float32, error) {
	if e.local {
		return e.localEmbed(input)
	}
	return e.openaiEmbed(input)
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

// localEmbed pipes the texts to embed_local.py and reads back the vectors.
func (e *embedder) localEmbed(input []string) ([][]float32, error) {
	body, _ := json.Marshal(map[string][]string{"input": input})
	cmd := exec.Command(e.pyCmd[0], e.pyCmd[1:]...)
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("local embedder: %w (%s)", err, tailBytes(out, 400))
	}
	var er embedResp
	if err := json.Unmarshal(out, &er); err != nil {
		return nil, fmt.Errorf("local decode: %w", err)
	}
	return orderByIndex(er, len(input)), nil
}

func (e *embedder) openaiEmbed(input []string) ([][]float32, error) {
	reqBody, _ := json.Marshal(map[string]any{"model": embedModel, "input": input})
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
		req, _ := http.NewRequest(http.MethodPost, "https://api.openai.com/v1/embeddings", bytes.NewReader(reqBody))
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
			lastErr = fmt.Errorf("openai status %d", resp.StatusCode)
			continue
		}
		var er embedResp
		if err := json.Unmarshal(raw, &er); err != nil {
			return nil, fmt.Errorf("decode embeddings: %w (%s)", err, raw)
		}
		if er.Error != nil {
			return nil, fmt.Errorf("openai: %s", er.Error.Message)
		}
		return orderByIndex(er, len(input)), nil
	}
	return nil, fmt.Errorf("openai embeddings failed after retries: %w", lastErr)
}

func orderByIndex(er embedResp, n int) [][]float32 {
	out := make([][]float32, n)
	for _, d := range er.Data {
		if d.Index >= 0 && d.Index < n {
			out[d.Index] = d.Embedding
		}
	}
	return out
}

func tailBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// cosine returns the cosine similarity of two vectors.
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
