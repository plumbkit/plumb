package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestHistoryDefaults(t *testing.T) {
	h := Defaults().History
	if !h.Enabled || h.MaxContentBytes != 8<<20 || h.MaxDiffBytes != 4<<20 {
		t.Fatalf("defaults = %+v", h)
	}
	if !slices.Contains(h.SensitiveGlobs, ".env") || !slices.Contains(h.SensitiveGlobs, "*.tfvars") || len(h.SensitiveGlobs) != 19 {
		t.Fatalf("sensitive globs = %v", h.SensitiveGlobs)
	}
}

// A project's sensitive_globs ADD to the global list; replacing it would let a
// repository drop .env from its own protection.
func TestProjectHistoryGlobsAddToTheGlobalList(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	toml := "[history]\nsensitive_globs = [\"*.secret\"]\nmax_diff_bytes = 1\n"
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	got := loadProjectForTest(t, ws)
	want := slices.Concat(Defaults().History.SensitiveGlobs, []string{"*.secret"})
	if !slices.Equal(got.History.SensitiveGlobs, want) {
		t.Fatalf("project globs = %v, want the global list plus *.secret", got.History.SensitiveGlobs)
	}
	if got.History.MaxDiffBytes != 4<<20 {
		t.Fatalf("max_diff_bytes is forced-global; project value leaked: %d", got.History.MaxDiffBytes)
	}
}

func loadProjectForTest(t *testing.T, ws string) Config {
	t.Helper()
	got, err := LoadProject(Defaults(), ws)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
