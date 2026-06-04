package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetProjectValue_CreatesSparseFile verifies a first override creates
// .plumb/config.toml containing only the touched key — never the global
// defaults, which would shadow the global config.
func TestSetProjectValue_CreatesSparseFile(t *testing.T) {
	ws := t.TempDir()
	if err := SetProjectValue(ws, []string{"edits", "rate_limit_per_minute"}, 60); err != nil {
		t.Fatalf("SetProjectValue: %v", err)
	}
	path := filepath.Join(ws, ".plumb", "config.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading project config: %v", err)
	}
	got := string(data)
	if want := "rate_limit_per_minute = 60"; !contains(got, want) {
		t.Errorf("project config = %q, want it to contain %q", got, want)
	}
	// It must NOT carry unrelated default keys (no shadowing).
	for _, leak := range []string{"strict", "show_write_diff", "log_level", "allow_writes"} {
		if contains(got, leak) {
			t.Errorf("project config leaked unrelated key %q: %q", leak, got)
		}
	}

	// Round-trip through LoadProject: the value overrides global, others inherit.
	base := Defaults()
	base.Edits.RateLimitPerMinute = 120
	merged, err := LoadProject(base, ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if merged.Edits.RateLimitPerMinute != 60 {
		t.Errorf("merged rate limit = %d, want 60", merged.Edits.RateLimitPerMinute)
	}
	if merged.Edits.ShowWriteDiff != base.Edits.ShowWriteDiff {
		t.Errorf("merged show_write_diff = %v, want inherited %v", merged.Edits.ShowWriteDiff, base.Edits.ShowWriteDiff)
	}
}

// TestUnsetProjectValue_RemovesEmptyFile verifies that unsetting the only key
// removes the file entirely (back to fully inheriting from global).
func TestUnsetProjectValue_RemovesEmptyFile(t *testing.T) {
	ws := t.TempDir()
	if err := SetProjectValue(ws, []string{"git", "allow_push"}, true); err != nil {
		t.Fatalf("SetProjectValue: %v", err)
	}
	path := filepath.Join(ws, ".plumb", "config.toml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected project config to exist: %v", err)
	}
	if err := UnsetProjectValue(ws, []string{"git", "allow_push"}); err != nil {
		t.Fatalf("UnsetProjectValue: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected project config removed after unsetting the only key, stat err = %v", err)
	}
}

// TestUnsetProjectValue_PrunesTableKeepsSiblings verifies that unsetting one key
// in a multi-key table leaves the siblings, and prunes only fully-empty tables.
func TestUnsetProjectValue_PrunesTableKeepsSiblings(t *testing.T) {
	ws := t.TempDir()
	if err := SetProjectValue(ws, []string{"git", "allow_writes"}, false); err != nil {
		t.Fatal(err)
	}
	if err := SetProjectValue(ws, []string{"git", "allow_push"}, true); err != nil {
		t.Fatal(err)
	}
	if err := UnsetProjectValue(ws, []string{"git", "allow_push"}); err != nil {
		t.Fatal(err)
	}
	raw, err := LoadProjectRaw(ws)
	if err != nil {
		t.Fatal(err)
	}
	git, ok := raw["git"].(map[string]any)
	if !ok {
		t.Fatalf("git table missing after partial unset: %+v", raw)
	}
	if _, ok := git["allow_writes"]; !ok {
		t.Errorf("allow_writes should survive: %+v", git)
	}
	if _, ok := git["allow_push"]; ok {
		t.Errorf("allow_push should be gone: %+v", git)
	}
}

// TestProjectValuePresent reports overridden vs inherited.
func TestProjectValuePresent(t *testing.T) {
	ws := t.TempDir()
	path := []string{"topology", "watch"}
	if present, _ := ProjectValuePresent(ws, path); present {
		t.Error("watch should be inherited (absent) before any set")
	}
	if err := SetProjectValue(ws, path, false); err != nil {
		t.Fatal(err)
	}
	if present, _ := ProjectValuePresent(ws, path); !present {
		t.Error("watch should be present (overridden) after set")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}
