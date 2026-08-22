package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/plumbkit/plumb/internal/setup"
)

// The uninstall tests pin the safety properties the feature promises: plumb's
// entry goes, siblings survive, everything is backed up first, and a repeat
// (or a never-registered client) is a quiet no-op.

func writeTestConfig(t *testing.T, path string, cfg map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating config dir: %v", err)
	}
	if err := writeJSON(path, cfg); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
}

func assertHasBackup(t *testing.T, pathWithoutExt string) {
	t.Helper()
	pattern := pathWithoutExt + ".*.bak*"
	if matches, err := filepath.Glob(pattern); err != nil || len(matches) == 0 {
		t.Errorf("expected a backup matching %s, got %v (err %v)", pattern, matches, err)
	}
}

func TestRemoveServerEntry_JSONRoundTrip(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, map[string]any{
		"otherTop": "kept",
		"mcpServers": map[string]any{
			"other": map[string]any{"command": "/bin/other"},
			"plumb": map[string]any{"command": "/bin/plumb", "args": []any{"serve"}},
		},
	})

	removed, err := removeMcpServersJSON(cfgPath)
	if err != nil || !removed {
		t.Fatalf("removeServerEntry = (%v, %v), want (true, nil)", removed, err)
	}

	cfg, err := parseJSONConfig(cfgPath)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	servers := cfg["mcpServers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("plumb entry still present after removal")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server was removed with plumb")
	}
	if cfg["otherTop"] != "kept" {
		t.Error("unrelated top-level key was dropped")
	}
	assertHasBackup(t, cfgPath)

	removed, err = removeMcpServersJSON(cfgPath)
	if err != nil || removed {
		t.Errorf("repeat removal = (%v, %v), want (false, nil)", removed, err)
	}
}

func TestRemoveServerEntry_EmptyServersKeyDropped(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, map[string]any{
		"mcpServers": map[string]any{
			"plumb": map[string]any{"command": "/bin/plumb"},
		},
	})

	if removed, err := removeMcpServersJSON(cfgPath); err != nil || !removed {
		t.Fatalf("removeServerEntry = (%v, %v), want (true, nil)", removed, err)
	}
	cfg, err := parseJSONConfig(cfgPath)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	if _, ok := cfg["mcpServers"]; ok {
		t.Error("an mcpServers left empty should be dropped, restoring the pre-plumb shape")
	}
}

func TestRemoveServerEntry_AbsentFileCreatesNothing(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "missing.json")
	removed, err := removeMcpServersJSON(cfgPath)
	if err != nil || removed {
		t.Fatalf("absent config = (%v, %v), want (false, nil)", removed, err)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Error("uninstall must not create a config file")
	}
}

