package semantics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// cohere adapts Cohere's /v2/embed shape, which differs from the OpenAI
// contract: input is `texts`, `input_type` is required, and vectors come back
// under `embeddings.float`. We use input_type "search_document" for both the
// query and the corpus — self-consistent (so cosine is comparable), at a small
// quality cost vs the asymmetric search_query/search_document split, which is a
// noted v1 simplification.
type cohere struct {
	hc      *http.Client
	baseURL string
	model   string
	apiKey  string
}

func (c *cohere) Model() string { return c.model }

func (c *cohere) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, _ := json.Marshal(map[string]any{
		"model":           c.model,
		"texts":           texts,
		"input_type":      "search_document",
		"embedding_types": []string{"float"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("semantics: cohere request: %w", err)
	}
	defer resp.Body.Close()
	raw, over, err := readCapped(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("semantics: reading cohere response: %w", err)
	}
	if over {
		return nil, fmt.Errorf("semantics: cohere response exceeded %d bytes", maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("semantics: cohere returned %d: %s", resp.StatusCode, snippet(raw))
	}
	var er struct {
		Embeddings struct {
			Float [][]float32 `json:"float"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("semantics: decode cohere embeddings: %w", err)
	}
	if len(er.Embeddings.Float) != len(texts) {
		return nil, fmt.Errorf("semantics: cohere returned %d vectors for %d inputs", len(er.Embeddings.Float), len(texts))
	}
	for i, v := range er.Embeddings.Float {
		if len(v) == 0 {
			return nil, fmt.Errorf("semantics: cohere returned no vector for input %d", i)
		}
	}
	return er.Embeddings.Float, nil
}
