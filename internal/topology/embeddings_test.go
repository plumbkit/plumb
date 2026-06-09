package topology

import (
	"context"
	"reflect"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
)

func TestEncodeDecodeVec(t *testing.T) {
	v := []float32{0, 1, -2.5, 3.14159, 1e9}
	if got := decodeVec(encodeVec(v)); !reflect.DeepEqual(got, v) {
		t.Errorf("round-trip = %v, want %v", got, v)
	}
}

func TestEmbeddingsStore_PutGet(t *testing.T) {
	s, err := Open(t.TempDir(), config.TopologyConfig{MaxFileSizeBytes: 512 * 1024}, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	want := map[string][]float32{
		"hash-a": {1, 0, 0},
		"hash-b": {0, 1, 0},
	}
	if err := s.PutEmbeddings(ctx, "model-x", want); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetEmbeddings(ctx, "model-x", []string{"hash-a", "hash-b", "hash-missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got["hash-a"], want["hash-a"]) || !reflect.DeepEqual(got["hash-b"], want["hash-b"]) {
		t.Errorf("get = %v, want a+b only", got)
	}
	// A different model must not see model-x's vectors.
	if other, _ := s.GetEmbeddings(ctx, "model-y", []string{"hash-a"}); len(other) != 0 {
		t.Errorf("model isolation broken: %v", other)
	}
}

func TestEmbedDocAndContentHash(t *testing.T) {
	n := Node{Name: "Foo", Signature: "func Foo()", Docstring: "does foo"}
	doc := EmbedDoc(n)
	if doc != "Foo | func Foo() | does foo" {
		t.Errorf("EmbedDoc = %q", doc)
	}
	if ContentHash(doc) == ContentHash("different") {
		t.Error("distinct content must hash differently")
	}
}
