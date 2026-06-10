package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/memory"
)

func TestHintRelPath(t *testing.T) {
	ws := "/ws"
	cases := map[string]string{
		`{"file_path":"/ws/internal/auth/login.go"}`: "internal/auth/login.go",
		`{"path":"internal/auth/login.go"}`:          "internal/auth/login.go",
		`{"file_path":"/other/x.go"}`:                "", // outside workspace
		`{}`:                                         "", // no path arg
		// An in-workspace dir literally named "..config" must still hint — a bare
		// ".." prefix check would wrongly reject it as an escape.
		`{"file_path":"/ws/..config/app.go"}`: "..config/app.go",
		`{"path":"../escape.go"}`:             "", // genuine escape
	}
	for in, want := range cases {
		if got := hintRelPath(ws, json.RawMessage(in)); got != want {
			t.Errorf("hintRelPath(%s) = %q, want %q", in, got, want)
		}
	}
}

func writePathMemory(t *testing.T, ws, name, paths string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: d\npaths: " + paths + "\n---\n\nbody"
	if err := memory.Write(ws, name, content, ""); err != nil {
		t.Fatalf("Write %q: %v", name, err)
	}
}

func TestMemoryHintCache_Match(t *testing.T) {
	ws := t.TempDir()
	writePathMemory(t, ws, "auth-gotchas", "internal/auth/**")
	writePathMemory(t, ws, "cmd-notes", "cmd/**")

	cache := &memoryHintCache{}
	mems := cache.memories(ws)
	if len(mems) != 2 {
		t.Fatalf("expected 2 memories cached, got %d", len(mems))
	}

	got := matchingMemoryNames(mems, "internal/auth/login.go", 3)
	if len(got) != 1 || got[0] != "auth-gotchas" {
		t.Errorf("expected [auth-gotchas], got %v", got)
	}
	if n := matchingMemoryNames(mems, "internal/db/store.go", 3); len(n) != 0 {
		t.Errorf("non-matching path should yield no hints, got %v", n)
	}
}

func TestMatchingMemoryNames_RespectsMax(t *testing.T) {
	mems := []memory.Memory{
		{Name: "a", Paths: []string{"**"}},
		{Name: "b", Paths: []string{"**"}},
		{Name: "c", Paths: []string{"**"}},
	}
	if got := matchingMemoryNames(mems, "x.go", 2); len(got) != 2 {
		t.Errorf("max=2 should cap to 2, got %v", got)
	}
}

func TestHintBlock(t *testing.T) {
	block := hintBlock([]string{"auth-gotchas"}, 512)
	if !strings.Contains(block, "[Hint:") || !strings.Contains(block, "'auth-gotchas'") {
		t.Errorf("unexpected hint block: %q", block)
	}
	if !strings.Contains(block, "read_memory") {
		t.Errorf("hint should point at read_memory: %q", block)
	}
	// Plural form.
	if b := hintBlock([]string{"a", "b"}, 512); !strings.Contains(b, "memories") {
		t.Errorf("multiple names should read 'memories': %q", b)
	}
}