func TestRemoveServerEntry_NonObjectServersIsNoop(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	before := `{"mcpServers": [1, 2]}` + "\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeMcpServersJSON(cfgPath)
	if err != nil || removed {
		t.Fatalf("non-object servers key = (%v, %v), want (false, nil)", removed, err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(after) != before {
		t.Error("a config plumb cannot have registered into must be left byte-identical")
	}
}

func TestRemoveServerEntry_CodexTOML(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cfg := map[string]any{
		"model": "x",
		"mcp_servers": map[string]any{
			"other": map[string]any{"command": "/bin/other"},
			"plumb": map[string]any{"command": "/bin/plumb", "args": []any{"serve"}},
		},
	}
	data, err := toml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if removed, err := removeCodexTOML(cfgPath); err != nil || !removed {
		t.Fatalf("removeCodexTOML = (%v, %v), want (true, nil)", removed, err)
	}
	parsed, err := parseTOMLConfig(cfgPath)
	if err != nil {
		t.Fatalf("re-reading config: %v", err)
	}
	servers := parsed["mcp_servers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("plumb table still present after removal")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server table was removed with plumb")
	}
	assertHasBackup(t, cfgPath)
}

func TestSetupZCodeOut(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	writeTestConfig(t, cfgPath, map[string]any{
		"hooks": map[string]any{"onStart": true},
		"mcp": map[string]any{"servers": map[string]any{
			"plumb": map[string]any{"type": "stdio", "command": "/bin/plumb", "args": []any{"serve"}},
			"other": map[string]any{"command": "/bin/other"},
		}},
	})

	if removed, err := setupZCodeOut(cfgPath); err != nil || !removed {
		t.Fatalf("setupZCodeOut = (%v, %v), want (true, nil)", removed, err)
	}
	cfg, err := parseJSONConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	mcp := cfg["mcp"].(map[string]any)
	servers := mcp["servers"].(map[string]any)
	if _, ok := servers["plumb"]; ok {
		t.Error("nested mcp.servers.plumb still present")
	}
	if _, ok := servers["other"]; !ok {
		t.Error("sibling server was removed")
	}
	if _, ok := cfg["hooks"]; !ok {
		t.Error("ZCode's own hooks section must survive")
	}

	// An mcp.servers holding only plumb drops the servers key but keeps "mcp".
	writeTestConfig(t, cfgPath, map[string]any{
		"mcp": map[string]any{"servers": map[string]any{
			"plumb": map[string]any{"command": "/bin/plumb"},
		}},
	})
	if removed, err := setupZCodeOut(cfgPath); err != nil || !removed {
		t.Fatalf("setupZCodeOut (only plumb) = (%v, %v), want (true, nil)", removed, err)
	}
	cfg, _ = parseJSONConfig(cfgPath)
	mcp = cfg["mcp"].(map[string]any)
	if _, ok := mcp["servers"]; ok {
		t.Error("an mcp.servers left empty should be dropped")
	}
}

func TestSetupDSHOut(t *testing.T) {
	t.Run("fresh registration round-trips to an empty layer", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "cordis.patch.yml")
		if _, _, err := setupDSHInto(cfgPath, "/bin/plumb"); err != nil {
			t.Fatal(err)
		}
		if removed, err := setupDSHOut(cfgPath); err != nil || !removed {
			t.Fatalf("setupDSHOut = (%v, %v), want (true, nil)", removed, err)
		}
		if _, registered, err := dshCommandExtractor(cfgPath); err != nil || registered {
			t.Errorf("extractor after removal = (%v, %v), want registered=false", registered, err)
		}
	})

	t.Run("sibling rows in the insert entry survive", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "cordis.patch.yml")
		src := `- id: keep
  insert:
    - id: mcp-plumb
      name: "@deepseek-ai/dsh-mcp-client"
      config:
        serverName: plumb
        transport: stdio
        command: /bin/plumb
        args: [serve]
    - id: other-plugin
      name: other
`
		if err := os.WriteFile(cfgPath, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if removed, err := setupDSHOut(cfgPath); err != nil || !removed {
			t.Fatalf("setupDSHOut = (%v, %v), want (true, nil)", removed, err)
		}
		data, _ := os.ReadFile(cfgPath)
		got := string(data)
		if _, registered, _ := dshCommandExtractor(cfgPath); registered {
			t.Error("plumb row still registered after removal")
		}
		for _, want := range []string{"other-plugin", "keep"} {
			if !strings.Contains(got, want) {
				t.Errorf("patch lost %q after removal:\n%s", want, got)
			}
		}
		assertHasBackup(t, cfgPath)
	})

	t.Run("absent layer creates nothing", func(t *testing.T) {
		cfgPath := filepath.Join(t.TempDir(), "absent.yml")
		if removed, err := setupDSHOut(cfgPath); err != nil || removed {
			t.Fatalf("setupDSHOut on absent file = (%v, %v), want (false, nil)", removed, err)
		}
		if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
			t.Error("uninstall must not create a patch file")
		}
	})
}

func TestSetupAntigravityOut(t *testing.T) {
	base := t.TempDir()
	standalone := filepath.Join(base, "antigravity-cli", "mcp", "plumb.json")

	// A legacy-era flat config with a plumb entry: Antigravity no longer reads
	// these, so they are not plumb's to manage — uninstall must leave it alone.
	flat := filepath.Join(base, "config", "mcp_config.json")
	writeTestConfig(t, flat, map[string]any{
		"mcpServers": map[string]any{
			"plumb": map[string]any{"command": "/bin/plumb", "args": []any{"serve"}},
		},
	})
	flatBefore, err := os.ReadFile(flat)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := setupAntigravityInto(standalone, "/bin/plumb"); err != nil {
		t.Fatalf("setupAntigravityInto: %v", err)
	}
	if removed, err := setupAntigravityOut(standalone); err != nil || !removed {
		t.Fatalf("setupAntigravityOut = (%v, %v), want (true, nil)", removed, err)
	}
	if _, err := os.Stat(standalone); !os.IsNotExist(err) {
		t.Errorf("standalone should be deleted, stat err = %v", err)
	}
	assertHasBackup(t, standalone[:len(standalone)-len(".json")])

	flatAfter, err := os.ReadFile(flat)
	if err != nil {
		t.Fatal(err)
	}
	if string(flatBefore) != string(flatAfter) {
		t.Error("a legacy-era flat mcp_config.json must be left untouched by uninstall")
	}

	if removed, err := setupAntigravityOut(standalone); err != nil || removed {
		t.Fatalf("repeat setupAntigravityOut = (%v, %v), want (false, nil)", removed, err)
	}
}

