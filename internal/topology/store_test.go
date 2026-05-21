package topology

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golimpio/plumb/internal/config"
)

func TestStore_OpenClose(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".plumb"), 0o700); err != nil {
		t.Fatalf("mkdir .plumb: %v", err)
	}
	s, err := Open(dir, config.TopologyConfig{}, []Extractor{&minimalExtractor{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

func TestStore_OpenEmptyWorkspace(t *testing.T) {
	_, err := Open("", config.TopologyConfig{}, nil)
	if err == nil {
		t.Error("Open with empty workspace should return error")
	}
}

func TestStore_Enqueue_DoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".plumb"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s, err := Open(dir, config.TopologyConfig{}, []Extractor{&minimalExtractor{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Enqueue should return immediately without blocking.
	done := make(chan struct{})
	go func() {
		s.Enqueue("/some/path.go")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Enqueue blocked for >1s")
	}
}

func TestStore_ToRelative(t *testing.T) {
	s := &Store{workspace: "/project"}
	cases := []struct {
		input string
		want  string
	}{
		{"/project/internal/foo.go", "internal/foo.go"},
		{"internal/foo.go", "internal/foo.go"},
		{"/other/path.go", "/other/path.go"},
	}
	for _, c := range cases {
		if got := s.toRelative(c.input); got != c.want {
			t.Errorf("toRelative(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func TestStore_SearchAfterEnqueue(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".plumb"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	goFile := filepath.Join(dir, "indexed.go")
	if err := os.WriteFile(goFile, []byte("package main\n\nfunc EnqueuedSymbol() {}\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := Open(dir, config.TopologyConfig{}, []Extractor{&minimalExtractor{}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Wait for initial resync.
	deadline := time.Now().Add(10 * time.Second)
	var results []SearchResult
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		results, _ = s.Search(context.Background(), "EnqueuedSymbol", SearchOpts{Limit: 5})
		if len(results) > 0 {
			break
		}
	}
	if len(results) == 0 {
		t.Error("Search after Open+resync returned no results for EnqueuedSymbol")
	}
}
