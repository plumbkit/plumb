package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

	got := matchingMemoryNames(mems, "internal/auth/login.go", nil)
	if len(got) != 1 || got[0] != "auth-gotchas" {
		t.Errorf("expected [auth-gotchas], got %v", got)
	}
	if n := matchingMemoryNames(mems, "internal/db/store.go", nil); len(n) != 0 {
		t.Errorf("non-matching path should yield no hints, got %v", n)
	}
}

// TestUnseenHints_CapAfterSuppression: the hint cap applies AFTER the seen
// filter, so an already-hinted memory frees its slot for the next unseen one
// instead of permanently blocking everything ranked below the cap.
func TestUnseenHints_CapAfterSuppression(t *testing.T) {
	s := &connSession{}
	if got := s.unseenHints([]string{"a", "b", "c"}, 2); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("first call should hint the top 2, got %v", got)
	}
	// a and b are now seen; c must claim the freed slot on the next call.
	if got := s.unseenHints([]string{"a", "b", "c"}, 2); len(got) != 1 || got[0] != "c" {
		t.Fatalf("seen memories must free their slots for unseen ones, got %v", got)
	}
	if got := s.unseenHints([]string{"a", "b", "c"}, 2); len(got) != 0 {
		t.Fatalf("all seen → no hints, got %v", got)
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
	got := matchingMemoryNames(mems, "x.go", nil)
	if len(got) != 4 || got[0] != "zz-user-notes" {
		t.Errorf("user-authored memory must come first, got %v", got)
	}
}