func TestRemovePlumbSkills(t *testing.T) {
	skills := embeddedSkills()
	if len(skills) < 2 {
		t.Fatalf("test needs at least two embedded skills, have %d", len(skills))
	}
	dir := t.TempDir()

	ours, rewritten := skills[0], skills[1]
	oursDir := filepath.Join(dir, ours.Name)
	if err := os.MkdirAll(filepath.Join(oursDir, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oursDir, "SKILL.md"), []byte(stampSkillContent(ours.Content)), 0o644); err != nil {
		t.Fatal(err)
	}

	rewrittenDir := filepath.Join(dir, rewritten.Name)
	if err := os.MkdirAll(rewrittenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No provenance marker and different content: a skill the user made their own.
	if err := os.WriteFile(filepath.Join(rewrittenDir, "SKILL.md"), []byte("# my own\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed, kept := removePlumbSkills(dir)
	if len(removed) != 1 || removed[0] != ours.Name {
		t.Errorf("removed = %v, want [%s]", removed, ours.Name)
	}
	if len(kept) != 1 || kept[0] != rewritten.Name {
		t.Errorf("kept = %v, want [%s]", kept, rewritten.Name)
	}
	if _, err := os.Stat(oursDir); !os.IsNotExist(err) {
		t.Error("plumb-owned skill dir should be removed")
	}
	if _, err := os.Stat(rewrittenDir); err != nil {
		t.Error("user-rewritten skill dir must survive")
	}
	// The backup directory sits beside the removed skill, not inside it.
	matches, _ := filepath.Glob(filepath.Join(dir, ours.Name+".*.bak"))
	if len(matches) == 0 {
		t.Error("skill backup directory missing")
	} else if _, err := os.Stat(filepath.Join(matches[0], "SKILL.md")); err != nil {
		t.Errorf("backup exists but holds no SKILL.md: %v", err)
	}
}

// TestUninstallTargetAt_RemovesInstructionsBlock is the PLAN-364 PR-2
// deferred item: `plumb setup <client> --uninstall` must remove the managed
// instruction block it (or a bare `plumb setup <client>`) wrote, not just
// the client's MCP config entry. Drives the same two steps a real `plumb
// setup codex` then `plumb setup codex --uninstall` would: register, apply
// the instructions block, uninstall, and confirm the block is gone while the
// rest of the file the block was appended to would have survived.
func TestUninstallTargetAt_RemovesInstructionsBlock(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	setupGlobalInstructionsFlag = false

	cfgPath, err := codexTarget.pathFn()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := codexTarget.intoFn(cfgPath, "/bin/plumb"); err != nil {
		t.Fatalf("intoFn: %v", err)
	}
	if _, err := applyInstructionsBlock(codexTarget); err != nil {
		t.Fatalf("applyInstructionsBlock: %v", err)
	}

	agentsPath := filepath.Join(dir, "AGENTS.md")
	before, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("AGENTS.md not written by setup: %v", err)
	}
	if !strings.Contains(string(before), "<!-- plumb:managed:start") {
		t.Fatal("AGENTS.md has no managed block to begin with")
	}

	if err := uninstallTargetAt(codexTarget, []string{cfgPath}, true); err != nil {
		t.Fatalf("uninstallTargetAt: %v", err)
	}

	status, err := setup.Check(agentsPath, setup.DefaultTemplate, setup.DefaultVersion)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != setup.StatusMissing {
		t.Errorf("status after --uninstall = %v, want %v (block removed)", status, setup.StatusMissing)
	}
}

// TestUninstallTargetAt_NoRegistrationLeavesInstructionsBlockAlone is the
// inverse: uninstall is a no-op (matching the skills-removal precedent) when
// plumb was not registered in the client's config to begin with, even if an
// instructions block happens to be present — an uninstall must not delete
// content it had no registration to reverse.
func TestUninstallTargetAt_NoRegistrationLeavesInstructionsBlockAlone(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	setupGlobalInstructionsFlag = false

	if _, err := applyInstructionsBlock(codexTarget); err != nil {
		t.Fatalf("applyInstructionsBlock: %v", err)
	}
	agentsPath := filepath.Join(dir, "AGENTS.md")

	cfgPath, err := codexTarget.pathFn()
	if err != nil {
		t.Fatal(err)
	}
	// No intoFn call: plumb was never registered in the config.
	if err := uninstallTargetAt(codexTarget, []string{cfgPath}, true); err != nil {
		t.Fatalf("uninstallTargetAt: %v", err)
	}

	after, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("reading AGENTS.md: %v", err)
	}
	if !strings.Contains(string(after), "<!-- plumb:managed:start") {
		t.Error("instructions block was removed even though plumb was never registered in the config")
	}
}
