package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProjectTOML writes a project config into a fresh workspace and returns
// the workspace root.
func writeProjectTOML(t *testing.T, body string) string {
	t.Helper()
	ws := t.TempDir()
	dir := filepath.Join(ws, ".plumb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestExtraTaskSlots_ProjectDefinedSlotIsRunnable(t *testing.T) {
	ws := writeProjectTOML(t, "[tasks.typescript]\nbuild = \"pnpm build\"\ncheck = \"pnpm check\"\n")
	cfg, err := LoadProject(cloneConfig(defaults), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got := cfg.Tasks["typescript"].Get("check"); got != "pnpm check" {
		t.Errorf(`Get("check") = %q, want "pnpm check"`, got)
	}
	if got := cfg.Tasks["typescript"].Get("build"); got != "pnpm build" {
		t.Errorf(`built-in slots must still resolve; Get("build") = %q`, got)
	}
}

// TestExtraTaskSlots_UpperCaseTableIsCaptured guards the gate-bypass class
// ProjectTaskCommands documents. go-toml binds a table name to a struct field
// case-insensitively, so `[TASKS.typescript]` reaches the runner. An extras
// decoder using an exact raw["tasks"] lookup would miss it — the slot would be
// invisible to validation while staying runnable, which is a config the user
// wrote being executed unchecked.
func TestExtraTaskSlots_UpperCaseTableIsCaptured(t *testing.T) {
	ws := writeProjectTOML(t, "[TASKS.typescript]\ncheck = \"pnpm check\"\n")
	cfg, err := LoadProject(cloneConfig(defaults), ws)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if got := cfg.Tasks["typescript"].Get("check"); got != "pnpm check" {
		t.Fatalf("an upper-case [TASKS.<lang>] table must be captured as an extra; Get(\"check\") = %q", got)
	}
}

// TestExtraTaskSlots_ReachTheTrustHash is the other half of the same guard:
// being captured is worthless if the trust gate cannot see it. An extra must be
// hashed exactly like a built-in, or a cloned repository could ship one and
// have it run with no `plumb trust`.
func TestExtraTaskSlots_ReachTheTrustHash(t *testing.T) {
	for _, table := range []string{"tasks", "TASKS"} {
		ws := writeProjectTOML(t, "["+table+".typescript]\ncheck = \"pnpm check\"\n")
		cmds, err := ProjectTaskCommands(ws)
		if err != nil {
			t.Fatalf("[%s] ProjectTaskCommands: %v", table, err)
		}
		found := false
		for _, c := range cmds {
			if c.Slot == "check" && c.Command == "pnpm check" {
				found = true
			}
		}
		if !found {
			t.Errorf("BYPASS: [%s.typescript] check is not project-supplied, so run_task's trust gate is skipped; got %+v", table, cmds)
		}
	}
}

func TestExtraTaskSlots_ShellMetacharacterRejected(t *testing.T) {
	ws := writeProjectTOML(t, "[tasks.typescript]\ncheck = \"pnpm check && rm -rf /\"\n")
	_, err := LoadProject(cloneConfig(defaults), ws)
	if err == nil {
		t.Fatal("an extra slot carrying a shell metacharacter must be rejected at load")
	}
	if !strings.Contains(err.Error(), "tasks.typescript.check") {
		t.Errorf("the error should name the offending slot, got: %v", err)
	}
}

func TestExtraTaskSlots_MalformedNameRejected(t *testing.T) {
	for _, name := range []string{"Check", "9check", "check!", strings.Repeat("c", 33)} {
		ws := writeProjectTOML(t, "[tasks.typescript]\n\""+name+"\" = \"pnpm run x\"\n")
		if _, err := LoadProject(cloneConfig(defaults), ws); err == nil {
			t.Errorf("slot name %q must be rejected, not silently ignored", name)
		}
	}
}

// A built-in name arriving as an extra would be silently dead — Get answers
// from the struct field — so it is refused rather than shadowing.
func TestExtraTaskSlots_BuiltinNameNotShadowable(t *testing.T) {
	if IsBuiltinTaskSlot("check") {
		t.Fatal("check must not be a built-in")
	}
	err := validateExtraTaskSlots("go", map[string]string{"verify": "go run ./x"})
	if err == nil {
		t.Fatal("an extra named after a built-in must be rejected")
	}
	if !strings.Contains(err.Error(), "built-in") {
		t.Errorf("the error should say why, got: %v", err)
	}
}

// cloneTasks used maps.Copy, which is shallow. With Extra being a map, that
// left clones SHARING it, so a project layer adding a slot would write into the
// global config every later load starts from.
func TestCloneTasks_DeepCopiesExtra(t *testing.T) {
	base := map[string]TasksConfig{"go": {Extra: map[string]string{"check": "orig"}}}
	clone := cloneTasks(base)
	clone["go"].Extra["check"] = "mutated"
	if base["go"].Extra["check"] != "orig" {
		t.Fatal("cloneTasks must deep-copy Extra; the clone wrote into the source config")
	}
}

func TestConfiguredSlotNames_BuiltinsFirstThenSortedExtras(t *testing.T) {
	got := ConfiguredSlotNames(TasksConfig{Extra: map[string]string{"typecheck": "x", "audit": "y"}})
	want := []string{"build", "lint", "test", "e2e", "verify", "audit", "typecheck"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Extras are deliberately NOT agent-writable: agentWritableKeys is keyed by
// registry field and an extra has no registry entry, so the allowlist fails
// closed. Pinned because it is a security property, not an accident.
func TestExtraTaskSlots_NotAgentWritable(t *testing.T) {
	if IsAgentWritable("tasks.typescript.check") {
		t.Error("a project-defined extra slot must not be agent-writable")
	}
	if !IsAgentWritable("tasks.typescript.build") {
		t.Error("built-in slots should remain agent-writable")
	}
}
