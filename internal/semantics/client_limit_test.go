package semantics

import (
	"strings"
	"testing"
)

// infReader yields an unbounded stream of 'x' bytes.
type infReader struct{}

func (infReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// TestReadCapped verifies the embeddings response body is bounded: a small body
// passes through, an over-cap (or unbounded) body is reported as overflow so the
// caller fails instead of buffering it all. Regression test for data-2.
func TestReadCapped(t *testing.T) {
	data, over, err := readCapped(strings.NewReader("hello"))
	if err != nil || over || string(data) != "hello" {
		t.Fatalf("under cap: data=%q over=%v err=%v", data, over, err)
	}

	data, over, err = readCapped(infReader{})
	if err != nil {
		t.Fatalf("over cap: unexpected err=%v", err)
	}
	if !over {
		t.Fatal("over cap: expected overflow=true")
	}
	if len(data) != maxResponseBytes {
		t.Fatalf("over cap: len=%d, want %d", len(data), maxResponseBytes)
	}
}
