package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
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

// writeRawProjectConfig writes body verbatim as the workspace's project
// config, bypassing SetProjectValue — the fold tests need spellings the
// sparse writer would normalise away.
func writeRawProjectConfig(t *testing.T, ws, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ws, ".plumb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".plumb", "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProjectValuePresent_FoldVariantSpelling pins the read half of the #319
// fold: go-toml/v2 binds `[TASKS.go]` into Config.Tasks and `[[COMMAND]]` into
// Config.Commands case-insensitively, so a presence check under the lowercase
// path must see them — the exact-match miss was the mechanism of the run_task
// and run_command trust-gate bypasses.
func TestProjectValuePresent_FoldVariantSpelling(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "[TASKS.go]\ntest = \"go test ./...\"\n\n[[COMMAND]]\nname = \"lint\"\n")

	for _, tc := range []struct {
		path []string
		want bool
		desc string
	}{
		{[]string{"tasks", "go", "test"}, true, "[TASKS.go] leaf under lowercase path"},
		{[]string{"tasks", "go"}, true, "[TASKS.go] table under lowercase path"},
		{[]string{"command"}, true, "[[COMMAND]] array under lowercase path"},
		{[]string{"tasks", "rust", "test"}, false, "absent key stays absent"},
		{[]string{"git"}, false, "absent table stays absent"},
	} {
		got, err := ProjectValuePresent(ws, tc.path)
		if err != nil {
			t.Fatalf("ProjectValuePresent(%v): %v", tc.path, err)
		}
		if got != tc.want {
			t.Errorf("ProjectValuePresent(%v) = %v, want %v (%s)", tc.path, got, tc.want, tc.desc)
		}
	}
}

// TestSetProjectValue_WritesThroughFoldVariant pins the write half: setting
// git.allow_writes in a file that spells the table [GIT] must update the
// EXISTING table, not grow a second one — two fold variants both decode into
// Config.Git, and which one wins is last-wins in the map, not a decision the
// writer is entitled to make for the user.
func TestSetProjectValue_WritesThroughFoldVariant(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "[GIT]\nallow_writes = true\n")

	if err := SetProjectValue(ws, []string{"git", "allow_writes"}, false); err != nil {
		t.Fatalf("SetProjectValue: %v", err)
	}
	raw, err := LoadProjectRaw(ws)
	if err != nil {
		t.Fatalf("LoadProjectRaw: %v", err)
	}
	gitTable, ok := raw["GIT"].(map[string]any)
	if !ok {
		t.Fatalf("raw config = %#v, want the [GIT] table preserved", raw)
	}
	if got := gitTable["allow_writes"]; got != false {
		t.Errorf("allow_writes in [GIT] = %v, want false (written through the fold variant)", got)
	}
	if _, dup := raw["git"]; dup {
		t.Errorf("raw config grew a second, fold-duplicate [git] table: %#v", raw)
	}
}

// TestUnsetProjectValue_RemovesFoldVariant pins the unset half: unsetting the
// lowercase path must remove a fold-variant spelling — leaving it would keep
// reporting present (and keep decoding) after the user asked for inherit.
func TestUnsetProjectValue_RemovesFoldVariant(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "[TASKS.go]\ntest = \"go test ./...\"\n")

	if err := UnsetProjectValue(ws, []string{"tasks"}); err != nil {
		t.Fatalf("UnsetProjectValue: %v", err)
	}
	present, err := ProjectValuePresent(ws, []string{"tasks", "go", "test"})
	if err != nil {
		t.Fatalf("ProjectValuePresent: %v", err)
	}
	if present {
		t.Error("ProjectValuePresent(tasks.go.test) still true after unsetting [TASKS.go]")
	}
	if _, err := os.Stat(filepath.Join(ws, ".plumb", "config.toml")); !os.IsNotExist(err) {
		t.Errorf("project config file survives unsetting its only key: %v", err)
	}
}

// A config file can hold SEVERAL fold variants of one setting — TOML keys are
// case-sensitive, and plumb's own pre-#319 sparse writer produced exactly that
// by growing a second table beside an existing one. go-toml decodes them all
// into a single field, so the helpers must agree on one of them, deterministically.

// TestFoldLookup_PicksOneVariantDeterministically pins the choice against Go's
// randomised map iteration: ranging the map directly returned a different key
// run to run, which made a sparse write land in a different table each time.
func TestFoldLookup_PicksOneVariantDeterministically(t *testing.T) {
	seen := map[string]int{}
	for range 2000 {
		m := map[string]any{
			"GIT": map[string]any{"allow_writes": true},
			"Git": map[string]any{"allow_writes": false},
		}
		key, ok := foldLookup(m, "git")
		if !ok {
			t.Fatal("foldLookup found no variant of \"git\"")
		}
		seen[key]++
	}
	if len(seen) != 1 {
		t.Errorf("foldLookup picked %d different keys across runs (%v), want a single stable choice", len(seen), seen)
	}
}

