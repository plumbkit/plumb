package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/plumbkit/plumb/internal/config"
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

	got := matchingMemoryNames(mems, "internal/auth/login.go", nil, 3)
	if len(got) != 1 || got[0] != "auth-gotchas" {
		t.Errorf("expected [auth-gotchas], got %v", got)
	}
	if n := matchingMemoryNames(mems, "internal/db/store.go", nil, 3); len(n) != 0 {
		t.Errorf("non-matching path should yield no hints, got %v", n)
	}
}

func TestMatchingMemoryNames_RespectsMax(t *testing.T) {
	mems := []memory.Memory{
		{Name: "a", Paths: []string{"**"}},
		{Name: "b", Paths: []string{"**"}},
		{Name: "c", Paths: []string{"**"}},
	}
	if got := matchingMemoryNames(mems, "x.go", nil, 2); len(got) != 2 {
		t.Errorf("max=2 should cap to 2, got %v", got)
	}
}

// TestMatchingMemoryNames_UserAuthoredClaimSlotsFirst: generated episodic-*
// memories attach to the same hot files as hand-written notes, and List returns
// them in name order ("episodic-…" sorts early) — so without an explicit
// preference they would fill every capped hint slot. A user memory sorting LAST
// by name must still claim a slot ahead of every generated one.
func TestMatchingMemoryNames_UserAuthoredClaimSlotsFirst(t *testing.T) {
	mems := []memory.Memory{
		{Name: "episodic-20260610-aaaa", Paths: []string{"**"}, Confidence: memory.ConfidenceGenerated},
		{Name: "episodic-20260610-bbbb", Paths: []string{"**"}, Confidence: memory.ConfidenceGenerated},
		{Name: "episodic-20260610-cccc", Paths: []string{"**"}, Confidence: memory.ConfidenceGenerated},
		{Name: "zz-user-notes", Paths: []string{"**"}},
	}
	got := matchingMemoryNames(mems, "x.go", nil, 3)
	if len(got) != 3 || got[0] != "zz-user-notes" {
		t.Errorf("user-authored memory must claim the first hint slot, got %v", got)
	}
}

// TestMatchingMemoryNames_SymbolRefs: a memory whose provenance references a
// symbol present in the edited file matches even when no paths glob does; a
// nil symbol set skips the symbol pass entirely.
func TestMatchingMemoryNames_SymbolRefs(t *testing.T) {
	mems := []memory.Memory{
		{Name: "lock-design", SourceSymbols: []string{"AcquireDaemonLock"}},
		{Name: "other", SourceSymbols: []string{"Unrelated"}},
	}
	got := matchingMemoryNames(mems, "internal/cli/lock.go", map[string]bool{"AcquireDaemonLock": true}, 3)
	if len(got) != 1 || got[0] != "lock-design" {
		t.Errorf("symbol ref should match, got %v", got)
	}
	if got := matchingMemoryNames(mems, "internal/cli/lock.go", nil, 3); len(got) != 0 {
		t.Errorf("nil symbol set must skip the symbol pass, got %v", got)
	}
}

// TestEnrichToolOutput_HintOnceQuiet: a memory hints on the first matching
// read, stays quiet for the rest of the session, and hints again after a
// re-pin clears the suppression set.
func TestEnrichToolOutput_HintOnceQuiet(t *testing.T) {
	ws := t.TempDir()
	writePathMemory(t, ws, "auth-gotchas", "internal/auth/**")

	s := &connSession{store: config.NewStore(config.Defaults()), hintCache: &memoryHintCache{}}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)

	args := json.RawMessage(`{"file_path":"` + ws + `/internal/auth/login.go"}`)
	ctx := context.Background()

	if out := s.enrichToolOutput(ctx, "read_file", args, "body"); !strings.Contains(out, "[Hint:") {
		t.Fatalf("first matching read should hint: %q", out)
	}
	if out := s.enrichToolOutput(ctx, "read_file", args, "body"); strings.Contains(out, "[Hint:") {
		t.Fatalf("repeat hint must stay quiet for the session: %q", out)
	}
	s.clearHintSeen() // what a re-pin does
	if out := s.enrichToolOutput(ctx, "read_file", args, "body"); !strings.Contains(out, "[Hint:") {
		t.Fatalf("hint should fire again after re-pin: %q", out)
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
