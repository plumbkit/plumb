package semantics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAICompatible_OrdersByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %s, want /embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth = %q", got)
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Return out of order to prove the client re-orders by index.
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float32{0, 1}},
			{"index": 0, "embedding": []float32{1, 0}},
		}})
	}))
	defer srv.Close()

	emb, err := NewEmbedder("openai", srv.URL, "m", "k", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := emb.Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 2 || vecs[0][0] != 1 || vecs[1][1] != 1 {
		t.Errorf("vectors not ordered by index: %v", vecs)
	}
}

func TestCohere_Shape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embed" {
			t.Errorf("path = %s, want /embed", r.URL.Path)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["input_type"] != "search_document" {
			t.Errorf("missing input_type: %v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": map[string]any{"float": [][]float32{{1, 0}, {0, 1}}},
		})
	}))
	defer srv.Close()

	emb, err := NewEmbedder("cohere", srv.URL, "embed-v4.0", "k", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := emb.Embed(context.Background(), []string{"a", "b"})
	if err != nil || len(vecs) != 2 {
		t.Fatalf("cohere embed: %v %v", vecs, err)
	}
}

func TestEmbed_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	emb, _ := NewEmbedder("openai", srv.URL, "m", "k", time.Second)
	if _, err := emb.Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("non-200 should be an error")
	}
}

func TestNewEmbedder_Validation(t *testing.T) {
	if _, err := NewEmbedder("custom", "", "m", "", time.Second); err == nil {
		t.Error("empty base_url should fail")
	}
	if _, err := NewEmbedder("openai", "http://x/v1", "", "", time.Second); err == nil {
		t.Error("empty model should fail")
	}
}

func TestCosineAndRerank(t *testing.T) {
	if c := Cosine([]float32{1, 0}, []float32{1, 0}); c < 0.999 {
		t.Errorf("identical cosine = %f, want 1", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); c != 0 {
		t.Errorf("orthogonal cosine = %f, want 0", c)
	}
	// query ~ candidate 2; rerank should put index 2 first.
	q := []float32{0, 1}
	cands := [][]float32{{1, 0}, {1, 0}, {0, 1}}
	order := Rerank(q, cands)
	if order[0] != 2 {
		t.Errorf("rerank order = %v, want index 2 first", order)
	}
}