// TestMatchingMemoryNames_SymbolRefs: a memory whose provenance references a
// symbol present in the edited file matches even when no paths glob does; a
// nil symbol set skips the symbol pass entirely; a dotted stored reference
// ("Model.renderDashboard" — the form read_symbol/find_symbol args accept)
// still matches the bare node name.
func TestMatchingMemoryNames_SymbolRefs(t *testing.T) {
	mems := []memory.Memory{
		{Name: "lock-design", SourceSymbols: []string{"AcquireDaemonLock"}},
		{Name: "other", SourceSymbols: []string{"Unrelated"}},
	}
	got := matchingMemoryNames(mems, "internal/cli/lock.go", map[string]bool{"AcquireDaemonLock": true})
	if len(got) != 1 || got[0] != "lock-design" {
		t.Errorf("symbol ref should match, got %v", got)
	}
	if got := matchingMemoryNames(mems, "internal/cli/lock.go", nil); len(got) != 0 {
		t.Errorf("nil symbol set must skip the symbol pass, got %v", got)
	}
	dotted := []memory.Memory{{Name: "render-notes", SourceSymbols: []string{"Model.renderDashboard"}}}
	if got := matchingMemoryNames(dotted, "internal/tui/model.go", map[string]bool{"renderDashboard": true}); len(got) != 1 {
		t.Errorf("dotted stored symbol should match the bare node name, got %v", got)
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

// writeGeneratedPathMemory writes a memory whose frontmatter carries
// confidence: generated — the shape an episodic summary or shared finding
// leaves on disk.
func writeGeneratedPathMemory(t *testing.T, ws, name, paths string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: d\npaths: " + paths + "\nconfidence: generated\n---\n\nbody"
	if err := memory.Write(ws, name, content, ""); err != nil {
		t.Fatalf("Write %q: %v", name, err)
	}
}

func TestHintClassFor(t *testing.T) {
	cases := map[string]hintClass{
		"CHANGELOG.md":        hintClassProse,
		"docs/notes.TXT":      hintClassProse,
		"README.rst":          hintClassProse,
		"config/app.json":     hintClassConfig,
		".plumb/config.toml":  hintClassConfig,
		"ci/pipeline.yaml":    hintClassConfig,
		"ci/pipeline.yml":     hintClassConfig,
		"go.lock":             hintClassConfig,
		"internal/auth/x.go":  hintClassSource,
		"scripts/build.sh":    hintClassSource,
		"Makefile":            hintClassSource,
		"cmd/plumb/main_test": hintClassSource,
	}
	for rel, want := range cases {
		if got := hintClassFor(rel); got != want {
			t.Errorf("hintClassFor(%q) = %v, want %v", rel, got, want)
		}
	}
}

// TestHintEligibleMemories: episodic summaries are skipped on read_file but
// kept for mutation tools; a prose target skips them for every tool; a
// config/data target admits only user-authored memories.
func TestHintEligibleMemories(t *testing.T) {
	mems := []memory.Memory{
		{Name: "episodic-20260611-aaaa", Confidence: memory.ConfidenceGenerated},
		{Name: "finding-20260611-bbbb", Confidence: memory.ConfidenceGenerated},
		{Name: "user-notes"},
	}
	cases := []struct {
		tool  string
		class hintClass
		want  []string
	}{
		{"read_file", hintClassSource, []string{"finding-20260611-bbbb", "user-notes"}},
		{"edit_file", hintClassSource, []string{"episodic-20260611-aaaa", "finding-20260611-bbbb", "user-notes"}},
		{"write_file", hintClassSource, []string{"episodic-20260611-aaaa", "finding-20260611-bbbb", "user-notes"}},
		{"edit_file", hintClassProse, []string{"finding-20260611-bbbb", "user-notes"}},
		{"read_file", hintClassProse, []string{"finding-20260611-bbbb", "user-notes"}},
		{"read_file", hintClassConfig, []string{"user-notes"}},
		{"edit_file", hintClassConfig, []string{"user-notes"}},
	}
	for _, c := range cases {
		got := hintEligibleMemories(mems, c.tool, c.class)
		names := make([]string, len(got))
		for i, m := range got {
			names[i] = m.Name
		}
		if strings.Join(names, ",") != strings.Join(c.want, ",") {
			t.Errorf("hintEligibleMemories(%s, %v) = %v, want %v", c.tool, c.class, names, c.want)
		}
	}
}

func TestHintEffectiveBudget(t *testing.T) {
	cases := []struct {
		budget int
		class  hintClass
		want   int
	}{
		{512, hintClassProse, 256},
		{512, hintClassSource, 512},
		{512, hintClassConfig, 512},
		{0, hintClassProse, 0}, // unbounded stays unbounded
		{1, hintClassProse, 1}, // degenerate budget never halves to unbounded
		{-1, hintClassProse, -1},
	}
	for _, c := range cases {
		if got := hintEffectiveBudget(c.budget, c.class); got != c.want {
			t.Errorf("hintEffectiveBudget(%d, %v) = %d, want %d", c.budget, c.class, got, c.want)
		}
	}
}

// TestLabelGeneratedHints: generated names gain a "(generated) " prefix at
// render time; user-authored names are untouched.
func TestLabelGeneratedHints(t *testing.T) {
	mems := []memory.Memory{
		{Name: "episodic-20260611-aaaa", Confidence: memory.ConfidenceGenerated},
		{Name: "conventions"},
	}
	got := labelGeneratedHints([]string{"conventions", "episodic-20260611-aaaa"}, mems)
	if got[0] != "conventions" || got[1] != "(generated) episodic-20260611-aaaa" {
		t.Errorf("unexpected labels: %v", got)
	}
}

// TestMemoryHint_EpisodicSkippedOnRead: an episodic summary path-matched to a
// source file never hints on read_file, but the same memory hints — labelled —
// on edit_file, where prior-session context can matter.
func TestMemoryHint_EpisodicSkippedOnRead(t *testing.T) {
	ws := t.TempDir()
	writeGeneratedPathMemory(t, ws, "episodic-20260611-aaaa", "internal/**")

	s := &connSession{store: config.NewStore(config.Defaults()), hintCache: &memoryHintCache{}}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)

	args := json.RawMessage(`{"file_path":"` + ws + `/internal/auth/login.go"}`)
	ctx := context.Background()

	if out := s.enrichToolOutput(ctx, "read_file", args, "body"); strings.Contains(out, "[Hint:") {
		t.Fatalf("episodic memory must not hint on read_file: %q", out)
	}
	out := s.enrichToolOutput(ctx, "edit_file", args, "body")
	if !strings.Contains(out, "(generated) episodic-20260611-aaaa") {
		t.Fatalf("episodic memory should hint, labelled, on edit_file: %q", out)
	}
}

// TestMemoryHint_ProseSkipsEpisodicAllTools: a prose target (.md) skips
// episodic summaries even for mutation tools.
func TestMemoryHint_ProseSkipsEpisodicAllTools(t *testing.T) {
	ws := t.TempDir()
	writeGeneratedPathMemory(t, ws, "episodic-20260611-aaaa", "**")

	s := &connSession{store: config.NewStore(config.Defaults()), hintCache: &memoryHintCache{}}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)

	args := json.RawMessage(`{"file_path":"` + ws + `/CHANGELOG.md"}`)
	if out := s.enrichToolOutput(context.Background(), "edit_file", args, "body"); strings.Contains(out, "[Hint:") {
		t.Fatalf("episodic memory must not hint on a prose target, even on edit_file: %q", out)
	}
}