// TestSetProjectValue_CollapsesDuplicateFoldVariantTables is the load-bearing
// one: two variants both decode into the same field and the LAST in document
// order wins, so writing into one while the other survives stores a value the
// decoder then discards. Asserted by decoding the bytes on disk — the property
// under test is what the DECODER sees, independent of the trust tier above it.
func TestSetProjectValue_CollapsesDuplicateFoldVariantTables(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "[EDITS]\nstrict = true\n\n[edits]\nstrict = true\n")

	if err := SetProjectValue(ws, []string{"edits", "strict"}, false); err != nil {
		t.Fatalf("SetProjectValue: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(ws, ".plumb", "config.toml"))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var got Config
	if err := toml.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding written config: %v", err)
	}
	if got.Edits.Strict {
		t.Errorf("edits.strict decoded as true after writing false — a surviving fold variant overrode the write; file:\n%s", data)
	}

	raw, err := LoadProjectRaw(ws)
	if err != nil {
		t.Fatalf("LoadProjectRaw: %v", err)
	}
	if n := len(foldKeys(raw, "edits")); n != 1 {
		t.Errorf("%d fold variants of [edits] survived the write, want exactly 1; file:\n%s", n, data)
	}
}

// TestUnsetProjectValue_RemovesTheSettingFromEveryFoldVariantTable covers the
// half a leaf-only fold-delete misses: deleteNested must descend through every
// variant of an intermediate segment, or unsetting via `[git]` leaves `[GIT]`
// still holding the key and the setting stays in force.
func TestUnsetProjectValue_RemovesTheSettingFromEveryFoldVariantTable(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "[EDITS]\nstrict = true\n\n[edits]\nstrict = true\n")

	present, err := ProjectValuePresent(ws, []string{"edits", "strict"})
	if err != nil {
		t.Fatalf("ProjectValuePresent: %v", err)
	}
	if !present {
		t.Fatal("edits.strict absent before the unset — fixture is wrong")
	}

	if err := UnsetProjectValue(ws, []string{"edits", "strict"}); err != nil {
		t.Fatalf("UnsetProjectValue: %v", err)
	}

	present, err = ProjectValuePresent(ws, []string{"edits", "strict"})
	if err != nil {
		t.Fatalf("ProjectValuePresent: %v", err)
	}
	if present {
		data, _ := os.ReadFile(filepath.Join(ws, ".plumb", "config.toml"))
		t.Errorf("edits.strict still present after unset — a sibling fold variant survived; file:\n%s", data)
	}
}

// TestFoldLookup_PrefersTheExactSpelling pins the one line that makes the
// common case deterministic. Removing it survived the fold tests that shipped
// with #319 — they only ever build maps with a single variant, so nothing
// noticed which one was preferred.
func TestFoldLookup_PrefersTheExactSpelling(t *testing.T) {
	m := map[string]any{
		"GIT": map[string]any{"a": 1},
		"git": map[string]any{"b": 2},
		"Git": map[string]any{"c": 3},
	}
	key, ok := foldLookup(m, "git")
	if !ok {
		t.Fatal("foldLookup found no variant")
	}
	if key != "git" {
		t.Errorf("foldLookup = %q, want the exact spelling \"git\"", key)
	}
}

// TestSetProjectValue_LeavesAnArrayOfTablesIntact guards the trap folding
// introduced: `[[command]]` decodes to []any, so once the lookup matches case
// it FINDS a `[[COMMAND]]` array where it wants a table. Overwriting it would
// turn a stray extra table into "the project's whole command allow-list was
// deleted by a settings write". No live caller reaches this today — the web
// path validates the key through config.Lookup and the TUI only writes the
// `command` leaf — so this pins the behaviour for the next one.
func TestSetProjectValue_LeavesAnArrayOfTablesIntact(t *testing.T) {
	ws := t.TempDir()
	writeRawProjectConfig(t, ws, "[[COMMAND]]\nname = \"lint\"\nexec = [\"true\"]\n")

	if err := SetProjectValue(ws, []string{"command", "name"}, "clobbered"); err != nil {
		t.Fatalf("SetProjectValue: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(ws, ".plumb", "config.toml"))
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if !contains(string(data), "exec") {
		t.Errorf("the [[COMMAND]] array was destroyed by a sparse write; file:\n%s", data)
	}
}