// TestMemoryHint_ProseHalvedBudget: the same two memories fit the configured
// budget in full on a source target, but a prose target earns only half of it,
// clipping the block before the second name renders.
func TestMemoryHint_ProseHalvedBudget(t *testing.T) {
	ws := t.TempDir()
	writePathMemory(t, ws, "first-memory-with-a-name", "**")
	writePathMemory(t, ws, "second-memory-with-a-longer-name", "**")
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[memory]\nhint_budget_bytes = 200\n"
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &connSession{store: config.NewStore(config.Defaults()), hintCache: &memoryHintCache{}}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)
	ctx := context.Background()

	prose := json.RawMessage(`{"file_path":"` + ws + `/NOTES.md"}`)
	out := s.enrichToolOutput(ctx, "read_file", prose, "body")
	if !strings.Contains(out, "first-memory-with-a-name") {
		t.Fatalf("halved budget should still fit the first memory: %q", out)
	}
	if strings.Contains(out, "second-memory-with-a-longer-name") {
		t.Fatalf("halved prose budget must clip the second memory: %q", out)
	}

	s.clearHintSeen()
	source := json.RawMessage(`{"file_path":"` + ws + `/main.go"}`)
	out = s.enrichToolOutput(ctx, "read_file", source, "body")
	if !strings.Contains(out, "second-memory-with-a-longer-name") {
		t.Fatalf("full budget on a source target should fit both memories: %q", out)
	}
}

// TestMemoryHint_ConfigTargetUserOnly: a config/data target hints only
// user-authored memories — a generated (non-episodic) finding is skipped.
func TestMemoryHint_ConfigTargetUserOnly(t *testing.T) {
	ws := t.TempDir()
	writeGeneratedPathMemory(t, ws, "finding-20260611-bbbb", "**")
	writePathMemory(t, ws, "toolchain-notes", "**")

	s := &connSession{store: config.NewStore(config.Defaults()), hintCache: &memoryHintCache{}}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)

	args := json.RawMessage(`{"file_path":"` + ws + `/config/app.toml"}`)
	out := s.enrichToolOutput(context.Background(), "read_file", args, "body")
	if !strings.Contains(out, "'toolchain-notes'") {
		t.Fatalf("user-authored memory should hint on a config target: %q", out)
	}
	if strings.Contains(out, "finding-20260611-bbbb") {
		t.Fatalf("generated memory must not hint on a config target: %q", out)
	}
}

// TestMemoryHint_SourceLabelsGenerated: on a source target behaviour is
// unchanged apart from labelling — a generated finding still hints on
// read_file, prefixed "(generated) ", beside an unlabelled user memory.
func TestMemoryHint_SourceLabelsGenerated(t *testing.T) {
	ws := t.TempDir()
	writeGeneratedPathMemory(t, ws, "finding-20260611-bbbb", "**")
	writePathMemory(t, ws, "conventions", "**")

	s := &connSession{store: config.NewStore(config.Defaults()), hintCache: &memoryHintCache{}}
	s.mutate(func(v *sessionView) { v.acquiredRoot = ws })
	s.applyProjectConfig(ws)

	args := json.RawMessage(`{"file_path":"` + ws + `/internal/auth/login.go"}`)
	out := s.enrichToolOutput(context.Background(), "read_file", args, "body")
	if !strings.Contains(out, "'conventions'") || strings.Contains(out, "(generated) conventions") {
		t.Fatalf("user memory must hint unlabelled: %q", out)
	}
	if !strings.Contains(out, "'(generated) finding-20260611-bbbb'") {
		t.Fatalf("generated memory must hint with the (generated) label: %q", out)
	}
}
